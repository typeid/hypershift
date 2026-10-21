package controlplaneautoscaling

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	schedulingv1alpha1 "github.com/openshift/hypershift/api/scheduling/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/utils/ptr"
)

func containerPolicy(spec vpaautoscalingv1.VerticalPodAutoscalerSpec, name string) *vpaautoscalingv1.ContainerResourcePolicy {
	for i := range spec.ResourcePolicy.ContainerPolicies {
		if spec.ResourcePolicy.ContainerPolicies[i].ContainerName == name {
			return &spec.ResourcePolicy.ContainerPolicies[i]
		}
	}
	return nil
}

func kasWorkload() resolvedWorkload {
	return resolvedWorkload{
		workloadKey:  kasKey,
		updatePolicy: schedulingv1alpha1.PodUpdatePolicy{UpdateMode: schedulingv1alpha1.UpdateModeInPlaceOrRecreate, MinReplicas: 2},
		updateMode:   vpaautoscalingv1.UpdateModeInPlaceOrRecreate,
		containers: []schedulingv1alpha1.ContainerResourcePolicy{
			{
				ContainerName: "kube-apiserver",
				MinAllowed:    schedulingv1alpha1.ResourceBounds{CPU: "1", Memory: "4Gi"},
				MaxAllowed:    schedulingv1alpha1.ResourceBounds{Memory: "32Gi"},
			},
		},
	}
}

func TestBuildVPASpec(t *testing.T) {
	testCases := []struct {
		name   string
		input  func() vpaSpecInput
		verify func(*WithT, vpaautoscalingv1.VerticalPodAutoscalerSpec, []string)
	}{
		{
			name: "When the workload evicts, it should target the workload with its settings and the default recommender",
			input: func() vpaSpecInput {
				return vpaSpecInput{workload: kasWorkload()}
			},
			verify: func(g *WithT, spec vpaautoscalingv1.VerticalPodAutoscalerSpec, notes []string) {
				g.Expect(spec.TargetRef.APIVersion).To(Equal("apps/v1"))
				g.Expect(spec.TargetRef.Kind).To(Equal("Deployment"))
				g.Expect(spec.TargetRef.Name).To(Equal("kube-apiserver"))
				g.Expect(spec.Recommenders).To(BeEmpty())
				g.Expect(*spec.UpdatePolicy.UpdateMode).To(Equal(vpaautoscalingv1.UpdateModeInPlaceOrRecreate))
				g.Expect(spec.UpdatePolicy.MinReplicas).To(Equal(ptr.To(int32(2))))

				defaultPolicy := containerPolicy(spec, vpaautoscalingv1.DefaultContainerResourcePolicy)
				g.Expect(*defaultPolicy.Mode).To(Equal(vpaautoscalingv1.ContainerScalingModeOff), "containers no profile names are not autoscaled")

				kas := containerPolicy(spec, "kube-apiserver")
				g.Expect(kas.Mode).To(BeNil())
				g.Expect(*kas.ControlledValues).To(Equal(vpaautoscalingv1.ContainerControlledValuesRequestsOnly))
				g.Expect(kas.ControlledResources).To(BeNil(), "both cpu and memory are controlled")
				g.Expect(kas.MinAllowed.Cpu().String()).To(Equal("1"))
				g.Expect(kas.MinAllowed.Memory().String()).To(Equal("4Gi"))
				g.Expect(kas.MaxAllowed.Memory().String()).To(Equal("32Gi"))
				g.Expect(spec.ResourcePolicy.ContainerPolicies).To(HaveLen(2))
				g.Expect(notes).To(BeEmpty())
			},
		},
		{
			name: "When the workload is a StatefulSet, it should target a StatefulSet",
			input: func() vpaSpecInput {
				w := kasWorkload()
				w.workloadKey = etcdKey
				return vpaSpecInput{workload: w}
			},
			verify: func(g *WithT, spec vpaautoscalingv1.VerticalPodAutoscalerSpec, _ []string) {
				g.Expect(spec.TargetRef.Kind).To(Equal("StatefulSet"))
				g.Expect(spec.TargetRef.Name).To(Equal("etcd"))
			},
		},
		{
			name: "When the update mode doesn't evict, it should not set minReplicas",
			input: func() vpaSpecInput {
				w := kasWorkload()
				w.updateMode = vpaautoscalingv1.UpdateModeInitial
				return vpaSpecInput{workload: w}
			},
			verify: func(g *WithT, spec vpaautoscalingv1.VerticalPodAutoscalerSpec, _ []string) {
				g.Expect(*spec.UpdatePolicy.UpdateMode).To(Equal(vpaautoscalingv1.UpdateModeInitial))
				g.Expect(spec.UpdatePolicy.MinReplicas).To(BeNil())
			},
		},
		{
			name: "When a floor is above the configured minimum, it should raise minAllowed",
			input: func() vpaSpecInput {
				return vpaSpecInput{workload: kasWorkload(), floors: map[string]corev1.ResourceList{
					"kube-apiserver": {corev1.ResourceMemory: resource.MustParse("12Gi"), corev1.ResourceCPU: resource.MustParse("500m")},
				}}
			},
			verify: func(g *WithT, spec vpaautoscalingv1.VerticalPodAutoscalerSpec, notes []string) {
				kas := containerPolicy(spec, "kube-apiserver")
				g.Expect(kas.MinAllowed.Memory().String()).To(Equal("12Gi"))
				g.Expect(kas.MinAllowed.Cpu().String()).To(Equal("1"), "the configured minimum is higher than the floor")
				g.Expect(notes).To(BeEmpty())
			},
		},
		{
			name: "When a floor exceeds maxAllowed, it should cap it and report it",
			input: func() vpaSpecInput {
				return vpaSpecInput{workload: kasWorkload(), floors: map[string]corev1.ResourceList{
					"kube-apiserver": {corev1.ResourceMemory: resource.MustParse("64Gi")},
				}}
			},
			verify: func(g *WithT, spec vpaautoscalingv1.VerticalPodAutoscalerSpec, notes []string) {
				g.Expect(containerPolicy(spec, "kube-apiserver").MinAllowed.Memory().String()).To(Equal("32Gi"))
				g.Expect(notes).To(ConsistOf("Floor memory=64Gi on kube-apiserver/kube-apiserver is capped at maxAllowed 32Gi"))
			},
		},
		{
			name: "When a floor names a container no profile names, it should leave the container unmanaged",
			input: func() vpaSpecInput {
				return vpaSpecInput{workload: kasWorkload(), floors: map[string]corev1.ResourceList{
					"konnectivity-server": {corev1.ResourceMemory: resource.MustParse("1Gi")},
				}}
			},
			verify: func(g *WithT, spec vpaautoscalingv1.VerticalPodAutoscalerSpec, _ []string) {
				g.Expect(containerPolicy(spec, "konnectivity-server")).To(BeNil())
			},
		},
		{
			name: "When merged profiles set a minimum above the maximum, it should cap the minimum and report it",
			input: func() vpaSpecInput {
				w := kasWorkload()
				w.containers[0].MinAllowed.Memory = "48Gi"
				return vpaSpecInput{workload: w}
			},
			verify: func(g *WithT, spec vpaautoscalingv1.VerticalPodAutoscalerSpec, notes []string) {
				g.Expect(containerPolicy(spec, "kube-apiserver").MinAllowed.Memory().String()).To(Equal("32Gi"))
				g.Expect(notes).To(ConsistOf("minAllowed memory=48Gi on kube-apiserver/kube-apiserver is capped at maxAllowed 32Gi"))
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			spec, notes := buildVPASpec(tc.input())
			tc.verify(g, spec, notes)
		})
	}
}

func TestVPAName(t *testing.T) {
	testCases := []struct {
		name     string
		workload workloadKey
		verify   func(*WithT, string)
	}{
		{
			name:     "When the workload is a Deployment, it should append the suffix",
			workload: kasKey,
			verify: func(g *WithT, name string) {
				g.Expect(name).To(Equal("kube-apiserver-cp-autoscaling"))
			},
		},
		{
			name:     "When the workload is a StatefulSet, it should append a different suffix",
			workload: etcdKey,
			verify: func(g *WithT, name string) {
				g.Expect(name).To(Equal("etcd-sts-cp-autoscaling"))
			},
		},
		{
			name:     "When the workload name is at the maximum length, it should keep the name valid",
			workload: workloadKey{resource: schedulingv1alpha1.WorkloadResourceStatefulSets, name: strings.Repeat("a", 63)},
			verify: func(g *WithT, name string) {
				g.Expect(len(name)).To(BeNumerically("<=", 253))
				g.Expect(name).To(HaveSuffix("-sts-cp-autoscaling"))
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.verify(NewWithT(t), vpaName(tc.workload))
		})
	}
}

func TestHasRecommendation(t *testing.T) {
	recommendation := &vpaautoscalingv1.RecommendedPodResources{ContainerRecommendations: []vpaautoscalingv1.RecommendedContainerResources{{ContainerName: "etcd"}}}
	testCases := []struct {
		name     string
		status   vpaautoscalingv1.VerticalPodAutoscalerStatus
		expected bool
	}{
		{
			name: "When the recommendation is provided, it should return true",
			status: vpaautoscalingv1.VerticalPodAutoscalerStatus{
				Recommendation: recommendation,
				Conditions:     []vpaautoscalingv1.VerticalPodAutoscalerCondition{{Type: vpaautoscalingv1.RecommendationProvided, Status: corev1.ConditionTrue}},
			},
			expected: true,
		},
		{
			name: "When the RecommendationProvided condition is False, it should return false",
			status: vpaautoscalingv1.VerticalPodAutoscalerStatus{
				Recommendation: recommendation,
				Conditions:     []vpaautoscalingv1.VerticalPodAutoscalerCondition{{Type: vpaautoscalingv1.RecommendationProvided, Status: corev1.ConditionFalse}},
			},
		},
		{
			name: "When there is no recommendation, it should return false",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(hasRecommendation(&vpaautoscalingv1.VerticalPodAutoscaler{Status: tc.status})).To(Equal(tc.expected))
		})
	}
}
