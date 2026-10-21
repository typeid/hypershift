# Control Plane Vertical Autoscaling

Control plane vertical autoscaling resizes hosted control plane components from their observed usage with the
Vertical Pod Autoscaler (VPA). A management cluster administrator creates one cluster-scoped
`ControlPlaneVerticalAutoscalingConfiguration`; the HyperShift operator then creates one `VerticalPodAutoscaler` per
autoscaled workload in the control plane namespace of every selected HostedCluster.

**This feature is Tech Preview.** It requires the HyperShift operator to run with the `TechPreviewNoUpgrade` feature
set (`hypershift install --tech-preview-no-upgrade`), which enables the `ControlPlaneVerticalAutoscaling` feature gate
and installs the `ControlPlaneVerticalAutoscalingConfiguration` CRD.

Vertical autoscaling and [`ClusterSizingConfiguration`](resource-based-control-plane-autoscaling.md) size classes are
mutually exclusive. A HostedCluster with the `hypershift.openshift.io/hosted-cluster-size` label, or with the
`hypershift.openshift.io/resource-based-cp-auto-scaling: "true"` annotation, is never autoscaled by this feature.

## Prerequisites

- The VPA operator is installed on the management cluster with its updater and admission controller running, that is
  with `recommendationOnly: false` on its `VerticalPodAutoscalerController`. See
  [installing the VPA operator](resource-based-control-plane-autoscaling.md#vpa-operator).
- For the `InPlaceOrRecreate` update mode: a VPA version and a Kubernetes version that support in-place pod resize.

The VPAs use the default recommender. If the VPA API isn't served, selected HostedClusters report
`ControlPlaneVerticalAutoscaling=False` with reason `VPANotInstalled` and the control plane is otherwise unaffected.
The operator checks for the VPA API every minute, so installing or removing the VPA operator is noticed without a
restart. If the VPA operator is installed after the HyperShift operator starts, VPA status is polled every 5 minutes;
restart the HyperShift operator so it watches VPAs instead.

## Configuration

Only the instance named `cluster` is honoured. The configuration is an ordered list of profiles. Each profile selects
HostedClusters by label and sets VPA settings for their control plane workloads, using the field names and structure of
a `VerticalPodAutoscaler` spec.

```yaml
apiVersion: scheduling.hypershift.openshift.io/v1alpha1
kind: ControlPlaneVerticalAutoscalingConfiguration
metadata:
  name: cluster
spec:
  hostedClusters:              # which HostedClusters are autoscaled at all
    type: Selector             # or: type: All
    selector:
      matchExpressions:
      - { key: example.com/autoscaled, operator: Exists }
  profiles:
  - name: base                 # every selected HostedCluster
    hostedClusters: { type: All }
    allWorkloads:
      updatePolicy:            # VPA spec.updatePolicy, for every autoscaled workload
        updateMode: InPlaceOrRecreate
        minReplicas: 1
    workloads:
    - resource: deployments    # VPA spec.targetRef: an apps/v1 Deployment...
      name: kube-apiserver
      resourcePolicy:          # VPA spec.resourcePolicy
        containerPolicies:
        - containerName: kube-apiserver
          minAllowed: { cpu: 350m, memory: 2Gi }
          maxAllowed: { cpu: "14", memory: 56Gi }
    - resource: statefulsets   # ...or StatefulSet
      name: etcd
      resourcePolicy:
        containerPolicies:
        - containerName: etcd
          maxAllowed: { cpu: "14", memory: 56Gi }
  - name: warmup               # raised minimums for new clusters
    hostedClusters:
      type: Selector
      selector:
        matchLabels: { example.com/warmup: "true" }
    workloads:
    - resource: deployments
      name: kube-apiserver
      resourcePolicy:
        containerPolicies:
        - containerName: kube-apiserver
          minAllowed: { memory: 6Gi }
  - name: paused               # last, so that it wins
    hostedClusters:
      type: Selector
      selector:
        matchLabels: { control-plane-autoscaling-paused: "true" }
    allWorkloads:
      updatePolicy:
        updateMode: Initial
```

- **`hostedClusters`** selects the HostedClusters to autoscale. When it is omitted, nothing is selected. Profiles only
  apply to HostedClusters selected here, so a profile's selector alone never turns autoscaling on for a cluster.
- **`profiles`** apply in list order. Every profile whose `hostedClusters` matches a HostedCluster applies to it, and
  each field a profile sets replaces the value from earlier profiles. Within a profile, `allWorkloads` applies first
  and the workload's own entry second. List the profile for every cluster first, and profiles that must win, such as
  a [pause](#pause-autoscaling-on-one-cluster), last. A later profile can't unset a field; it can only set another
  value.
- **`allWorkloads.updatePolicy`** sets the VPA `updatePolicy` of every autoscaled workload of the matching clusters,
  including workloads named only in other profiles. It never makes a workload autoscaled.
- **`workloads`** names Deployments or StatefulSets in the control plane namespace by `resource` and `name`, up to
  128 per profile. A workload that doesn't exist on a given cluster, or a named container that the workload's pod
  template doesn't have, is skipped and listed in that cluster's condition, so one configuration can serve releases
  and platforms with different workloads. An entry sets:
    - `updatePolicy.updateMode`: `Off`, `Initial`, `Recreate` or `InPlaceOrRecreate`. When no matching profile sets
      it, `Off` is used and the workload is listed in the HostedCluster condition.
    - `updatePolicy.minReplicas`: the number of running replicas a workload needs before the updater evicts one of
      its pods; the updater's default is 2. Set it to `1` to resize single-replica workloads. It is only used with
      `Recreate` and `InPlaceOrRecreate`.
    - `resourcePolicy.containerPolicies`: the containers to autoscale, each with optional `minAllowed` and
      `maxAllowed` for `cpu` and `memory`, merged by container name and by resource. Quantities are strings, so
      write `cpu: "2"` rather than `cpu: 2`.

Autoscaling is opt-in at two levels:

- A container is autoscaled only when a matching profile names it in `containerPolicies`. Every other container, such
  as a sidecar, keeps the requests of its pod template.
- A workload gets a VPA only when at least one of its containers is autoscaled. A workload whose entries only set
  `updatePolicy` is listed in the HostedCluster condition and gets no VPA.

The generated VPAs differ from VPA defaults in these ways:

| Setting | VPA default | Here |
| --- | --- | --- |
| `updateMode` | `Recreate` | `Off` unless a profile sets it |
| Containers that no policy names (`"*"`) | autoscaled | `mode: Off` |
| `controlledValues` | `RequestsAndLimits` | `RequestsOnly`: limits keep their template value |
| Evicting modes on `controllerAvailabilityPolicy: SingleReplica` | applied | applied as `Initial`, because an eviction would mean API downtime |

Both cpu and memory are autoscaled, by the default recommender. When profiles combine into a container minimum above its
maximum, the minimum is capped at the maximum and the cap is listed in the HostedCluster condition. A workload that
already has a VPA not created by HyperShift is skipped, since two VPAs on one pod conflict, and the HostedCluster
condition reports `VPAConflict` until that VPA is deleted.

## Emergency controls

### Raise a container's minimum on one cluster

The existing `resource-request-override.hypershift.openshift.io/<deployment>.<container>` HostedCluster annotation, for
example `memory=12Gi,cpu=2`, sets the container's request in the workload template. On an autoscaled container it also
becomes the container's minimum (`minAllowed`): the larger of the annotation and the configured minimum, capped at
`maxAllowed`. Autoscaling stays on above that minimum. On a container that isn't autoscaled, the annotation only sets
the request.

Each entry of the annotation is used on its own, as the control-plane operator does for the template: in
`memory=12Gi,cpu=2c`, the memory minimum applies and the invalid `cpu` entry is ignored and listed in the HostedCluster
condition. When cpu or memory is set more than once, the last entry wins. Changing the annotation
updates the workload template, so the workload's pods are restarted once, one at a time within their disruption
budget.

### Pause autoscaling on one cluster

Define the pause ahead of time in the configuration, as the last profile: it selects HostedClusters with a pause label
and changes the update mode of every workload. To pause a cluster, label its HostedCluster
`control-plane-autoscaling-paused: "true"`; remove the label to resume.

The VPAs stay in place in both cases and keep computing recommendations, so the effect of resuming is visible in
advance. Choose the update mode by what pods that restart during the pause should get:

| `updateMode` | Running pods | Pods created during the pause | Use when |
| --- | --- | --- | --- |
| `Initial` | unchanged | the current recommendation | resizing is disruptive, but the recommendations are right |
| `Off` | unchanged | the requests of the pod template | the recommendations themselves are wrong |

With `Initial`, a floor annotation still raises a container's minimum. With `Off`, the floor annotation is the way to
keep a container above its template request.

Pausing the HostedCluster's reconciliation with `spec.pausedUntil` stops the HyperShift operator from changing the
VPAs, but it doesn't stop the VPA from resizing pods; use the pause profile for that.

## Status

Each HostedCluster selected by the configuration's `hostedClusters` reports a `ControlPlaneVerticalAutoscaling`
condition. HostedClusters that aren't selected have no such condition.

| Status | Reason | Meaning |
| --- | --- | --- |
| True | `AsExpected` | Every autoscaled workload that exists has a VPA, and none has gone 30 minutes without a recommendation. The message lists the matching profiles, autoscaled workloads, workloads waiting for a first recommendation, active floors, and workloads or containers that were not found. |
| False | `ClusterSizingActive` | The HostedCluster is sized by `ClusterSizingConfiguration`. |
| False | `VPANotInstalled` | The VPA API isn't served on the management cluster. |
| False | `RecommendationMissing` | A VPA has had no recommendation for 30 minutes, for example because the recommender isn't running. A workload without running pods, such as a Deployment scaled to zero, is listed in the message instead, because no recommendation is expected. |
| False | `VPAConflict` | A VPA not created by HyperShift targets an autoscaled workload, so that workload isn't autoscaled. The other workloads still are. Delete the other VPA, or stop naming the workload in the configuration. |
| False | `VPARejected` | The API server rejected a VPA, for example because the installed VPA doesn't support `InPlaceOrRecreate`. Use `Recreate` instead. |

When several apply, the reason is the first of `VPARejected`, `VPAConflict` and `RecommendationMissing`.

The configuration itself has no status; each HostedCluster's condition is the result. For a fleet view, the
`hypershift_control_plane_vertical_autoscaling_condition` metric is 1 for the current status and reason of each selected
HostedCluster's condition, with labels `namespace`, `name`, `status` and `reason`. For example,
`sum by (reason) (hypershift_control_plane_vertical_autoscaling_condition)` counts clusters per reason, and
`hypershift_control_plane_vertical_autoscaling_condition{status="False"} == 1` lists the clusters that need attention.

The `hypershift_control_plane_vertical_autoscaling_workloads` metric counts autoscaled workloads per HostedCluster, by update
mode. `hypershift_control_plane_vertical_autoscaling_profile` is 1 for each profile that matches a HostedCluster, for example
to list the clusters in a canary. Each VPA's `hypershift.openshift.io/control-plane-vertical-autoscaling-profiles` annotation
lists the profiles that set its workload, in order, and the VPA itself shows the merged settings.

## Support procedures

- **Is a cluster autoscaled?** Check the `ControlPlaneVerticalAutoscaling` condition and
  `oc get vpa -n <control plane namespace> -l hypershift.openshift.io/control-plane-vertical-autoscaling-managed=true`.
- **A workload is repeatedly OOM-killed.** Set a floor on the container, for example
  `resource-request-override.hypershift.openshift.io/kube-apiserver.kube-apiserver: memory=12Gi`, then fix the
  configured `minAllowed` and remove the annotation.
- **Autoscaling makes things worse on one cluster.** [Pause autoscaling](#pause-autoscaling-on-one-cluster) on that
  cluster with its pause label.
- **Pods are Pending after a resize.** The recommendation exceeds node capacity; lower `maxAllowed`.
- **Disable the feature for the fleet.** Delete the configuration, or remove `hostedClusters`. VPAs and the
  HostedCluster conditions are removed, and pods return to their template resources the next time they restart.
  Disabling the `ControlPlaneVerticalAutoscaling` feature gate instead stops the controller without deleting the VPAs
  or the conditions, so delete the configuration first.
