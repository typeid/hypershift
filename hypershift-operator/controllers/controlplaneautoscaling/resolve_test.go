package controlplaneautoscaling

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	schedulingv1alpha1 "github.com/openshift/hypershift/api/scheduling/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
)

var (
	kasKey       = workloadKey{resource: schedulingv1alpha1.WorkloadResourceDeployments, name: "kube-apiserver"}
	schedulerKey = workloadKey{resource: schedulingv1alpha1.WorkloadResourceDeployments, name: "kube-scheduler"}
	etcdKey      = workloadKey{resource: schedulingv1alpha1.WorkloadResourceStatefulSets, name: "etcd"}
)

func testConfig() *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
	return &schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfigurationName, Generation: 3},
		Spec: schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfigurationSpec{
			HostedClusters: schedulingv1alpha1.HostedClusterSelection{Type: schedulingv1alpha1.HostedClusterSelectionAll},
			Profiles: []schedulingv1alpha1.AutoscalingProfile{
				{
					Name:           "base",
					HostedClusters: schedulingv1alpha1.HostedClusterSelection{Type: schedulingv1alpha1.HostedClusterSelectionAll},
					AllWorkloads:   allWorkloads(schedulingv1alpha1.PodUpdatePolicy{UpdateMode: schedulingv1alpha1.UpdateModeInPlaceOrRecreate}),
					Workloads: []schedulingv1alpha1.WorkloadAutoscaling{
						workload(kasKey, schedulingv1alpha1.PodUpdatePolicy{MinReplicas: 2},
							schedulingv1alpha1.ContainerResourcePolicy{ContainerName: "kube-apiserver", MaxAllowed: schedulingv1alpha1.ResourceBounds{Memory: "32Gi"}}),
						workload(schedulerKey, schedulingv1alpha1.PodUpdatePolicy{}, schedulingv1alpha1.ContainerResourcePolicy{ContainerName: "kube-scheduler"}),
						workload(etcdKey, schedulingv1alpha1.PodUpdatePolicy{UpdateMode: schedulingv1alpha1.UpdateModeOff}, schedulingv1alpha1.ContainerResourcePolicy{ContainerName: "etcd"}),
					},
				},
				{
					Name:           "canary",
					HostedClusters: selectLabel("autoscaling-canary", "true"),
					Workloads: []schedulingv1alpha1.WorkloadAutoscaling{
						workload(schedulerKey, schedulingv1alpha1.PodUpdatePolicy{UpdateMode: schedulingv1alpha1.UpdateModeOff}),
					},
				},
			},
		},
	}
}

func allWorkloads(updatePolicy schedulingv1alpha1.PodUpdatePolicy) schedulingv1alpha1.AllWorkloadsAutoscaling {
	return schedulingv1alpha1.AllWorkloadsAutoscaling{UpdatePolicy: updatePolicy}
}

func workload(key workloadKey, updatePolicy schedulingv1alpha1.PodUpdatePolicy, containers ...schedulingv1alpha1.ContainerResourcePolicy) schedulingv1alpha1.WorkloadAutoscaling {
	return schedulingv1alpha1.WorkloadAutoscaling{
		Resource:       key.resource,
		Name:           key.name,
		UpdatePolicy:   updatePolicy,
		ResourcePolicy: schedulingv1alpha1.PodResourcePolicy{ContainerPolicies: containers},
	}
}

func selectLabel(key, value string) schedulingv1alpha1.HostedClusterSelection {
	return schedulingv1alpha1.HostedClusterSelection{
		Type:     schedulingv1alpha1.HostedClusterSelectionSelector,
		Selector: schedulingv1alpha1.HostedClusterSelector{MatchLabels: map[string]schedulingv1alpha1.LabelValue{key: schedulingv1alpha1.LabelValue(value)}},
	}
}

func testHostedCluster() *hyperv1.HostedCluster {
	return &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "clusters", Name: "example", Generation: 7},
		Spec: hyperv1.HostedClusterSpec{
			ControllerAvailabilityPolicy: hyperv1.HighlyAvailable,
		},
	}
}

func workloadNames(res *resolution) []string {
	var names []string
	for _, w := range res.workloads {
		names = append(names, w.String())
	}
	return names
}

func workloadByKey(res *resolution, key workloadKey) resolvedWorkload {
	for _, w := range res.workloads {
		if w.workloadKey == key {
			return w
		}
	}
	return resolvedWorkload{}
}

func TestResolve(t *testing.T) {
	testCases := []struct {
		name   string
		config func(*schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration
		hc     func(*hyperv1.HostedCluster)
		verify func(*WithT, *resolution)
	}{
		{
			name: "When no configuration exists, it should not select the HostedCluster",
			config: func(*schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				return nil
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.selected).To(BeFalse())
				g.Expect(res.workloads).To(BeEmpty())
			},
		},
		{
			name: "When hostedClusters is omitted, it should select no HostedCluster",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.HostedClusters = schedulingv1alpha1.HostedClusterSelection{}
				return c
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.selected).To(BeFalse())
			},
		},
		{
			name: "When the selector doesn't match the HostedCluster labels, it should not select it",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.HostedClusters = schedulingv1alpha1.HostedClusterSelection{
					Type: schedulingv1alpha1.HostedClusterSelectionSelector,
					Selector: schedulingv1alpha1.HostedClusterSelector{
						MatchExpressions: []schedulingv1alpha1.LabelSelectorRequirement{{Key: "example.com/autoscaled", Operator: schedulingv1alpha1.LabelSelectorOpExists}},
					},
				}
				return c
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.selected).To(BeFalse())
			},
		},
		{
			name: "When the selector matches the HostedCluster labels, it should resolve every workload of the matching profiles",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.HostedClusters = selectLabel("example.com/autoscaled", "true")
				return c
			},
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Labels = map[string]string{"example.com/autoscaled": "true"}
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.selected).To(BeTrue())
				g.Expect(res.reason).To(BeEmpty())
				g.Expect(res.profiles).To(Equal([]string{"base"}))
				g.Expect(workloadNames(res)).To(Equal([]string{"deployments/kube-apiserver", "deployments/kube-scheduler", "statefulsets/etcd"}))
				kas := workloadByKey(res, kasKey)
				g.Expect(kas.updatePolicy.MinReplicas).To(Equal(int32(2)))
				g.Expect(kas.updateMode).To(Equal(vpaautoscalingv1.UpdateModeInPlaceOrRecreate))
				g.Expect(kas.containers).To(HaveLen(1))
				g.Expect(kas.profiles).To(Equal([]string{"base"}))
				g.Expect(workloadByKey(res, etcdKey).updateMode).To(Equal(vpaautoscalingv1.UpdateModeOff), "a workload entry wins over allWorkloads of the same profile")
			},
		},
		{
			name: "When the HostedCluster has a size class label, it should report ClusterSizingActive",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Labels = map[string]string{hyperv1.HostedClusterSizeLabel: "small"}
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.selected).To(BeTrue())
				g.Expect(res.reason).To(Equal(hyperv1.ControlPlaneAutoscalingClusterSizingActiveReason))
				g.Expect(res.workloads).To(BeEmpty())
			},
		},
		{
			name: "When the HostedCluster uses resource-based control plane autoscaling, it should report ClusterSizingActive",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Annotations = map[string]string{hyperv1.ResourceBasedControlPlaneAutoscalingAnnotation: "true"}
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.reason).To(Equal(hyperv1.ControlPlaneAutoscalingClusterSizingActiveReason))
			},
		},
		{
			name: "When a later profile matches, it should replace only the fields it sets",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Labels = map[string]string{"autoscaling-canary": "true"}
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.profiles).To(Equal([]string{"base", "canary"}))
				scheduler := workloadByKey(res, schedulerKey)
				g.Expect(scheduler.updateMode).To(Equal(vpaautoscalingv1.UpdateModeOff))
				g.Expect(scheduler.containers).To(Equal([]schedulingv1alpha1.ContainerResourcePolicy{{ContainerName: "kube-scheduler"}}))
				g.Expect(scheduler.profiles).To(Equal([]string{"base", "canary"}))
				g.Expect(workloadByKey(res, kasKey).profiles).To(Equal([]string{"base"}))
			},
		},
		{
			name: "When a profile doesn't match, it should not apply it",
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.profiles).To(Equal([]string{"base"}))
				g.Expect(workloadByKey(res, schedulerKey).updateMode).To(Equal(vpaautoscalingv1.UpdateModeInPlaceOrRecreate))
			},
		},
		{
			name: "When a later profile sets allWorkloads, it should apply to every workload and keep the fields it doesn't set",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.Profiles = append(c.Spec.Profiles, schedulingv1alpha1.AutoscalingProfile{
					Name:           "paused",
					HostedClusters: selectLabel("paused", "true"),
					AllWorkloads:   allWorkloads(schedulingv1alpha1.PodUpdatePolicy{UpdateMode: schedulingv1alpha1.UpdateModeInitial}),
				})
				return c
			},
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Labels = map[string]string{"paused": "true"}
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(workloadNames(res)).To(Equal([]string{"deployments/kube-apiserver", "deployments/kube-scheduler", "statefulsets/etcd"}))
				for _, w := range res.workloads {
					g.Expect(w.updateMode).To(Equal(vpaautoscalingv1.UpdateModeInitial), w.name)
					g.Expect(w.profiles).To(Equal([]string{"base", "paused"}), w.name)
				}
				g.Expect(workloadByKey(res, kasKey).updatePolicy.MinReplicas).To(Equal(int32(2)))
			},
		},
		{
			name: "When profiles set container bounds, it should merge cpu and memory separately",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.Profiles = append(c.Spec.Profiles, schedulingv1alpha1.AutoscalingProfile{
					Name:           "warmup",
					HostedClusters: selectLabel("warmup", "true"),
					Workloads: []schedulingv1alpha1.WorkloadAutoscaling{
						workload(kasKey, schedulingv1alpha1.PodUpdatePolicy{}, schedulingv1alpha1.ContainerResourcePolicy{
							ContainerName: "kube-apiserver",
							MinAllowed:    schedulingv1alpha1.ResourceBounds{CPU: "2", Memory: "8Gi"},
							MaxAllowed:    schedulingv1alpha1.ResourceBounds{CPU: "14"},
						}),
					},
				})
				return c
			},
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Labels = map[string]string{"warmup": "true"}
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(workloadByKey(res, kasKey).containers).To(Equal([]schedulingv1alpha1.ContainerResourcePolicy{{
					ContainerName: "kube-apiserver",
					MinAllowed:    schedulingv1alpha1.ResourceBounds{CPU: "2", Memory: "8Gi"},
					MaxAllowed:    schedulingv1alpha1.ResourceBounds{CPU: "14", Memory: "32Gi"},
				}}))
			},
		},
		{
			name: "When a matching profile names a container of a workload that earlier profiles don't, it should add it with earlier allWorkloads settings",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.Profiles[1].Workloads = []schedulingv1alpha1.WorkloadAutoscaling{
					workload(workloadKey{resource: schedulingv1alpha1.WorkloadResourceDeployments, name: "openshift-apiserver"}, schedulingv1alpha1.PodUpdatePolicy{},
						schedulingv1alpha1.ContainerResourcePolicy{ContainerName: "openshift-apiserver"}),
				}
				return c
			},
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Labels = map[string]string{"autoscaling-canary": "true"}
			},
			verify: func(g *WithT, res *resolution) {
				oas := workloadByKey(res, workloadKey{resource: schedulingv1alpha1.WorkloadResourceDeployments, name: "openshift-apiserver"})
				g.Expect(oas.updateMode).To(Equal(vpaautoscalingv1.UpdateModeInPlaceOrRecreate))
				g.Expect(oas.profiles).To(Equal([]string{"base", "canary"}))
			},
		},
		{
			name: "When no matching profile names a container of a workload, it should not autoscale it and report it",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.Profiles[1].Workloads = []schedulingv1alpha1.WorkloadAutoscaling{
					workload(workloadKey{resource: schedulingv1alpha1.WorkloadResourceDeployments, name: "openshift-apiserver"}, schedulingv1alpha1.PodUpdatePolicy{MinReplicas: 1}),
				}
				return c
			},
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Labels = map[string]string{"autoscaling-canary": "true"}
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(workloadNames(res)).To(Equal([]string{"deployments/kube-apiserver", "deployments/kube-scheduler", "statefulsets/etcd"}))
				g.Expect(res.notes).To(ContainElement("Not autoscaled because no matching profile names a container: deployments/openshift-apiserver"))
			},
		},
		{
			name: "When a Deployment and a StatefulSet have the same name, it should resolve them separately",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.Profiles[0].Workloads = append(c.Spec.Profiles[0].Workloads,
					workload(workloadKey{resource: schedulingv1alpha1.WorkloadResourceDeployments, name: "etcd"}, schedulingv1alpha1.PodUpdatePolicy{},
						schedulingv1alpha1.ContainerResourcePolicy{ContainerName: "proxy"}))
				return c
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(workloadByKey(res, etcdKey).updateMode).To(Equal(vpaautoscalingv1.UpdateModeOff))
				g.Expect(workloadByKey(res, workloadKey{resource: schedulingv1alpha1.WorkloadResourceDeployments, name: "etcd"}).updateMode).To(Equal(vpaautoscalingv1.UpdateModeInPlaceOrRecreate))
			},
		},
		{
			name: "When no matching profile sets updateMode, it should use Off and report it",
			config: func(c *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				c.Spec.Profiles[0].AllWorkloads = schedulingv1alpha1.AllWorkloadsAutoscaling{}
				return c
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(workloadByKey(res, schedulerKey).updateMode).To(Equal(vpaautoscalingv1.UpdateModeOff))
				g.Expect(res.notes).To(ContainElement(ContainSubstring("no matching profile sets updateMode: deployments/kube-apiserver, deployments/kube-scheduler")))
			},
		},
		{
			name: "When the controller availability policy is SingleReplica, it should apply evicting modes as Initial",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Spec.ControllerAvailabilityPolicy = hyperv1.SingleReplica
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(workloadByKey(res, kasKey).updateMode).To(Equal(vpaautoscalingv1.UpdateModeInitial))
				g.Expect(workloadByKey(res, etcdKey).updateMode).To(Equal(vpaautoscalingv1.UpdateModeOff))
				g.Expect(res.notes).To(ContainElement(ContainSubstring("SingleReplica: deployments/kube-apiserver, deployments/kube-scheduler")))
			},
		},
		{
			name: "When a resource request override annotation is set, it should resolve a floor",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Annotations = map[string]string{hyperv1.ResourceRequestOverrideAnnotationPrefix + "/kube-apiserver.kube-apiserver": "memory=12Gi,cpu=2"}
			},
			verify: func(g *WithT, res *resolution) {
				g.Expect(res.floors).To(HaveKeyWithValue("kube-apiserver", HaveKey("kube-apiserver")))
				floor := res.floors["kube-apiserver"]["kube-apiserver"]
				g.Expect(floor.Memory().String()).To(Equal("12Gi"))
				g.Expect(floor.Cpu().String()).To(Equal("2"))
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			config := testConfig()
			if tc.config != nil {
				config = tc.config(config)
			}
			hc := testHostedCluster()
			if tc.hc != nil {
				tc.hc(hc)
			}
			res, err := resolve(config, hc)
			g.Expect(err).ToNot(HaveOccurred())
			tc.verify(g, res)
		})
	}
}

func TestParseResourceRequests(t *testing.T) {
	testCases := []struct {
		name            string
		value           string
		expected        corev1.ResourceList
		expectedInvalid []string
	}{
		{
			name:     "When memory and cpu are set, it should return both",
			value:    "memory=12Gi,cpu=2000m",
			expected: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("12Gi"), corev1.ResourceCPU: resource.MustParse("2000m")},
		},
		{
			name:     "When another resource is set, it should accept it but not return it as a floor",
			value:    "aro.openshift.io/swift-nic=1,memory=1Gi",
			expected: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		},
		{
			name:            "When one quantity is invalid, it should ignore that entry and return the others",
			value:           "memory=12Gi,cpu=2c",
			expected:        corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("12Gi")},
			expectedInvalid: []string{"cpu=2c"},
		},
		{
			name:            "When a quantity is zero, it should ignore the entry",
			value:           "cpu=0",
			expected:        corev1.ResourceList{},
			expectedInvalid: []string{"cpu=0"},
		},
		{
			name:     "When a resource is set twice, it should use the last entry like the control-plane operator",
			value:    "memory=1Gi,memory=2Gi",
			expected: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		},
		{
			name:     "When a resource other than cpu and memory is set twice, it should accept it",
			value:    "example.com/device=1,example.com/device=2,cpu=1",
			expected: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		},
		{
			name:            "When an entry has no quantity, it should ignore it and return the others",
			value:           "memory,cpu=1",
			expected:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			expectedInvalid: []string{"memory"},
		},
		{
			name:            "When an entry has no resource name, it should ignore it",
			value:           "=1Gi",
			expected:        corev1.ResourceList{},
			expectedInvalid: []string{"=1Gi"},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			result, invalid := parseResourceRequests(tc.value)
			g.Expect(invalid).To(Equal(tc.expectedInvalid))
			g.Expect(result).To(HaveLen(len(tc.expected)))
			for name, q := range tc.expected {
				got := result[name]
				g.Expect(got.Cmp(q)).To(Equal(0), string(name))
			}
		})
	}
}

func TestParseFloors(t *testing.T) {
	testCases := []struct {
		name           string
		annotations    map[string]string
		expectedFloors []string
		expectNote     bool
	}{
		{
			name: "When override annotations are valid, it should key floors by workload and container",
			annotations: map[string]string{
				hyperv1.ResourceRequestOverrideAnnotationPrefix + "/kube-apiserver.kube-apiserver": "memory=12Gi",
				hyperv1.ResourceRequestOverrideAnnotationPrefix + "/etcd.etcd":                     "cpu=1",
				"unrelated": "value",
			},
			expectedFloors: []string{"etcd/etcd", "kube-apiserver/kube-apiserver"},
		},
		{
			name: "When an override annotation has no container, it should ignore it and report it",
			annotations: map[string]string{
				hyperv1.ResourceRequestOverrideAnnotationPrefix + "/kube-apiserver": "memory=12Gi",
			},
			expectNote: true,
		},
		{
			name: "When an override annotation has an invalid entry, it should keep the valid entries and report the invalid one",
			annotations: map[string]string{
				hyperv1.ResourceRequestOverrideAnnotationPrefix + "/kube-apiserver.kube-apiserver": "memory=12Gi,cpu=2c",
			},
			expectedFloors: []string{"kube-apiserver/kube-apiserver"},
			expectNote:     true,
		},
		{
			name: "When an override annotation sets only other resources, it should not create a floor",
			annotations: map[string]string{
				hyperv1.ResourceRequestOverrideAnnotationPrefix + "/kube-apiserver.kube-apiserver": "aro.openshift.io/swift-nic=1",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			floors, notes := parseFloors(tc.annotations)
			var got []string
			for _, workload := range sortedKeys(floors) {
				for _, container := range sortedKeys(floors[workload]) {
					got = append(got, workload+"/"+container)
				}
			}
			g.Expect(got).To(Equal(tc.expectedFloors))
			if tc.expectNote {
				g.Expect(notes).To(HaveLen(1))
			} else {
				g.Expect(notes).To(BeEmpty())
			}
		})
	}
}
