//go:build e2ev2

/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tests

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	schedulingv1alpha1 "github.com/openshift/hypershift/api/scheduling/v1alpha1"
	"github.com/openshift/hypershift/test/e2e/v2/internal"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// verticalAutoscalingTestLabel selects only the test's HostedCluster, so the configuration doesn't affect other
	// HostedClusters on the management cluster.
	verticalAutoscalingTestLabel = "e2e.hypershift.openshift.io/control-plane-vertical-autoscaling"

	verticalAutoscalingTimeout = 2 * time.Minute
)

// skipUnlessVerticalAutoscalingAvailable skips the test unless the ControlPlaneVerticalAutoscalingConfiguration API and the
// VerticalPodAutoscaler API are served, and no configuration exists that the test would overwrite.
func skipUnlessVerticalAutoscalingAvailable(tc *internal.TestContext) {
	GinkgoHelper()
	err := tc.MgmtClient.Get(tc.Context, crclient.ObjectKey{Name: schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfigurationName},
		&schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration{})
	switch {
	case meta.IsNoMatchError(err):
		Skip("ControlPlaneVerticalAutoscalingConfiguration API is not installed; the HyperShift operator needs the TechPreviewNoUpgrade feature set")
	case err == nil:
		Skip("a ControlPlaneVerticalAutoscalingConfiguration already exists on the management cluster")
	case !apierrors.IsNotFound(err):
		Expect(err).NotTo(HaveOccurred())
	}
	if err := tc.MgmtClient.List(tc.Context, &vpaautoscalingv1.VerticalPodAutoscalerList{}, crclient.InNamespace(tc.ControlPlaneNamespace)); meta.IsNoMatchError(err) {
		Skip("VerticalPodAutoscaler API is not installed on the management cluster")
	}
}

func verticalAutoscalingCondition(tc *internal.TestContext) *metav1.Condition {
	GinkgoHelper()
	hc, err := tc.GetHostedCluster()
	Expect(err).NotTo(HaveOccurred())
	return meta.FindStatusCondition(hc.Status.Conditions, string(hyperv1.ControlPlaneVerticalAutoscaling))
}

// ControlPlaneVerticalAutoscalingTest validates that a ControlPlaneVerticalAutoscalingConfiguration creates a
// VerticalPodAutoscaler only for workloads with a named container, deletes it when the workload is removed from the
// configuration, and removes the HostedCluster condition when the configuration is deleted. It uses update mode Off,
// so no pod is resized.
func ControlPlaneVerticalAutoscalingTest(getTestCtx internal.TestContextGetter) {
	It("should create VerticalPodAutoscalers only for workloads with a named container, and clean them up", func() {
		tc := getTestCtx()
		skipUnlessVerticalAutoscalingAvailable(tc)
		hc, err := tc.GetHostedCluster()
		Expect(err).NotTo(HaveOccurred())
		if _, sized := hc.Labels[hyperv1.HostedClusterSizeLabel]; sized || hc.Annotations[hyperv1.ResourceBasedControlPlaneAutoscalingAnnotation] == "true" {
			Skip("the hosted cluster is sized by ClusterSizingConfiguration")
		}

		By("labelling the HostedCluster so that only it is selected")
		original := hc.DeepCopy()
		if hc.Labels == nil {
			hc.Labels = map[string]string{}
		}
		hc.Labels[verticalAutoscalingTestLabel] = "true"
		Expect(tc.MgmtClient.Patch(tc.Context, hc, crclient.MergeFrom(original))).To(Succeed())
		DeferCleanup(func() {
			current, err := tc.GetHostedCluster()
			if apierrors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())
			patched := current.DeepCopy()
			delete(patched.Labels, verticalAutoscalingTestLabel)
			Expect(tc.MgmtClient.Patch(tc.Context, patched, crclient.MergeFrom(current))).To(Succeed())
		})

		By("creating a configuration that names a container of kube-apiserver but none of kube-scheduler")
		config := &schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfigurationName},
			Spec: schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfigurationSpec{
				HostedClusters: schedulingv1alpha1.HostedClusterSelection{
					Type: schedulingv1alpha1.HostedClusterSelectionSelector,
					Selector: schedulingv1alpha1.HostedClusterSelector{
						MatchLabels: map[string]schedulingv1alpha1.LabelValue{verticalAutoscalingTestLabel: "true"},
					},
				},
				Profiles: []schedulingv1alpha1.AutoscalingProfile{{
					Name:           "e2e",
					HostedClusters: schedulingv1alpha1.HostedClusterSelection{Type: schedulingv1alpha1.HostedClusterSelectionAll},
					AllWorkloads: schedulingv1alpha1.AllWorkloadsAutoscaling{
						UpdatePolicy: schedulingv1alpha1.PodUpdatePolicy{UpdateMode: schedulingv1alpha1.UpdateModeOff},
					},
					Workloads: []schedulingv1alpha1.WorkloadAutoscaling{
						{
							Resource: schedulingv1alpha1.WorkloadResourceDeployments,
							Name:     "kube-apiserver",
							ResourcePolicy: schedulingv1alpha1.PodResourcePolicy{
								ContainerPolicies: []schedulingv1alpha1.ContainerResourcePolicy{{ContainerName: "kube-apiserver"}},
							},
						},
						{
							Resource:     schedulingv1alpha1.WorkloadResourceDeployments,
							Name:         "kube-scheduler",
							UpdatePolicy: schedulingv1alpha1.PodUpdatePolicy{MinReplicas: 1},
						},
					},
				}},
			},
		}
		Expect(tc.MgmtClient.Create(tc.Context, config)).To(Succeed())
		DeferCleanup(func() {
			Expect(crclient.IgnoreNotFound(tc.MgmtClient.Delete(tc.Context, config))).To(Succeed())
		})

		kasVPA := crclient.ObjectKey{Namespace: tc.ControlPlaneNamespace, Name: "kube-apiserver-cp-autoscaling"}
		Eventually(func(g Gomega) {
			vpa := &vpaautoscalingv1.VerticalPodAutoscaler{}
			g.Expect(tc.MgmtClient.Get(tc.Context, kasVPA, vpa)).To(Succeed())
			g.Expect(vpa.Labels).To(HaveKeyWithValue("hypershift.openshift.io/control-plane-vertical-autoscaling-managed", "true"))
			g.Expect(vpa.Spec.TargetRef.Kind).To(Equal("Deployment"))
			g.Expect(vpa.Spec.TargetRef.Name).To(Equal("kube-apiserver"))
			g.Expect(*vpa.Spec.UpdatePolicy.UpdateMode).To(Equal(vpaautoscalingv1.UpdateModeOff))
		}).WithTimeout(verticalAutoscalingTimeout).Should(Succeed(), "a VerticalPodAutoscaler should be created for kube-apiserver")

		Eventually(func(g Gomega) {
			condition := verticalAutoscalingCondition(tc)
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "reason %q message %q", condition.Reason, condition.Message)
			g.Expect(condition.Message).To(ContainSubstring("Not autoscaled because no matching profile names a container: deployments/kube-scheduler"))
		}).WithTimeout(verticalAutoscalingTimeout).Should(Succeed())

		vpas := &vpaautoscalingv1.VerticalPodAutoscalerList{}
		Expect(tc.MgmtClient.List(tc.Context, vpas, crclient.InNamespace(tc.ControlPlaneNamespace),
			crclient.MatchingLabels{"hypershift.openshift.io/control-plane-vertical-autoscaling-managed": "true"})).To(Succeed())
		Expect(vpas.Items).To(HaveLen(1), "only kube-apiserver has a named container")

		By("removing kube-apiserver from the configuration")
		Expect(tc.MgmtClient.Get(tc.Context, crclient.ObjectKeyFromObject(config), config)).To(Succeed())
		config.Spec.Profiles[0].Workloads = config.Spec.Profiles[0].Workloads[1:]
		Expect(tc.MgmtClient.Update(tc.Context, config)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(tc.MgmtClient.Get(tc.Context, kasVPA, &vpaautoscalingv1.VerticalPodAutoscaler{}))
		}).WithTimeout(verticalAutoscalingTimeout).Should(BeTrue(), "the kube-apiserver VerticalPodAutoscaler should be deleted")

		By("deleting the configuration")
		Expect(tc.MgmtClient.Delete(tc.Context, config)).To(Succeed())
		Eventually(func() *metav1.Condition {
			return verticalAutoscalingCondition(tc)
		}).WithTimeout(verticalAutoscalingTimeout).Should(BeNil(), "the ControlPlaneVerticalAutoscaling condition should be removed")
	})
}

// RegisterControlPlaneVerticalAutoscalingTests registers all control plane vertical autoscaling tests.
func RegisterControlPlaneVerticalAutoscalingTests(getTestCtx internal.TestContextGetter) {
	ControlPlaneVerticalAutoscalingTest(getTestCtx)
}

var _ = Describe("[sig-hypershift][Jira:Hypershift][Feature:ControlPlaneVerticalAutoscaling] Control Plane Vertical Autoscaling", Label("lifecycle", "control-plane-vertical-autoscaling"), Serial, func() {
	var testCtx *internal.TestContext

	BeforeEach(func() {
		testCtx = internal.GetTestContext()
		Expect(testCtx).NotTo(BeNil(), "test context should be set up in BeforeSuite")
	})

	RegisterControlPlaneVerticalAutoscalingTests(func() *internal.TestContext { return testCtx })
})
