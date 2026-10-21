package controlplaneautoscaling

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	schedulingv1alpha1 "github.com/openshift/hypershift/api/scheduling/v1alpha1"
	hyperapi "github.com/openshift/hypershift/support/api"
	"github.com/openshift/hypershift/support/k8sutil"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

const testCPNamespace = "clusters-example"

var testNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func testHCP() *hyperv1.HostedControlPlane {
	return &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   testCPNamespace,
			Name:        "example",
			UID:         "hcp-uid",
			Annotations: map[string]string{k8sutil.HostedClusterAnnotation: "clusters/example"},
		},
	}
}

// testDeployment returns a Deployment whose pod template has a container named like the Deployment and a sidecar.
func testDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: testCPNamespace, Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name}, {Name: "sidecar"}}}},
		},
	}
}

// testStatefulSet returns a StatefulSet whose pod template has a container named like the StatefulSet and a sidecar.
func testStatefulSet(name string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: testCPNamespace, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name}, {Name: "sidecar"}}}},
		},
	}
}

func deploymentKey(name string) workloadKey {
	return workloadKey{resource: schedulingv1alpha1.WorkloadResourceDeployments, name: name}
}

func managedVPA(workload workloadKey, created time.Time) *vpaautoscalingv1.VerticalPodAutoscaler {
	return &vpaautoscalingv1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         testCPNamespace,
			Name:              vpaName(workload),
			Labels:            map[string]string{managedLabel: "true"},
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: vpaautoscalingv1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: workload.kind(), Name: workload.name},
		},
	}
}

func foreignVPA(name, workload string) *vpaautoscalingv1.VerticalPodAutoscaler {
	return &vpaautoscalingv1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: testCPNamespace, Name: name},
		Spec: vpaautoscalingv1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: workload},
		},
	}
}

func getVPA(g *WithT, c client.Client, workload workloadKey) *vpaautoscalingv1.VerticalPodAutoscaler {
	vpa := &vpaautoscalingv1.VerticalPodAutoscaler{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: testCPNamespace, Name: vpaName(workload)}, vpa)).To(Succeed())
	return vpa
}

func vpaExists(g *WithT, c client.Client, namespace, name string) bool {
	err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &vpaautoscalingv1.VerticalPodAutoscaler{})
	if apierrors.IsNotFound(err) {
		return false
	}
	g.Expect(err).ToNot(HaveOccurred())
	return true
}

func findHCCondition(g *WithT, c client.Client) *metav1.Condition {
	hc := &hyperv1.HostedCluster{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: "example"}, hc)).To(Succeed())
	return meta.FindStatusCondition(hc.Status.Conditions, string(hyperv1.ControlPlaneVerticalAutoscaling))
}

func hcCondition(g *WithT, c client.Client) *metav1.Condition {
	condition := findHCCondition(g, c)
	g.Expect(condition).ToNot(BeNil())
	return condition
}

// conditionMetricSeries returns the condition metric's series for the test HostedCluster, as <status>/<reason>.
func conditionMetricSeries() []string {
	var series []string
	metrics := make(chan prometheus.Metric, 16)
	conditionMetric.Collect(metrics)
	close(metrics)
	for m := range metrics {
		pb := &dto.Metric{}
		if err := m.Write(pb); err != nil {
			continue
		}
		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["namespace"] == "clusters" && labels["name"] == "example" {
			series = append(series, labels["status"]+"/"+labels["reason"])
		}
	}
	return series
}

// withExistingCondition gives the HostedCluster a ControlPlaneVerticalAutoscaling condition from an earlier reconcile.
func withExistingCondition(hc *hyperv1.HostedCluster) {
	hc.Status.Conditions = []metav1.Condition{{
		Type: string(hyperv1.ControlPlaneVerticalAutoscaling), Status: metav1.ConditionTrue, Reason: hyperv1.AsExpectedReason, LastTransitionTime: metav1.NewTime(testNow),
	}}
}

func TestReconcile(t *testing.T) {
	testCases := []struct {
		name         string
		config       func(*schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration
		hc           func(*hyperv1.HostedCluster)
		objects      []client.Object
		noHCP        bool
		vpaServed    *bool
		vpaUnwatched bool
		interceptor  *interceptor.Funcs
		verify       func(*WithT, client.Client, reconcile.Result)
	}{
		{
			name:    "When the HostedCluster is selected, it should create a VerticalPodAutoscaler per existing workload owned by the HostedControlPlane",
			objects: []client.Object{testDeployment("kube-apiserver"), testDeployment("kube-scheduler"), testStatefulSet("etcd")},
			verify: func(g *WithT, c client.Client, result reconcile.Result) {
				kas := getVPA(g, c, kasKey)
				g.Expect(kas.Labels).To(HaveKeyWithValue(managedLabel, "true"))
				g.Expect(kas.Annotations).To(HaveKeyWithValue(k8sutil.HostedClusterAnnotation, "clusters/example"))
				g.Expect(kas.Annotations).To(HaveKeyWithValue(profilesAnnotation, "base"))
				g.Expect(kas.OwnerReferences).To(ConsistOf(HaveField("UID", BeEquivalentTo("hcp-uid"))))
				g.Expect(kas.Spec.TargetRef.Kind).To(Equal("Deployment"))
				g.Expect(kas.Spec.Recommenders).To(BeEmpty())

				etcd := getVPA(g, c, etcdKey)
				g.Expect(etcd.Spec.TargetRef.Kind).To(Equal("StatefulSet"))
				g.Expect(*etcd.Spec.UpdatePolicy.UpdateMode).To(Equal(vpaautoscalingv1.UpdateModeOff))

				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(condition.Reason).To(Equal(hyperv1.AsExpectedReason))
				g.Expect(condition.ObservedGeneration).To(Equal(int64(7)))
				g.Expect(condition.Message).To(ContainSubstring("Profiles: base"))
				g.Expect(condition.Message).To(ContainSubstring("Autoscaled workloads: deployments/kube-apiserver, deployments/kube-scheduler, statefulsets/etcd"))
				g.Expect(condition.Message).To(ContainSubstring("Waiting for a first recommendation"))
				g.Expect(result.RequeueAfter).To(Equal(recommendationTimeout))

				g.Expect(testutil.ToFloat64(profileMetric.WithLabelValues("clusters", "example", "base"))).To(Equal(float64(1)))
				g.Expect(testutil.ToFloat64(workloadsMetric.WithLabelValues("clusters", "example", string(vpaautoscalingv1.UpdateModeInPlaceOrRecreate)))).To(Equal(float64(2)))
				g.Expect(conditionMetricSeries()).To(ConsistOf("True/" + hyperv1.AsExpectedReason))
			},
		},
		{
			name:    "When a configured workload doesn't exist, it should skip it and list it in the condition",
			objects: []client.Object{testDeployment("kube-apiserver")},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(schedulerKey))).To(BeFalse())
				g.Expect(hcCondition(g, c).Message).To(ContainSubstring("not found in the control plane namespace: deployments/kube-scheduler, statefulsets/etcd"))
			},
		},
		{
			name: "When another VerticalPodAutoscaler targets a workload, it should skip that workload and report VPAConflict",
			objects: []client.Object{
				testDeployment("kube-apiserver"), testDeployment("kube-scheduler"), testStatefulSet("etcd"),
				foreignVPA("kube-apiserver", "kube-apiserver"),
			},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(kasKey))).To(BeFalse())
				g.Expect(vpaExists(g, c, testCPNamespace, "kube-apiserver")).To(BeTrue())
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(schedulerKey))).To(BeTrue(), "other workloads are still autoscaled")
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(condition.Reason).To(Equal(hyperv1.ControlPlaneAutoscalingVPAConflictReason))
				g.Expect(conditionMetricSeries()).To(ConsistOf("False/" + hyperv1.ControlPlaneAutoscalingVPAConflictReason))
				g.Expect(condition.Message).To(HavePrefix("Not autoscaled because a VerticalPodAutoscaler not created by HyperShift targets them; delete it to autoscale the workload: deployments/kube-apiserver"))
			},
		},
		{
			name: "When a VerticalPodAutoscaler is rejected and another conflicts, it should report VPARejected",
			objects: []client.Object{
				testDeployment("kube-apiserver"), testDeployment("kube-scheduler"),
				foreignVPA("kube-apiserver", "kube-apiserver"),
			},
			interceptor: &interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*vpaautoscalingv1.VerticalPodAutoscaler); ok {
						return apierrors.NewInvalid(schema.GroupKind{Group: "autoscaling.k8s.io", Kind: "VerticalPodAutoscaler"}, obj.GetName(), nil)
					}
					return c.Create(ctx, obj, opts...)
				},
			},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				condition := hcCondition(g, c)
				g.Expect(condition.Reason).To(Equal(hyperv1.ControlPlaneAutoscalingVPARejectedReason))
				g.Expect(condition.Message).To(ContainSubstring("not created by HyperShift targets them; delete it to autoscale the workload: deployments/kube-apiserver"))
			},
		},
		{
			name: "When a workload is no longer configured, it should delete its VerticalPodAutoscaler",
			objects: []client.Object{
				testDeployment("kube-apiserver"), testDeployment("openshift-apiserver"),
				managedVPA(deploymentKey("openshift-apiserver"), testNow),
			},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(deploymentKey("openshift-apiserver")))).To(BeFalse())
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(kasKey))).To(BeTrue())
			},
		},
		{
			name: "When a workload no longer has a named container, it should delete its VerticalPodAutoscaler",
			config: func(config *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				config.Spec.Profiles[0].Workloads[1].ResourcePolicy = schedulingv1alpha1.PodResourcePolicy{}
				config.Spec.Profiles[0].Workloads[1].UpdatePolicy = schedulingv1alpha1.PodUpdatePolicy{MinReplicas: 1}
				return config
			},
			objects: []client.Object{testDeployment("kube-apiserver"), testDeployment("kube-scheduler"), managedVPA(schedulerKey, testNow)},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(schedulerKey))).To(BeFalse())
				g.Expect(hcCondition(g, c).Message).To(ContainSubstring("Not autoscaled because no matching profile names a container: deployments/kube-scheduler"))
			},
		},
		{
			name: "When no configuration exists, it should delete managed VerticalPodAutoscalers and remove the condition",
			config: func(*schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				return nil
			},
			hc: withExistingCondition,
			objects: []client.Object{
				testDeployment("kube-apiserver"),
				managedVPA(kasKey, testNow),
				foreignVPA("kube-apiserver", "kube-apiserver"),
			},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(kasKey))).To(BeFalse())
				g.Expect(vpaExists(g, c, testCPNamespace, "kube-apiserver")).To(BeTrue())
				g.Expect(findHCCondition(g, c)).To(BeNil())
			},
		},
		{
			name: "When the configuration doesn't select the HostedCluster, it should delete managed VerticalPodAutoscalers and remove the condition",
			config: func(config *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				config.Spec.HostedClusters = selectLabel("example.com/autoscaled", "true")
				return config
			},
			hc: func(hc *hyperv1.HostedCluster) {
				withExistingCondition(hc)
				setConditionMetric(hc.Namespace, hc.Name, metav1.ConditionTrue, hyperv1.AsExpectedReason)
			},
			objects: []client.Object{testDeployment("kube-apiserver"), managedVPA(kasKey, testNow)},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(kasKey))).To(BeFalse())
				g.Expect(findHCCondition(g, c)).To(BeNil())
				g.Expect(conditionMetricSeries()).To(BeEmpty())
			},
		},
		{
			name: "When reconciliation of the HostedCluster is paused, it should leave its VerticalPodAutoscalers and condition unchanged",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Spec.PausedUntil = ptr.To("true")
				withExistingCondition(hc)
			},
			objects: []client.Object{testDeployment("kube-apiserver"), testDeployment("openshift-apiserver"), managedVPA(deploymentKey("openshift-apiserver"), testNow)},
			verify: func(g *WithT, c client.Client, result reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(kasKey))).To(BeFalse())
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(deploymentKey("openshift-apiserver")))).To(BeTrue())
				g.Expect(hcCondition(g, c).Reason).To(Equal(hyperv1.AsExpectedReason))
				g.Expect(result.RequeueAfter).To(BeZero())
				g.Expect(conditionMetricSeries()).To(ConsistOf("True/"+hyperv1.AsExpectedReason), "the unchanged condition is exported after an operator restart")
			},
		},
		{
			name: "When the HostedCluster is sized by ClusterSizingConfiguration, it should create no VerticalPodAutoscaler",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Labels = map[string]string{hyperv1.HostedClusterSizeLabel: "large"}
			},
			objects: []client.Object{testDeployment("kube-apiserver")},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(kasKey))).To(BeFalse())
				g.Expect(hcCondition(g, c).Reason).To(Equal(hyperv1.ControlPlaneAutoscalingClusterSizingActiveReason))
			},
		},
		{
			name:      "When the VerticalPodAutoscaler API isn't served, it should report VPANotInstalled and check again later",
			vpaServed: ptr.To(false),
			objects:   []client.Object{testDeployment("kube-apiserver")},
			verify: func(g *WithT, c client.Client, result reconcile.Result) {
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(condition.Reason).To(Equal(hyperv1.ControlPlaneAutoscalingVPANotInstalledReason))
				g.Expect(conditionMetricSeries()).To(ConsistOf("False/" + hyperv1.ControlPlaneAutoscalingVPANotInstalledReason))
				g.Expect(testutil.CollectAndCount(workloadsMetric)).To(BeZero())
				g.Expect(result.RequeueAfter).To(Equal(vpaPollInterval))
			},
		},
		{
			name:    "When the HostedControlPlane doesn't exist yet, it should create no VerticalPodAutoscaler",
			noHCP:   true,
			objects: []client.Object{testDeployment("kube-apiserver")},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(kasKey))).To(BeFalse())
			},
		},
		{
			name: "When a floor annotation is set, it should raise minAllowed and list the floor in the condition",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Annotations = map[string]string{hyperv1.ResourceRequestOverrideAnnotationPrefix + "/kube-apiserver.kube-apiserver": "memory=12Gi"}
			},
			objects: []client.Object{testDeployment("kube-apiserver")},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				kas := getVPA(g, c, kasKey)
				g.Expect(containerPolicy(kas.Spec, "kube-apiserver").MinAllowed.Memory().String()).To(Equal("12Gi"))
				g.Expect(hcCondition(g, c).Message).To(ContainSubstring("Active floors: kube-apiserver/kube-apiserver memory=12Gi"))
			},
		},
		{
			name: "When a named container isn't in the Deployment's pod template, it should leave it out and report it",
			config: func(config *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				config.Spec.Profiles[0].Workloads[0].ResourcePolicy.ContainerPolicies = append(config.Spec.Profiles[0].Workloads[0].ResourcePolicy.ContainerPolicies,
					schedulingv1alpha1.ContainerResourcePolicy{ContainerName: "audit-logs"})
				config.Spec.Profiles[0].Workloads[1].ResourcePolicy.ContainerPolicies[0].ContainerName = "scheduler"
				return config
			},
			objects: []client.Object{testDeployment("kube-apiserver"), testDeployment("kube-scheduler"), managedVPA(schedulerKey, testNow)},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				kas := getVPA(g, c, kasKey)
				g.Expect(containerPolicy(kas.Spec, "kube-apiserver")).ToNot(BeNil())
				g.Expect(containerPolicy(kas.Spec, "audit-logs")).To(BeNil())
				g.Expect(vpaExists(g, c, testCPNamespace, vpaName(schedulerKey))).To(BeFalse(), "a workload with no named container left gets no VerticalPodAutoscaler")
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(condition.Message).To(ContainSubstring("Named containers not found in the workload's pod template: deployments/kube-apiserver/audit-logs, deployments/kube-scheduler/scheduler"))
			},
		},
		{
			name: "When a named container isn't in the StatefulSet's pod template, it should leave it out and report it",
			config: func(config *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration) *schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration {
				config.Spec.Profiles[0].Workloads[2].ResourcePolicy.ContainerPolicies = append(config.Spec.Profiles[0].Workloads[2].ResourcePolicy.ContainerPolicies,
					schedulingv1alpha1.ContainerResourcePolicy{ContainerName: "etcd-metric"})
				return config
			},
			objects: []client.Object{testStatefulSet("etcd")},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				etcd := getVPA(g, c, etcdKey)
				g.Expect(containerPolicy(etcd.Spec, "etcd")).ToNot(BeNil())
				g.Expect(containerPolicy(etcd.Spec, "etcd-metric")).To(BeNil())
				g.Expect(hcCondition(g, c).Message).To(ContainSubstring("Named containers not found in the workload's pod template: statefulsets/etcd/etcd-metric"))
			},
		},
		{
			name: "When a StatefulSet is scaled to zero, it should not report a missing recommendation",
			objects: []client.Object{
				func() client.Object {
					statefulSet := testStatefulSet("etcd")
					statefulSet.Spec.Replicas = ptr.To[int32](0)
					return statefulSet
				}(),
				managedVPA(etcdKey, testNow.Add(-time.Hour)),
			},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(condition.Message).To(ContainSubstring("No recommendation expected because the workload has no running pods: statefulsets/etcd"))
			},
		},
		{
			name: "When a floor annotation has an invalid entry, it should still apply the valid entries and report the invalid one",
			hc: func(hc *hyperv1.HostedCluster) {
				hc.Annotations = map[string]string{hyperv1.ResourceRequestOverrideAnnotationPrefix + "/kube-apiserver.kube-apiserver": "memory=12Gi,cpu=2c"}
			},
			objects: []client.Object{testDeployment("kube-apiserver")},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				kas := getVPA(g, c, kasKey)
				g.Expect(containerPolicy(kas.Spec, "kube-apiserver").MinAllowed.Memory().String()).To(Equal("12Gi"))
				g.Expect(hcCondition(g, c).Message).To(ContainSubstring(`Ignored invalid entries in resource request override annotations: ` +
					hyperv1.ResourceRequestOverrideAnnotationPrefix + `/kube-apiserver.kube-apiserver ("cpu=2c")`))
			},
		},
		{
			name: "When a Deployment is scaled to zero, it should not report a missing recommendation",
			objects: []client.Object{
				func() client.Object {
					deployment := testDeployment("kube-apiserver")
					deployment.Spec.Replicas = ptr.To[int32](0)
					return deployment
				}(),
				managedVPA(kasKey, testNow.Add(-time.Hour)),
			},
			verify: func(g *WithT, c client.Client, result reconcile.Result) {
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(condition.Message).To(ContainSubstring("No recommendation expected because the workload has no running pods: deployments/kube-apiserver"))
				g.Expect(result.RequeueAfter).To(BeZero())
			},
		},
		{
			name: "When the recommender reports that no pods match, it should not report a missing recommendation",
			objects: []client.Object{
				testStatefulSet("etcd"),
				func() client.Object {
					vpa := managedVPA(etcdKey, testNow.Add(-time.Hour))
					vpa.Status.Conditions = []vpaautoscalingv1.VerticalPodAutoscalerCondition{{Type: vpaautoscalingv1.NoPodsMatched, Status: corev1.ConditionTrue}}
					return vpa
				}(),
			},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(condition.Message).To(ContainSubstring("no running pods: statefulsets/etcd"))
			},
		},
		{
			name:    "When a VerticalPodAutoscaler has had no recommendation for too long, it should report RecommendationMissing",
			objects: []client.Object{testDeployment("kube-apiserver"), managedVPA(kasKey, testNow.Add(-31*time.Minute))},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(condition.Reason).To(Equal(hyperv1.ControlPlaneAutoscalingRecommendationMissingReason))
				g.Expect(condition.Message).To(ContainSubstring("check that the workload's recommender is running: deployments/kube-apiserver"))
			},
		},
		{
			name: "When every VerticalPodAutoscaler has a recommendation, it should report AsExpected without requeueing",
			objects: []client.Object{
				testDeployment("kube-apiserver"),
				func() client.Object {
					vpa := managedVPA(kasKey, testNow.Add(-time.Hour))
					vpa.Status = vpaautoscalingv1.VerticalPodAutoscalerStatus{
						Recommendation: &vpaautoscalingv1.RecommendedPodResources{ContainerRecommendations: []vpaautoscalingv1.RecommendedContainerResources{{ContainerName: "kube-apiserver"}}},
						Conditions:     []vpaautoscalingv1.VerticalPodAutoscalerCondition{{Type: vpaautoscalingv1.RecommendationProvided, Status: corev1.ConditionTrue}},
					}
					return vpa
				}(),
			},
			verify: func(g *WithT, c client.Client, result reconcile.Result) {
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(condition.Message).ToNot(ContainSubstring("Waiting"))
				g.Expect(result.RequeueAfter).To(BeZero())
			},
		},
		{
			name:         "When VerticalPodAutoscalers aren't watched, it should poll them even when every one has a recommendation",
			vpaUnwatched: true,
			objects: []client.Object{testDeployment("kube-apiserver"),
				func() client.Object {
					vpa := managedVPA(kasKey, testNow.Add(-time.Hour))
					vpa.Status = vpaautoscalingv1.VerticalPodAutoscalerStatus{
						Recommendation: &vpaautoscalingv1.RecommendedPodResources{ContainerRecommendations: []vpaautoscalingv1.RecommendedContainerResources{{ContainerName: "kube-apiserver"}}},
						Conditions:     []vpaautoscalingv1.VerticalPodAutoscalerCondition{{Type: vpaautoscalingv1.RecommendationProvided, Status: corev1.ConditionTrue}},
					}
					return vpa
				}(),
			},
			verify: func(g *WithT, c client.Client, result reconcile.Result) {
				g.Expect(hcCondition(g, c).Status).To(Equal(metav1.ConditionTrue))
				g.Expect(result.RequeueAfter).To(Equal(vpaPollInterval))
			},
		},
		{
			name:         "When VerticalPodAutoscalers aren't watched and a recommendation is pending, it should poll before the recommendation timeout",
			vpaUnwatched: true,
			objects:      []client.Object{testDeployment("kube-apiserver")},
			verify: func(g *WithT, c client.Client, result reconcile.Result) {
				g.Expect(result.RequeueAfter).To(Equal(vpaPollInterval))
			},
		},
		{
			name:    "When the API server rejects a VerticalPodAutoscaler, it should report VPARejected",
			objects: []client.Object{testDeployment("kube-apiserver")},
			interceptor: &interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*vpaautoscalingv1.VerticalPodAutoscaler); ok {
						return apierrors.NewInvalid(schema.GroupKind{Group: "autoscaling.k8s.io", Kind: "VerticalPodAutoscaler"}, obj.GetName(),
							field.ErrorList{field.NotSupported(field.NewPath("spec", "updatePolicy", "updateMode"), "InPlaceOrRecreate", []string{"Off", "Initial", "Recreate", "Auto"})})
					}
					return c.Create(ctx, obj, opts...)
				},
			},
			verify: func(g *WithT, c client.Client, _ reconcile.Result) {
				condition := hcCondition(g, c)
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(condition.Reason).To(Equal(hyperv1.ControlPlaneAutoscalingVPARejectedReason))
				g.Expect(condition.Message).To(ContainSubstring("deployments/kube-apiserver"))
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			hc := testHostedCluster()
			if tc.hc != nil {
				tc.hc(hc)
			}
			objects := []client.Object{hc}
			config := testConfig()
			if tc.config != nil {
				config = tc.config(config)
			}
			if config != nil {
				objects = append(objects, config)
			}
			if !tc.noHCP {
				objects = append(objects, testHCP())
			}
			objects = append(objects, tc.objects...)

			builder := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).WithObjects(objects...).WithStatusSubresource(hc)
			if tc.interceptor != nil {
				builder = builder.WithInterceptorFuncs(*tc.interceptor)
			}
			c := builder.Build()
			served := true
			if tc.vpaServed != nil {
				served = *tc.vpaServed
			}
			r := &Reconciler{
				Client:     c,
				vpaServed:  func() bool { return served },
				vpaWatched: !tc.vpaUnwatched,
				now:        func() time.Time { return testNow },
			}
			result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(hc)})
			g.Expect(err).ToNot(HaveOccurred())
			tc.verify(g, c, result)
		})
	}

	t.Run("When the HostedCluster doesn't exist, it should do nothing", func(t *testing.T) {
		g := NewWithT(t)
		c := fake.NewClientBuilder().WithScheme(hyperapi.Scheme).Build()
		r := &Reconciler{Client: c, vpaServed: func() bool { return true }, vpaWatched: true, now: func() time.Time { return testNow }}
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Namespace: "clusters", Name: "missing"}})
		g.Expect(err).ToNot(HaveOccurred())
	})
}

func TestCachedVPAServed(t *testing.T) {
	t.Run("When the API is installed or removed, it should notice it after the recheck interval", func(t *testing.T) {
		g := NewWithT(t)
		now := testNow
		installed, calls := false, 0
		served := cachedVPAServed(func() (bool, error) { calls++; return installed, nil }, func() time.Time { return now })
		g.Expect(served()).To(BeFalse())

		installed = true
		now = now.Add(vpaServedRecheck - time.Second)
		g.Expect(served()).To(BeFalse(), "the result is cached until the recheck interval passes")
		now = now.Add(time.Second)
		g.Expect(served()).To(BeTrue())

		installed = false
		now = now.Add(vpaServedRecheck)
		g.Expect(served()).To(BeFalse())
		g.Expect(calls).To(Equal(3))
	})

	t.Run("When a check fails, it should keep the previous result", func(t *testing.T) {
		g := NewWithT(t)
		now := testNow
		var err error
		served := cachedVPAServed(func() (bool, error) { return err == nil, err }, func() time.Time { return now })
		g.Expect(served()).To(BeTrue())

		err = errors.New("discovery failed")
		now = now.Add(vpaServedRecheck)
		g.Expect(served()).To(BeTrue())
	})
}
