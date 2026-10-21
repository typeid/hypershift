package controlplaneautoscaling

import (
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	workloadsMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hypershift_control_plane_vertical_autoscaling_workloads",
		Help: "Number of control plane workloads of a HostedCluster managed by a VerticalPodAutoscaler, by update mode.",
	}, []string{"namespace", "name", "update_mode"})

	profileMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hypershift_control_plane_vertical_autoscaling_profile",
		Help: "Set to 1 for each ControlPlaneVerticalAutoscalingConfiguration profile that selects a HostedCluster.",
	}, []string{"namespace", "name", "profile"})

	conditionMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hypershift_control_plane_vertical_autoscaling_condition",
		Help: "Set to 1 for the status and reason of the ControlPlaneVerticalAutoscaling condition of each HostedCluster selected by the ControlPlaneVerticalAutoscalingConfiguration.",
	}, []string{"namespace", "name", "status", "reason"})

	registerMetricsOnce sync.Once
)

func registerMetrics() {
	registerMetricsOnce.Do(func() {
		metrics.Registry.MustRegister(workloadsMetric, profileMetric, conditionMetric)
	})
}

// setMetrics replaces the HostedCluster's workload counts and matching profiles.
func setMetrics(namespace, name string, profiles []string, managed []managedWorkload) {
	deleteWorkloadMetrics(namespace, name)
	for _, profile := range profiles {
		profileMetric.WithLabelValues(namespace, name, profile).Set(1)
	}
	for _, w := range managed {
		workloadsMetric.WithLabelValues(namespace, name, string(w.updateMode)).Inc()
	}
}

// setConditionMetric replaces the HostedCluster's condition status and reason.
func setConditionMetric(namespace, name string, status metav1.ConditionStatus, reason string) {
	conditionMetric.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
	conditionMetric.WithLabelValues(namespace, name, string(status), reason).Set(1)
}

// deleteWorkloadMetrics deletes the HostedCluster's workload counts and matching profiles, for a HostedCluster
// that is selected but not autoscaled.
func deleteWorkloadMetrics(namespace, name string) {
	workloadsMetric.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
	profileMetric.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
}

// deleteMetrics deletes every metric of the HostedCluster, for a HostedCluster that isn't selected or no longer exists.
func deleteMetrics(namespace, name string) {
	deleteWorkloadMetrics(namespace, name)
	conditionMetric.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
}
