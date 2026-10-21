package controlplaneautoscaling

import (
	"fmt"
	"slices"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	schedulingv1alpha1 "github.com/openshift/hypershift/api/scheduling/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
)

// resolution is the autoscaling configuration that applies to one HostedCluster.
type resolution struct {
	// selected is true when a configuration exists and its hostedClusters field selects the HostedCluster.
	selected bool
	// reason is set when the HostedCluster is selected but vertical autoscaling doesn't apply to it
	// (ClusterSizingActive), and empty otherwise.
	reason  string
	message string

	// profiles are the names of the profiles that select the HostedCluster, in order.
	profiles []string
	// workloads are the autoscaled workloads, sorted by resource and name.
	workloads []resolvedWorkload
	// floors are the per-container minimum requests from resource-request-override annotations,
	// keyed by workload name and then container name.
	floors map[string]map[string]corev1.ResourceList
	// notes are messages for the HostedCluster condition, such as invalid annotations.
	notes []string
}

// workloadKey identifies a workload by resource and name.
type workloadKey struct {
	resource schedulingv1alpha1.WorkloadResource
	name     string
}

func (k workloadKey) String() string {
	return string(k.resource) + "/" + k.name
}

// kind returns the kind of the workload's resource.
func (k workloadKey) kind() string {
	if k.resource == schedulingv1alpha1.WorkloadResourceStatefulSets {
		return "StatefulSet"
	}
	return "Deployment"
}

// resolvedWorkload is the autoscaling configuration of one workload on one HostedCluster, merged from every
// matching profile.
type resolvedWorkload struct {
	workloadKey
	updatePolicy schedulingv1alpha1.PodUpdatePolicy
	// updateMode is the update mode applied, after the default and the single-replica downgrade.
	updateMode vpaautoscalingv1.UpdateMode
	// containers are the merged container policies, sorted by container name.
	containers []schedulingv1alpha1.ContainerResourcePolicy
	// profiles are the names of the profiles that set something for this workload, in order.
	profiles []string
}

// isClusterSizingActive returns true when the HostedCluster is sized by ClusterSizingConfiguration,
// either through a size class label or the resource-based control plane autoscaler.
func isClusterSizingActive(hc *hyperv1.HostedCluster) bool {
	if _, hasSize := hc.Labels[hyperv1.HostedClusterSizeLabel]; hasSize {
		return true
	}
	return hc.Annotations[hyperv1.ResourceBasedControlPlaneAutoscalingAnnotation] == "true"
}

// selectsHostedCluster returns true when the configuration's hostedClusters field selects the HostedCluster,
// regardless of whether the HostedCluster is eligible.
func selectsHostedCluster(config *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration, hc *hyperv1.HostedCluster) (bool, error) {
	if config == nil {
		return false, nil
	}
	return matchesSelection(config.Spec.HostedClusters, hc.Labels)
}

// matchesSelection returns true when the selection selects HostedClusters with these labels.
func matchesSelection(selection schedulingv1alpha1.HostedClusterSelection, hcLabels map[string]string) (bool, error) {
	switch selection.Type {
	case schedulingv1alpha1.HostedClusterSelectionAll:
		return true, nil
	case schedulingv1alpha1.HostedClusterSelectionSelector:
		return matchesSelector(selection.Selector, hcLabels)
	default:
		return false, nil
	}
}

// matchesSelector returns true when the labels match the selector.
func matchesSelector(selector schedulingv1alpha1.HostedClusterSelector, hcLabels map[string]string) (bool, error) {
	s, err := toLabelSelector(selector)
	if err != nil {
		return false, err
	}
	return s.Matches(labels.Set(hcLabels)), nil
}

// toLabelSelector converts a HostedClusterSelector to a labels.Selector.
func toLabelSelector(selector schedulingv1alpha1.HostedClusterSelector) (labels.Selector, error) {
	ls := &metav1.LabelSelector{}
	if len(selector.MatchLabels) > 0 {
		ls.MatchLabels = make(map[string]string, len(selector.MatchLabels))
		for k, v := range selector.MatchLabels {
			ls.MatchLabels[k] = string(v)
		}
	}
	for _, expr := range selector.MatchExpressions {
		values := make([]string, 0, len(expr.Values))
		for _, v := range expr.Values {
			values = append(values, string(v))
		}
		ls.MatchExpressions = append(ls.MatchExpressions, metav1.LabelSelectorRequirement{
			Key:      expr.Key,
			Operator: metav1.LabelSelectorOperator(expr.Operator),
			Values:   values,
		})
	}
	return metav1.LabelSelectorAsSelector(ls)
}

// resolve computes the autoscaling configuration for one HostedCluster: selection, eligibility, the merge of
// matching profiles, per-container floors and the single-replica downgrade.
func resolve(config *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration, hc *hyperv1.HostedCluster) (*resolution, error) {
	selected, err := selectsHostedCluster(config, hc)
	if err != nil {
		return nil, fmt.Errorf("invalid hostedClusters selector: %w", err)
	}
	if !selected {
		return &resolution{}, nil
	}
	if isClusterSizingActive(hc) {
		return &resolution{
			selected: true,
			reason:   hyperv1.ControlPlaneAutoscalingClusterSizingActiveReason,
			message: fmt.Sprintf("The HostedCluster is sized by ClusterSizingConfiguration (label %s or annotation %s); remove both to use vertical autoscaling",
				hyperv1.HostedClusterSizeLabel, hyperv1.ResourceBasedControlPlaneAutoscalingAnnotation),
		}, nil
	}

	res := &resolution{selected: true}
	var matching []schedulingv1alpha1.AutoscalingProfile
	named := map[workloadKey]bool{}
	for _, profile := range config.Spec.Profiles {
		matches, err := matchesSelection(profile.HostedClusters, hc.Labels)
		if err != nil {
			return nil, fmt.Errorf("invalid hostedClusters selector in profile %q: %w", profile.Name, err)
		}
		if !matches {
			continue
		}
		matching = append(matching, profile)
		res.profiles = append(res.profiles, profile.Name)
		for _, entry := range profile.Workloads {
			key := workloadKey{resource: entry.Resource, name: entry.Name}
			named[key] = named[key] || len(entry.ResourcePolicy.ContainerPolicies) > 0
		}
	}

	res.floors, res.notes = parseFloors(hc.Annotations)

	keys := make([]workloadKey, 0, len(named))
	for key := range named {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b workloadKey) int { return strings.Compare(a.String(), b.String()) })

	singleReplica := hc.Spec.ControllerAvailabilityPolicy == hyperv1.SingleReplica
	var withoutContainers, downgraded, defaultedOff []string
	for _, key := range keys {
		if !named[key] {
			// Only named containers are autoscaled, so a VerticalPodAutoscaler without one would do nothing.
			withoutContainers = append(withoutContainers, key.String())
			continue
		}
		w := mergeWorkload(key, matching)
		if w.updatePolicy.UpdateMode == "" {
			w.updateMode = vpaautoscalingv1.UpdateModeOff
			defaultedOff = append(defaultedOff, key.String())
		} else {
			w.updateMode = vpaautoscalingv1.UpdateMode(w.updatePolicy.UpdateMode)
		}
		if singleReplica && evicts(w.updateMode) {
			w.updateMode = vpaautoscalingv1.UpdateModeInitial
			downgraded = append(downgraded, key.String())
		}
		res.workloads = append(res.workloads, w)
	}
	if len(withoutContainers) > 0 {
		res.notes = append(res.notes, fmt.Sprintf("Not autoscaled because no matching profile names a container: %s", strings.Join(withoutContainers, ", ")))
	}
	if len(defaultedOff) > 0 {
		res.notes = append(res.notes, fmt.Sprintf("Update mode Off is used because no matching profile sets updateMode: %s", strings.Join(defaultedOff, ", ")))
	}
	if len(downgraded) > 0 {
		res.notes = append(res.notes, fmt.Sprintf("Update mode Initial is used instead of an evicting mode because the controller availability policy is SingleReplica: %s", strings.Join(downgraded, ", ")))
	}
	return res, nil
}

// mergeWorkload merges the settings of one workload from the matching profiles, in order. Within a profile,
// allWorkloads applies first and the workload's own entry second; every field that is set replaces the
// earlier value.
func mergeWorkload(key workloadKey, profiles []schedulingv1alpha1.AutoscalingProfile) resolvedWorkload {
	w := resolvedWorkload{workloadKey: key}
	containers := map[string]schedulingv1alpha1.ContainerResourcePolicy{}
	for _, profile := range profiles {
		contributed := mergeUpdatePolicy(&w.updatePolicy, profile.AllWorkloads.UpdatePolicy)
		idx := slices.IndexFunc(profile.Workloads, func(c schedulingv1alpha1.WorkloadAutoscaling) bool {
			return c.Resource == key.resource && c.Name == key.name
		})
		if idx >= 0 {
			contributed = true
			entry := profile.Workloads[idx]
			mergeUpdatePolicy(&w.updatePolicy, entry.UpdatePolicy)
			for _, container := range entry.ResourcePolicy.ContainerPolicies {
				merged := containers[container.ContainerName]
				mergeContainer(&merged, container)
				containers[container.ContainerName] = merged
			}
		}
		if contributed {
			w.profiles = append(w.profiles, profile.Name)
		}
	}
	for _, container := range sortedKeys(containers) {
		w.containers = append(w.containers, containers[container])
	}
	return w
}

// mergeUpdatePolicy sets every field of dst that is set in src, and returns true when src sets a field.
func mergeUpdatePolicy(dst *schedulingv1alpha1.PodUpdatePolicy, src schedulingv1alpha1.PodUpdatePolicy) bool {
	if src.UpdateMode != "" {
		dst.UpdateMode = src.UpdateMode
	}
	if src.MinReplicas != 0 {
		dst.MinReplicas = src.MinReplicas
	}
	return src != schedulingv1alpha1.PodUpdatePolicy{}
}

// mergeContainer sets every field of dst that is set in src, merging cpu and memory bounds separately.
func mergeContainer(dst *schedulingv1alpha1.ContainerResourcePolicy, src schedulingv1alpha1.ContainerResourcePolicy) {
	dst.ContainerName = src.ContainerName
	mergeBounds(&dst.MinAllowed, src.MinAllowed)
	mergeBounds(&dst.MaxAllowed, src.MaxAllowed)
}

func mergeBounds(dst *schedulingv1alpha1.ResourceBounds, src schedulingv1alpha1.ResourceBounds) {
	if src.CPU != "" {
		dst.CPU = src.CPU
	}
	if src.Memory != "" {
		dst.Memory = src.Memory
	}
}

// evicts returns true for update modes in which the Vertical Pod Autoscaler updater evicts running pods.
func evicts(mode vpaautoscalingv1.UpdateMode) bool {
	return mode == vpaautoscalingv1.UpdateModeRecreate || mode == vpaautoscalingv1.UpdateModeInPlaceOrRecreate
}

// parseFloors returns the cpu and memory requests set by resource-request-override annotations, keyed by
// workload and container, and notes for annotations and entries that are ignored because they are invalid.
// Like the control-plane operator, which applies the annotation to the pod template, each entry is used on its own:
// an invalid entry doesn't stop the others from applying. Resources other than cpu and memory are not floors; the
// control-plane operator still applies them to the workload template.
func parseFloors(annotations map[string]string) (map[string]map[string]corev1.ResourceList, []string) {
	floors := map[string]map[string]corev1.ResourceList{}
	var invalidKeys, invalidEntries []string
	prefix := hyperv1.ResourceRequestOverrideAnnotationPrefix + "/"
	for _, key := range sortedKeys(annotations) {
		target, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		workload, container, ok := strings.Cut(target, ".")
		if !ok || len(validation.IsDNS1123Label(workload)) > 0 || len(validation.IsDNS1123Label(container)) > 0 {
			invalidKeys = append(invalidKeys, key)
			continue
		}
		requests, invalid := parseResourceRequests(annotations[key])
		for _, entry := range invalid {
			invalidEntries = append(invalidEntries, fmt.Sprintf("%s (%q)", key, entry))
		}
		if len(requests) == 0 {
			continue
		}
		if floors[workload] == nil {
			floors[workload] = map[string]corev1.ResourceList{}
		}
		floors[workload][container] = requests
	}
	var notes []string
	if len(invalidKeys) > 0 {
		notes = append(notes, fmt.Sprintf("Ignored resource request override annotations that don't name a workload and a container: %s", strings.Join(invalidKeys, ", ")))
	}
	if len(invalidEntries) > 0 {
		notes = append(notes, fmt.Sprintf("Ignored invalid entries in resource request override annotations: %s", strings.Join(invalidEntries, ", ")))
	}
	return floors, notes
}

// parseResourceRequests parses a resource-request-override annotation value such as "memory=12Gi,cpu=2"
// and returns its cpu and memory requests, and the entries it ignores. An entry is ignored unless it is a resource
// name and a quantity; a cpu or memory entry is also ignored unless its quantity is greater than zero. When cpu or
// memory is set more than once, the last entry wins, as in the control-plane operator.
func parseResourceRequests(value string) (corev1.ResourceList, []string) {
	result := corev1.ResourceList{}
	var invalid []string
	for _, entry := range strings.Split(value, ",") {
		name, quantity, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			invalid = append(invalid, entry)
			continue
		}
		q, err := resource.ParseQuantity(quantity)
		if err != nil {
			invalid = append(invalid, entry)
			continue
		}
		if name != string(corev1.ResourceCPU) && name != string(corev1.ResourceMemory) {
			continue
		}
		if q.Sign() <= 0 {
			invalid = append(invalid, entry)
			continue
		}
		result[corev1.ResourceName(name)] = q
	}
	return result, invalid
}

type stringSet map[string]struct{}

func (s stringSet) Has(key string) bool {
	_, ok := s[key]
	return ok
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
