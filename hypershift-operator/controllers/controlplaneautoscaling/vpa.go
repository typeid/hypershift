package controlplaneautoscaling

import (
	"fmt"
	"slices"

	schedulingv1alpha1 "github.com/openshift/hypershift/api/scheduling/v1alpha1"
	"github.com/openshift/hypershift/support/netutil"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/utils/ptr"
)

const (
	// managedLabel marks VerticalPodAutoscalers created by this controller.
	managedLabel = "hypershift.openshift.io/control-plane-vertical-autoscaling-managed"

	// profilesAnnotation records the profiles that set the VerticalPodAutoscaler's workload, in order.
	profilesAnnotation = "hypershift.openshift.io/control-plane-vertical-autoscaling-profiles"

	vpaNameSuffix = "cp-autoscaling"
)

// vpaName returns the name of the VerticalPodAutoscaler for a workload. The suffix keeps it distinct from
// VerticalPodAutoscalers created by other controllers, such as the resource-based control plane autoscaler.
// StatefulSets get their own suffix so that a Deployment and a StatefulSet with the same name don't collide.
func vpaName(key workloadKey) string {
	suffix := vpaNameSuffix
	if key.resource == schedulingv1alpha1.WorkloadResourceStatefulSets {
		suffix = "sts-" + vpaNameSuffix
	}
	return netutil.ShortenName(key.name, suffix, validation.DNS1123SubdomainMaxLength)
}

// vpaSpecInput is everything needed to build a workload's VerticalPodAutoscaler spec.
type vpaSpecInput struct {
	workload resolvedWorkload
	// floors are per-container minimum requests from resource-request-override annotations.
	floors map[string]corev1.ResourceList
}

// buildVPASpec returns the VerticalPodAutoscaler spec for a workload, and notes for the HostedCluster
// condition, such as minimums and floors capped at maxAllowed.
func buildVPASpec(in vpaSpecInput) (vpaautoscalingv1.VerticalPodAutoscalerSpec, []string) {
	w := in.workload
	spec := vpaautoscalingv1.VerticalPodAutoscalerSpec{
		TargetRef: &autoscalingv1.CrossVersionObjectReference{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       w.kind(),
			Name:       w.name,
		},
		UpdatePolicy: &vpaautoscalingv1.PodUpdatePolicy{UpdateMode: ptr.To(w.updateMode)},
	}
	if w.updatePolicy.MinReplicas > 0 && evicts(w.updateMode) {
		spec.UpdatePolicy.MinReplicas = ptr.To(w.updatePolicy.MinReplicas)
	}

	var notes []string
	// Only containers that a profile names are autoscaled; the others, such as sidecars, keep their template
	// requests.
	policies := []vpaautoscalingv1.ContainerResourcePolicy{{
		ContainerName: vpaautoscalingv1.DefaultContainerResourcePolicy,
		Mode:          ptr.To(vpaautoscalingv1.ContainerScalingModeOff),
	}}
	for _, container := range w.containers {
		policy := vpaautoscalingv1.ContainerResourcePolicy{
			ContainerName: container.ContainerName,
			// Limits are left as they are in the pod template.
			ControlledValues: ptr.To(vpaautoscalingv1.ContainerControlledValuesRequestsOnly),
			MinAllowed:       boundsToResourceList(container.MinAllowed),
			MaxAllowed:       boundsToResourceList(container.MaxAllowed),
		}
		notes = append(notes, capMinAllowed(&policy, w.name)...)
		policies = append(policies, policy)
	}

	for _, container := range sortedKeys(in.floors) {
		idx := slices.IndexFunc(policies, func(p vpaautoscalingv1.ContainerResourcePolicy) bool { return p.ContainerName == container })
		if idx < 0 {
			// No profile autoscales this container; the control-plane operator applies the override to its request.
			continue
		}
		policy := &policies[idx]
		for _, resourceName := range sortedKeys(in.floors[container]) {
			floor := in.floors[container][resourceName]
			if current, ok := policy.MinAllowed[resourceName]; ok && current.Cmp(floor) >= 0 {
				continue
			}
			if maxAllowed, ok := policy.MaxAllowed[resourceName]; ok && floor.Cmp(maxAllowed) > 0 {
				notes = append(notes, fmt.Sprintf("Floor %s=%s on %s/%s is capped at maxAllowed %s", resourceName, floor.String(), w.name, container, maxAllowed.String()))
				floor = maxAllowed
			}
			if policy.MinAllowed == nil {
				policy.MinAllowed = corev1.ResourceList{}
			}
			policy.MinAllowed[resourceName] = floor
		}
	}
	spec.ResourcePolicy = &vpaautoscalingv1.PodResourcePolicy{ContainerPolicies: policies}
	return spec, notes
}

// capMinAllowed lowers each minimum above its maximum to the maximum. Profiles merge bounds field by field,
// so a later profile's minimum can exceed an earlier profile's maximum.
func capMinAllowed(policy *vpaautoscalingv1.ContainerResourcePolicy, workload string) []string {
	var notes []string
	for _, resourceName := range sortedKeys(policy.MinAllowed) {
		minAllowed := policy.MinAllowed[resourceName]
		if maxAllowed, ok := policy.MaxAllowed[resourceName]; ok && minAllowed.Cmp(maxAllowed) > 0 {
			notes = append(notes, fmt.Sprintf("minAllowed %s=%s on %s/%s is capped at maxAllowed %s", resourceName, minAllowed.String(), workload, policy.ContainerName, maxAllowed.String()))
			policy.MinAllowed[resourceName] = maxAllowed
		}
	}
	return notes
}

// boundsToResourceList converts validated resource bounds to a resource list, or nil when no bound is set.
func boundsToResourceList(bounds schedulingv1alpha1.ResourceBounds) corev1.ResourceList {
	result := corev1.ResourceList{}
	if bounds.CPU != "" {
		if q, err := resource.ParseQuantity(bounds.CPU); err == nil {
			result[corev1.ResourceCPU] = q
		}
	}
	if bounds.Memory != "" {
		if q, err := resource.ParseQuantity(bounds.Memory); err == nil {
			result[corev1.ResourceMemory] = q
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// noPodsMatched returns true when the recommender reports that no pods match the VerticalPodAutoscaler, for example
// because its workload is scaled to zero.
func noPodsMatched(vpa *vpaautoscalingv1.VerticalPodAutoscaler) bool {
	for _, condition := range vpa.Status.Conditions {
		if condition.Type == vpaautoscalingv1.NoPodsMatched {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// hasRecommendation returns true when the VerticalPodAutoscaler has a recommendation for at least one container.
func hasRecommendation(vpa *vpaautoscalingv1.VerticalPodAutoscaler) bool {
	if vpa.Status.Recommendation == nil || len(vpa.Status.Recommendation.ContainerRecommendations) == 0 {
		return false
	}
	for _, condition := range vpa.Status.Conditions {
		if condition.Type == vpaautoscalingv1.RecommendationProvided {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
