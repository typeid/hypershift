package controlplaneautoscaling

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	schedulingv1alpha1 "github.com/openshift/hypershift/api/scheduling/v1alpha1"
	"github.com/openshift/hypershift/pkg/manifests"
	"github.com/openshift/hypershift/support/k8sutil"
	"github.com/openshift/hypershift/support/reconcilerpolicy"
	"github.com/openshift/hypershift/support/statuspatching"
	hyperutil "github.com/openshift/hypershift/support/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/client-go/discovery"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ControllerName = "ControlPlaneVerticalAutoscaling"

	// recommendationTimeout is how long a VerticalPodAutoscaler may go without a recommendation before
	// the HostedCluster reports RecommendationMissing.
	recommendationTimeout = 30 * time.Minute

	// vpaPollInterval is how often a HostedCluster is reconciled again while the VerticalPodAutoscaler API isn't
	// served, or while VerticalPodAutoscalers aren't watched because the API was installed after the operator started.
	vpaPollInterval = 5 * time.Minute

	// vpaServedRecheck is how long a check for the VerticalPodAutoscaler API is cached. Each check is a discovery
	// request.
	vpaServedRecheck = time.Minute
)

// Reconciler creates, updates and deletes a VerticalPodAutoscaler per configured control plane workload of
// each HostedCluster selected by the ControlPlaneVerticalAutoscalingConfiguration, and reports the result in the
// HostedCluster's ControlPlaneVerticalAutoscaling condition.
type Reconciler struct {
	client.Client
	// vpaServed reports whether the VerticalPodAutoscaler API is served on the management cluster.
	vpaServed func() bool
	// vpaWatched is true when the controller watches VerticalPodAutoscalers; otherwise it polls them.
	vpaWatched bool
	now        func() time.Time
}

// SetupWithManager registers the HostedCluster VerticalPodAutoscaler controller with the manager.
func SetupWithManager(mgr ctrl.Manager) error {
	registerMetrics()
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}
	r := &Reconciler{
		Client:    mgr.GetClient(),
		vpaServed: cachedVPAServed(vpaAPIServed(discoveryClient), time.Now),
		now:       time.Now,
	}
	// Watching VerticalPodAutoscalers requires the API to be served when the operator starts. If it is installed
	// later, VerticalPodAutoscalers are still created, and their status is polled.
	r.vpaWatched = r.vpaServed()
	existenceEvents := predicate.Funcs{
		// Only the object's existence matters, not its rollout status.
		UpdateFunc: func(event.UpdateEvent) bool { return false },
	}
	workloadEvents := predicate.Funcs{
		// A workload's replicas and containers matter too; both are in its spec, which bumps its generation.
		UpdateFunc: func(e event.UpdateEvent) bool { return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() },
	}
	bldr := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		// Only labels, annotations and spec affect the VerticalPodAutoscalers; ignoring status updates also keeps this
		// controller's own condition updates from triggering another reconcile.
		For(&hyperv1.HostedCluster{}, builder.WithPredicates(
			reconcilerpolicy.PredicatesForHostedClusterAnnotationScoping(mgr.GetClient()),
			predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{}, predicate.AnnotationChangedPredicate{}),
		)).
		// Every HostedCluster is reconciled for a configuration change, so metadata-only updates are ignored.
		Watches(&schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration{}, handler.EnqueueRequestsFromMapFunc(r.allHostedClusters),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&hyperv1.HostedControlPlane{}, handler.EnqueueRequestsFromMapFunc(hostedClusterForHCP), builder.WithPredicates(existenceEvents)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.hostedClusterForNamespace), builder.WithPredicates(workloadEvents)).
		Watches(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(r.hostedClusterForNamespace), builder.WithPredicates(workloadEvents))
	if r.vpaWatched {
		bldr = bldr.Watches(&vpaautoscalingv1.VerticalPodAutoscaler{}, handler.EnqueueRequestsFromMapFunc(r.hostedClusterForNamespace), builder.WithPredicates(vpaChanged()))
	} else {
		mgr.GetLogger().Info("VerticalPodAutoscaler API not served, control plane vertical autoscaling will not watch VerticalPodAutoscalers")
	}
	if err := bldr.Complete(r); err != nil {
		return fmt.Errorf("failed to set up %s controller: %w", ControllerName, err)
	}
	return nil
}

// vpaAPIServed returns a function that asks discovery whether the VerticalPodAutoscaler API is served.
func vpaAPIServed(discoveryClient discovery.DiscoveryInterface) func() (bool, error) {
	return func() (bool, error) {
		resources, err := discoveryClient.ServerResourcesForGroupVersion(vpaautoscalingv1.SchemeGroupVersion.String())
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(resources.APIResources, func(r metav1.APIResource) bool { return r.Name == "verticalpodautoscalers" }), nil
	}
}

// cachedVPAServed returns a function that reports whether the VerticalPodAutoscaler API is served, checking at most
// every vpaServedRecheck, so that installing or removing the Vertical Pod Autoscaler is noticed. When a check fails,
// the previous result is kept.
func cachedVPAServed(check func() (bool, error), now func() time.Time) func() bool {
	var mu sync.Mutex
	var served bool
	var checked time.Time
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		if !checked.IsZero() && now().Sub(checked) < vpaServedRecheck {
			return served
		}
		result, err := check()
		checked = now()
		if err == nil {
			served = result
		}
		return served
	}
}

// vpaChanged filters out VerticalPodAutoscaler status updates that don't change whether a recommendation
// is available or whether pods match, since recommenders update every VerticalPodAutoscaler's status frequently.
func vpaChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldVPA, okOld := e.ObjectOld.(*vpaautoscalingv1.VerticalPodAutoscaler)
			newVPA, okNew := e.ObjectNew.(*vpaautoscalingv1.VerticalPodAutoscaler)
			if !okOld || !okNew {
				return true
			}
			return oldVPA.Generation != newVPA.Generation ||
				hasRecommendation(oldVPA) != hasRecommendation(newVPA) ||
				noPodsMatched(oldVPA) != noPodsMatched(newVPA) ||
				!equality.Semantic.DeepEqual(oldVPA.Labels, newVPA.Labels) ||
				!equality.Semantic.DeepEqual(oldVPA.Annotations, newVPA.Annotations)
		},
	}
}

func (r *Reconciler) allHostedClusters(ctx context.Context, _ client.Object) []reconcile.Request {
	hcList := &hyperv1.HostedClusterList{}
	if err := r.List(ctx, hcList); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list HostedClusters")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(hcList.Items))
	for _, hc := range hcList.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&hc)})
	}
	return requests
}

func hostedClusterForHCP(_ context.Context, obj client.Object) []reconcile.Request {
	hcName := obj.GetAnnotations()[k8sutil.HostedClusterAnnotation]
	if hcName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: hyperutil.ParseNamespacedName(hcName)}}
}

// hostedClusterForNamespace maps an object in a control plane namespace to its HostedCluster, through the
// namespace's HostedControlPlane.
func (r *Reconciler) hostedClusterForNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	hcpList := &hyperv1.HostedControlPlaneList{}
	if err := r.List(ctx, hcpList, client.InNamespace(obj.GetNamespace())); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list HostedControlPlanes", "namespace", obj.GetNamespace())
		return nil
	}
	var requests []reconcile.Request
	for i := range hcpList.Items {
		requests = append(requests, hostedClusterForHCP(ctx, &hcpList.Items[i])...)
	}
	return requests
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	hc := &hyperv1.HostedCluster{}
	if err := r.Get(ctx, req.NamespacedName, hc); err != nil {
		if apierrors.IsNotFound(err) {
			deleteMetrics(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !hc.DeletionTimestamp.IsZero() {
		// VerticalPodAutoscalers are owned by the HostedControlPlane and are deleted with it.
		return ctrl.Result{}, nil
	}
	if isPaused, duration := reconcilerpolicy.IsReconciliationPaused(log, hc.Spec.PausedUntil); isPaused {
		// VerticalPodAutoscalers are left as they are; the pause doesn't stop the Vertical Pod Autoscaler itself.
		log.Info("Reconciliation paused", "pausedUntil", *hc.Spec.PausedUntil)
		// The condition is unchanged, but is exported again in case the operator restarted during the pause.
		if condition := meta.FindStatusCondition(hc.Status.Conditions, string(hyperv1.ControlPlaneVerticalAutoscaling)); condition != nil {
			setConditionMetric(hc.Namespace, hc.Name, condition.Status, condition.Reason)
		}
		return ctrl.Result{RequeueAfter: duration}, nil
	}

	config := &schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfiguration{}
	if err := r.Get(ctx, types.NamespacedName{Name: schedulingv1alpha1.ControlPlaneVerticalAutoscalingConfigurationName}, config); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get ControlPlaneVerticalAutoscalingConfiguration: %w", err)
		}
		config = nil
	}

	res, err := resolve(config, hc)
	if err != nil {
		return ctrl.Result{}, err
	}

	cpNamespace := manifests.HostedControlPlaneNamespace(hc.Namespace, hc.Name)
	vpaServed := r.vpaServed()

	if !res.selected {
		// The condition is only set on selected HostedClusters.
		if vpaServed {
			if err := r.deleteManagedVPAs(ctx, cpNamespace, nil); err != nil {
				return ctrl.Result{}, err
			}
		}
		deleteMetrics(hc.Namespace, hc.Name)
		return ctrl.Result{}, r.removeCondition(ctx, hc)
	}

	if !vpaServed {
		reason, message := hyperv1.ControlPlaneAutoscalingVPANotInstalledReason, "The VerticalPodAutoscaler API (autoscaling.k8s.io/v1) is not served on the management cluster"
		if res.reason != "" {
			reason, message = res.reason, res.message
		}
		deleteWorkloadMetrics(hc.Namespace, hc.Name)
		return ctrl.Result{RequeueAfter: vpaPollInterval}, r.setCondition(ctx, hc, metav1.ConditionFalse, reason, message)
	}

	if res.reason != "" {
		if err := r.deleteManagedVPAs(ctx, cpNamespace, nil); err != nil {
			return ctrl.Result{}, err
		}
		deleteWorkloadMetrics(hc.Namespace, hc.Name)
		return ctrl.Result{}, r.setCondition(ctx, hc, metav1.ConditionFalse, res.reason, res.message)
	}

	hcp := &hyperv1.HostedControlPlane{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cpNamespace, Name: hc.Name}, hcp); err != nil {
		if apierrors.IsNotFound(err) {
			// Workloads only exist once the HostedControlPlane does; its creation triggers a new reconcile.
			log.V(1).Info("HostedControlPlane not found yet, waiting before creating VerticalPodAutoscalers")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	status, err := r.reconcileVPAs(ctx, hc, hcp, res)
	if err != nil {
		return ctrl.Result{}, err
	}
	setMetrics(hc.Namespace, hc.Name, res.profiles, status.managed)

	conditionStatus, reason := metav1.ConditionTrue, hyperv1.AsExpectedReason
	switch {
	case len(status.rejected) > 0:
		conditionStatus, reason = metav1.ConditionFalse, hyperv1.ControlPlaneAutoscalingVPARejectedReason
	case len(status.conflicting) > 0:
		conditionStatus, reason = metav1.ConditionFalse, hyperv1.ControlPlaneAutoscalingVPAConflictReason
	case len(status.missingRecommendation) > 0:
		conditionStatus, reason = metav1.ConditionFalse, hyperv1.ControlPlaneAutoscalingRecommendationMissingReason
	}
	if err := r.setCondition(ctx, hc, conditionStatus, reason, status.message(res)); err != nil {
		return ctrl.Result{}, err
	}

	result := ctrl.Result{RequeueAfter: status.recheckAfter}
	if !r.vpaWatched && (result.RequeueAfter == 0 || result.RequeueAfter > vpaPollInterval) {
		result.RequeueAfter = vpaPollInterval
	}
	return result, nil
}

// reconcileStatus is the outcome of reconciling a HostedCluster's VerticalPodAutoscalers.
type reconcileStatus struct {
	// managed are the workloads that have a VerticalPodAutoscaler, with the update mode it uses.
	managed []managedWorkload
	// missing are autoscaled workloads that don't exist in the control plane namespace.
	missing []string
	// missingContainers are named containers that a workload's pod template doesn't have, as <workload>/<container>.
	missingContainers []string
	// noPods are workloads without running pods, so no recommendation is expected.
	noPods []string
	// conflicting are autoscaled workloads skipped because a VerticalPodAutoscaler not created by this controller
	// targets them.
	conflicting []string
	// rejected are workloads whose VerticalPodAutoscaler the API server rejected, with the reason.
	rejected []string
	// pendingRecommendation are workloads whose VerticalPodAutoscaler has no recommendation yet.
	pendingRecommendation []string
	// missingRecommendation are workloads whose VerticalPodAutoscaler has had no recommendation for recommendationTimeout.
	missingRecommendation []string
	// floors describe the active per-container floors.
	floors []string
	notes  []string
	// recheckAfter is when a pending recommendation reaches recommendationTimeout.
	recheckAfter time.Duration
}

type managedWorkload struct {
	name       string
	updateMode vpaautoscalingv1.UpdateMode
}

func (s *reconcileStatus) message(res *resolution) string {
	var parts []string
	add := func(format string, items []string) {
		if len(items) > 0 {
			parts = append(parts, fmt.Sprintf(format, strings.Join(items, ", ")))
		}
	}
	add("VerticalPodAutoscaler rejected: %s", s.rejected)
	add("Not autoscaled because a VerticalPodAutoscaler not created by HyperShift targets them; delete it to autoscale the workload: %s", s.conflicting)
	add("No recommendation for "+recommendationTimeout.String()+", check that the workload's recommender is running: %s", s.missingRecommendation)
	var managed []string
	for _, w := range s.managed {
		managed = append(managed, w.name)
	}
	add("Profiles: %s", res.profiles)
	add("Autoscaled workloads: %s", managed)
	add("Waiting for a first recommendation: %s", s.pendingRecommendation)
	add("No recommendation expected because the workload has no running pods: %s", s.noPods)
	add("Active floors: %s", s.floors)
	add("Autoscaled workloads not found in the control plane namespace: %s", s.missing)
	add("Named containers not found in the workload's pod template: %s", s.missingContainers)
	parts = append(parts, res.notes...)
	parts = append(parts, s.notes...)
	if len(parts) == 0 {
		return "No workloads are autoscaled"
	}
	return strings.Join(parts, ". ")
}

func (r *Reconciler) reconcileVPAs(ctx context.Context, hc *hyperv1.HostedCluster, hcp *hyperv1.HostedControlPlane, res *resolution) (*reconcileStatus, error) {
	namespace := hcp.Namespace
	status := &reconcileStatus{}

	vpaList := &vpaautoscalingv1.VerticalPodAutoscalerList{}
	if err := r.List(ctx, vpaList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list VerticalPodAutoscalers: %w", err)
	}
	foreignTargets := stringSet{}
	for _, vpa := range vpaList.Items {
		if vpa.Labels[managedLabel] == "true" || vpa.Spec.TargetRef == nil {
			continue
		}
		foreignTargets[vpa.Spec.TargetRef.Kind+"/"+vpa.Spec.TargetRef.Name] = struct{}{}
	}

	keep := stringSet{}
	var errs []error
	for _, w := range res.workloads {
		info, err := r.getWorkload(ctx, namespace, w.workloadKey)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !info.found {
			status.missing = append(status.missing, w.String())
			continue
		}
		// Skip named containers the pod template doesn't have, as for workloads that don't exist, rather than
		// waiting recommendationTimeout for a recommendation that can't come.
		w.containers = slices.DeleteFunc(slices.Clone(w.containers), func(c schedulingv1alpha1.ContainerResourcePolicy) bool {
			if info.containers.Has(c.ContainerName) {
				return false
			}
			status.missingContainers = append(status.missingContainers, w.String()+"/"+c.ContainerName)
			return true
		})
		if len(w.containers) == 0 {
			continue
		}
		if foreignTargets.Has(w.kind() + "/" + w.name) {
			status.conflicting = append(status.conflicting, w.String())
			continue
		}

		vpa := &vpaautoscalingv1.VerticalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: vpaName(w.workloadKey)}}
		keep[vpa.Name] = struct{}{}

		spec, notes := buildVPASpec(vpaSpecInput{workload: w, floors: res.floors[w.name]})
		status.notes = append(status.notes, notes...)
		status.floors = append(status.floors, describeFloors(w.name, res.floors[w.name])...)

		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, vpa, func() error {
			if vpa.Labels == nil {
				vpa.Labels = map[string]string{}
			}
			vpa.Labels[managedLabel] = "true"
			if vpa.Annotations == nil {
				vpa.Annotations = map[string]string{}
			}
			vpa.Annotations[profilesAnnotation] = strings.Join(w.profiles, ",")
			vpa.Annotations[k8sutil.HostedClusterAnnotation] = client.ObjectKeyFromObject(hc).String()
			vpa.Spec = spec
			return controllerutil.SetOwnerReference(hcp, vpa, r.Scheme())
		}); err != nil {
			if apierrors.IsInvalid(err) {
				status.rejected = append(status.rejected, fmt.Sprintf("%s (%v)", w, err))
				continue
			}
			errs = append(errs, fmt.Errorf("failed to reconcile VerticalPodAutoscaler for %s: %w", w, err))
			continue
		}
		status.managed = append(status.managed, managedWorkload{name: w.String(), updateMode: *spec.UpdatePolicy.UpdateMode})

		if !hasRecommendation(vpa) && (info.scaledToZero || noPodsMatched(vpa)) {
			status.noPods = append(status.noPods, w.String())
		} else if !hasRecommendation(vpa) {
			age := r.now().Sub(vpa.CreationTimestamp.Time)
			if vpa.CreationTimestamp.IsZero() {
				age = 0
			}
			if age >= recommendationTimeout {
				status.missingRecommendation = append(status.missingRecommendation, w.String())
			} else {
				status.pendingRecommendation = append(status.pendingRecommendation, w.String())
				if wait := recommendationTimeout - age; status.recheckAfter == 0 || wait < status.recheckAfter {
					status.recheckAfter = wait
				}
			}
		}
	}

	if err := r.deleteManagedVPAs(ctx, namespace, keep); err != nil {
		errs = append(errs, err)
	}
	return status, utilerrors.NewAggregate(errs)
}

// workloadInfo is what the controller knows about a workload in the control plane namespace.
type workloadInfo struct {
	found bool
	// scaledToZero is true for a workload with zero replicas, which has no pods to recommend for.
	scaledToZero bool
	// containers are the names of the containers in the workload's pod template.
	containers stringSet
}

// getWorkload reads the workload from the namespace.
func (r *Reconciler) getWorkload(ctx context.Context, namespace string, workload workloadKey) (workloadInfo, error) {
	key := types.NamespacedName{Namespace: namespace, Name: workload.name}
	var obj client.Object = &appsv1.Deployment{}
	if workload.resource == schedulingv1alpha1.WorkloadResourceStatefulSets {
		obj = &appsv1.StatefulSet{}
	}
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return workloadInfo{}, nil
		}
		return workloadInfo{}, fmt.Errorf("failed to get %s %s: %w", workload.kind(), workload.name, err)
	}
	var replicas *int32
	var podSpec corev1.PodSpec
	switch o := obj.(type) {
	case *appsv1.Deployment:
		replicas, podSpec = o.Spec.Replicas, o.Spec.Template.Spec
	case *appsv1.StatefulSet:
		replicas, podSpec = o.Spec.Replicas, o.Spec.Template.Spec
	}
	info := workloadInfo{found: true, scaledToZero: replicas != nil && *replicas == 0, containers: stringSet{}}
	for _, c := range podSpec.Containers {
		info.containers[c.Name] = struct{}{}
	}
	return info, nil
}

// deleteManagedVPAs deletes VerticalPodAutoscalers created by this controller in the namespace, except those in keep.
func (r *Reconciler) deleteManagedVPAs(ctx context.Context, namespace string, keep stringSet) error {
	vpaList := &vpaautoscalingv1.VerticalPodAutoscalerList{}
	if err := r.List(ctx, vpaList, client.InNamespace(namespace), client.MatchingLabels{managedLabel: "true"}); err != nil {
		return fmt.Errorf("failed to list VerticalPodAutoscalers: %w", err)
	}
	var errs []error
	for i := range vpaList.Items {
		vpa := &vpaList.Items[i]
		if keep.Has(vpa.Name) {
			continue
		}
		if _, err := k8sutil.DeleteIfNeeded(ctx, r.Client, vpa); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete VerticalPodAutoscaler %s: %w", vpa.Name, err))
		}
	}
	return utilerrors.NewAggregate(errs)
}

func describeFloors(workload string, floors map[string]corev1.ResourceList) []string {
	var result []string
	for _, container := range sortedKeys(floors) {
		var values []string
		for _, name := range sortedKeys(floors[container]) {
			q := floors[container][name]
			values = append(values, fmt.Sprintf("%s=%s", name, q.String()))
		}
		result = append(result, fmt.Sprintf("%s/%s %s", workload, container, strings.Join(values, ",")))
	}
	return result
}

func (r *Reconciler) setCondition(ctx context.Context, hc *hyperv1.HostedCluster, status metav1.ConditionStatus, reason, message string) error {
	condition := metav1.Condition{
		Type:               string(hyperv1.ControlPlaneVerticalAutoscaling),
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: hc.Generation,
	}
	setConditionMetric(hc.Namespace, hc.Name, status, reason)
	if existing := meta.FindStatusCondition(hc.Status.Conditions, condition.Type); existing != nil &&
		existing.Status == condition.Status && existing.Reason == condition.Reason &&
		existing.Message == condition.Message && existing.ObservedGeneration == condition.ObservedGeneration {
		return nil
	}
	return statuspatching.PatchStatusCondition(ctx, r.Client, hc, &hc.Status.Conditions, condition)
}

// removeCondition removes the ControlPlaneVerticalAutoscaling condition, which is only set on selected HostedClusters.
func (r *Reconciler) removeCondition(ctx context.Context, hc *hyperv1.HostedCluster) error {
	if meta.FindStatusCondition(hc.Status.Conditions, string(hyperv1.ControlPlaneVerticalAutoscaling)) == nil {
		return nil
	}
	return statuspatching.PatchStatus(ctx, r.Client, hc, func() error {
		meta.RemoveStatusCondition(&hc.Status.Conditions, string(hyperv1.ControlPlaneVerticalAutoscaling))
		return nil
	})
}
