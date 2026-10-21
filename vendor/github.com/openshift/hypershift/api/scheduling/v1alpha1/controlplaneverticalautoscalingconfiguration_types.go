package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ControlPlaneVerticalAutoscalingConfigurationName is the only name honoured for a ControlPlaneVerticalAutoscalingConfiguration.
	ControlPlaneVerticalAutoscalingConfigurationName = "cluster"
)

// UpdateMode controls when the Vertical Pod Autoscaler applies its recommendations to a workload's pods.
// It is the Vertical Pod Autoscaler's updatePolicy.updateMode, without the deprecated "Auto" mode and without
// the "InPlace" mode, which requires a Vertical Pod Autoscaler feature gate.
// +kubebuilder:validation:Enum=Off;Initial;Recreate;InPlaceOrRecreate
type UpdateMode string

const (
	// UpdateModeOff computes recommendations without applying them.
	UpdateModeOff UpdateMode = "Off"
	// UpdateModeInitial applies recommendations only when a pod is created.
	UpdateModeInitial UpdateMode = "Initial"
	// UpdateModeRecreate applies recommendations when a pod is created, and evicts running pods whose
	// requests differ significantly from the recommendation.
	UpdateModeRecreate UpdateMode = "Recreate"
	// UpdateModeInPlaceOrRecreate applies recommendations when a pod is created, and resizes running pods in
	// place, evicting them only when an in-place resize is not possible.
	UpdateModeInPlaceOrRecreate UpdateMode = "InPlaceOrRecreate"
)

// WorkloadResource is the resource of a control plane workload.
// +kubebuilder:validation:Enum=deployments;statefulsets
type WorkloadResource string

const (
	// WorkloadResourceDeployments is an apps/v1 Deployment.
	WorkloadResourceDeployments WorkloadResource = "deployments"
	// WorkloadResourceStatefulSets is an apps/v1 StatefulSet.
	WorkloadResourceStatefulSets WorkloadResource = "statefulsets"
)

// HostedClusterSelectionType is the way HostedClusters are selected.
// +kubebuilder:validation:Enum=All;Selector
type HostedClusterSelectionType string

const (
	// HostedClusterSelectionAll selects every HostedCluster.
	HostedClusterSelectionAll HostedClusterSelectionType = "All"
	// HostedClusterSelectionSelector selects the HostedClusters that match a label selector.
	HostedClusterSelectionSelector HostedClusterSelectionType = "Selector"
)

// LabelSelectorOperator is the set of operators that can be used in a label selector requirement.
// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist
type LabelSelectorOperator string

const (
	// LabelSelectorOpIn matches when the label value is one of the listed values.
	LabelSelectorOpIn LabelSelectorOperator = "In"
	// LabelSelectorOpNotIn matches when the label is absent or its value is not one of the listed values.
	LabelSelectorOpNotIn LabelSelectorOperator = "NotIn"
	// LabelSelectorOpExists matches when the label is present.
	LabelSelectorOpExists LabelSelectorOperator = "Exists"
	// LabelSelectorOpDoesNotExist matches when the label is absent.
	LabelSelectorOpDoesNotExist LabelSelectorOperator = "DoesNotExist"
)

// Extending this API: every field mirrors a Vertical Pod Autoscaler field, under the VPA's name and in the VPA's
// place, and is optional, because profiles merge field by field: a field that a profile sets replaces the earlier
// value, and a field it leaves unset inherits it. So a new field must be omittable; a field whose zero value is a
// valid setting must be a pointer; a list replaces the earlier list as a whole; and when no profile sets the new
// field, the controller must keep today's behaviour, which the type documentation below states as the defaults.

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:path=controlplaneverticalautoscalingconfigurations,shortName=cpvac,scope=Cluster
// +kubebuilder:metadata:annotations="release.openshift.io/feature-set=TechPreviewNoUpgrade"
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="exactly one configuration may exist and must be named 'cluster'"

// ControlPlaneVerticalAutoscalingConfiguration configures vertical autoscaling of hosted control plane workloads
// with the Vertical Pod Autoscaler. The HyperShift operator creates one VerticalPodAutoscaler per autoscaled workload
// in the control plane namespace of every selected HostedCluster.
//
// It is an ordered list of profiles. Each profile selects HostedClusters by label and sets Vertical Pod Autoscaler
// settings for their workloads, with the field names and structure of a VerticalPodAutoscaler spec. Every profile
// that selects a HostedCluster applies, in list order, and a field that a later profile sets replaces the earlier
// value.
//
// Vertical autoscaling is opt-in: a container is autoscaled only when a profile that selects the HostedCluster
// names it in a containerPolicies entry, and a workload has a VerticalPodAutoscaler only when at least one of its
// containers is autoscaled. Every other container keeps the requests of its pod template.
//
// The VerticalPodAutoscalers differ from the Vertical Pod Autoscaler's defaults in these ways:
// updateMode defaults to "Off" rather than "Recreate"; only requests are set, never limits ("RequestsOnly");
// containers that no profile names are not autoscaled; and on HostedClusters with a SingleReplica controller
// availability policy, "Recreate" and "InPlaceOrRecreate" are applied as "Initial".
// Both cpu and memory are autoscaled, by the default recommender.
//
// HostedClusters sized by ClusterSizingConfiguration are never autoscaled.
//
// The configuration has no status. Each selected HostedCluster reports the result in its
// ControlPlaneVerticalAutoscaling condition.
type ControlPlaneVerticalAutoscalingConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object's metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired autoscaling configuration.
	// +required
	Spec ControlPlaneVerticalAutoscalingConfigurationSpec `json:"spec,omitzero"`
}

// ControlPlaneVerticalAutoscalingConfigurationSpec selects the HostedClusters to autoscale and defines the profiles
// that set their autoscaling.
type ControlPlaneVerticalAutoscalingConfigurationSpec struct {
	// hostedClusters selects the HostedClusters this configuration applies to. Profiles only apply to
	// HostedClusters selected here, so a profile's selector alone never opts a HostedCluster into autoscaling.
	// When omitted, no HostedCluster is selected and no VerticalPodAutoscalers are created.
	// HostedClusters with the hypershift.openshift.io/hosted-cluster-size label, or with the
	// hypershift.openshift.io/resource-based-cp-auto-scaling annotation set to "true", are sized by
	// ClusterSizingConfiguration and are never autoscaled, regardless of this field.
	// +optional
	HostedClusters HostedClusterSelection `json:"hostedClusters,omitzero"`

	// profiles set the autoscaling of control plane workloads on the HostedClusters they select.
	// Every profile that selects a HostedCluster applies to it, in list order. For each workload, a profile's
	// allWorkloads applies first and then its workloads entry for that workload. Every field that is set
	// replaces the value from earlier profiles; a later profile cannot unset a field.
	// List the profile that applies to every HostedCluster first, and profiles that must win, such as one that
	// pauses autoscaling, last.
	// It must contain between 1 and 16 entries, and each entry's name must be unique.
	// +required
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(p, self.exists_one(q, q.name == p.name))",message="profile names must be unique"
	Profiles []AutoscalingProfile `json:"profiles,omitempty"`
}

// HostedClusterSelection selects HostedClusters, either all of them or those matching a label selector.
// +kubebuilder:validation:XValidation:rule="self.type == 'Selector' ? has(self.selector) : !has(self.selector)",message="selector is required when type is Selector, and forbidden otherwise"
// +union
type HostedClusterSelection struct {
	// type is the way HostedClusters are selected.
	// Valid values are:
	// - "All": selects every HostedCluster. selector must not be set.
	// - "Selector": selects the HostedClusters whose labels match selector. selector must be set.
	// +required
	// +unionDiscriminator
	Type HostedClusterSelectionType `json:"type,omitempty"`

	// selector selects HostedClusters by label.
	// It is required when type is "Selector", and must not be set otherwise.
	// +optional
	// +unionMember
	Selector HostedClusterSelector `json:"selector,omitzero"`
}

// HostedClusterSelector selects HostedClusters by their labels.
// It has the semantics and serialization of a Kubernetes label selector, but must not be empty: at least one of
// matchLabels and matchExpressions must be set. When both are set, a HostedCluster must satisfy both to match.
// +kubebuilder:validation:MinProperties=1
type HostedClusterSelector struct {
	// matchLabels is a map of label keys and values. A HostedCluster matches when it has every listed label with
	// the listed value.
	// When set, it must contain between 1 and 16 entries.
	// Each key must be a valid Kubernetes label key: an optional prefix and a name, separated by "/". The prefix,
	// when present, must be a DNS-1123 subdomain of at most 253 characters, with each "."-separated segment at most
	// 63 characters. The name must be 1 to 63 characters, consist of alphanumeric characters, "-", "_" or ".", and
	// start and end with an alphanumeric character.
	// Each value must be a non-empty Kubernetes label value: 1 to 63 characters consisting of alphanumeric
	// characters, "-", "_" or ".", starting and ending with an alphanumeric character. Unlike a Kubernetes label
	// selector, a label with an empty value can't be matched by value; use matchExpressions with Exists instead.
	// +optional
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$') && (!k.contains('/') || (k.indexOf('/') <= 253 && k.split('/')[0].split('.').all(s, s.size() <= 63))))",message="matchLabels keys must be valid label keys"
	MatchLabels map[string]LabelValue `json:"matchLabels,omitempty"`

	// matchExpressions is a list of label selector requirements. A HostedCluster matches when it satisfies every
	// requirement.
	// When set, it must contain between 1 and 16 entries. Several requirements may apply to the same key, as in a
	// Kubernetes label selector.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions,omitempty"`
}

// LabelValue is a non-empty Kubernetes label value: 1 to 63 characters consisting of alphanumeric characters,
// "-", "_" or ".", starting and ending with an alphanumeric character.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:XValidation:rule="self.matches('^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$')",message="label values must consist of alphanumeric characters, '-', '_' or '.', and start and end with an alphanumeric character"
type LabelValue string

// LabelSelectorRequirement is a selector requirement on a single label.
// +kubebuilder:validation:XValidation:rule="self.operator in ['In', 'NotIn'] ? has(self.values) : !has(self.values)",message="values must be set for In and NotIn, and must not be set for Exists and DoesNotExist"
type LabelSelectorRequirement struct {
	// key is the label key that the requirement applies to.
	// It must be a valid Kubernetes label key of at most 317 characters: an optional prefix and a name, separated
	// by "/". The prefix, when present, must be a DNS-1123 subdomain of at most 253 characters, with each
	// "."-separated segment at most 63 characters. The name must be 1 to 63 characters, consist of alphanumeric
	// characters, "-", "_" or ".", and start and end with an alphanumeric character.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	// +kubebuilder:validation:XValidation:rule="self.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$') && (!self.contains('/') || (self.indexOf('/') <= 253 && self.split('/')[0].split('.').all(s, s.size() <= 63)))",message="key must be a valid label key"
	Key string `json:"key,omitempty"`

	// operator is the key's relationship to values.
	// Valid values are:
	// - "In": the label must be present, with a value in values.
	// - "NotIn": the label must be absent, or have a value not in values.
	// - "Exists": the label must be present, with any value.
	// - "DoesNotExist": the label must be absent.
	// +required
	Operator LabelSelectorOperator `json:"operator,omitempty"`

	// values is the list of label values for the In and NotIn operators.
	// It is required when operator is "In" or "NotIn", and must not be set when operator is "Exists" or
	// "DoesNotExist".
	// When set, it must contain between 1 and 64 unique entries, each a non-empty Kubernetes label value: 1 to 63
	// characters consisting of alphanumeric characters, "-", "_" or ".", starting and ending with an alphanumeric
	// character.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Values []LabelValue `json:"values,omitempty"`
}

// AutoscalingProfile sets Vertical Pod Autoscaler settings for control plane workloads on the HostedClusters it
// selects.
// +kubebuilder:validation:XValidation:rule="has(self.allWorkloads) || has(self.workloads)",message="a profile must set allWorkloads or workloads"
type AutoscalingProfile struct {
	// name identifies the profile in the HostedCluster's ControlPlaneVerticalAutoscaling condition, in metrics,
	// and in the annotation that lists a VerticalPodAutoscaler's profiles.
	// It must be a DNS-1123 label: 1 to 63 characters, consisting of lowercase alphanumeric characters and "-",
	// and starting and ending with an alphanumeric character.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS-1123 label: lowercase alphanumeric characters or '-', starting and ending with an alphanumeric character"
	Name string `json:"name,omitempty"`

	// hostedClusters selects the HostedClusters this profile applies to, among those selected by
	// spec.hostedClusters. Use type "All" for a profile that applies to every selected HostedCluster.
	// +required
	HostedClusters HostedClusterSelection `json:"hostedClusters,omitzero"`

	// allWorkloads sets Vertical Pod Autoscaler settings for every autoscaled workload of the selected
	// HostedClusters, including workloads named only in other profiles. Within this profile, a workloads entry
	// replaces these settings for its workload. allWorkloads never makes a workload autoscaled.
	// For example, a profile that is listed last and sets allWorkloads.updatePolicy.updateMode to "Initial"
	// stops resizing running pods on the HostedClusters it selects.
	// When set, it must set at least one field.
	// When omitted, the profile only sets the workloads it lists.
	// +optional
	AllWorkloads AllWorkloadsAutoscaling `json:"allWorkloads,omitzero"`

	// workloads sets Vertical Pod Autoscaler settings for individual workloads.
	// When set, it must contain between 1 and 128 entries, and each entry's combination of resource and name must
	// be unique.
	// When omitted, the profile only applies allWorkloads.
	// +optional
	// +listType=map
	// +listMapKey=resource
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	Workloads []WorkloadAutoscaling `json:"workloads,omitempty"`
}

// AllWorkloadsAutoscaling are Vertical Pod Autoscaler settings for every autoscaled workload of a HostedCluster.
// +kubebuilder:validation:MinProperties=1
type AllWorkloadsAutoscaling struct {
	// updatePolicy sets the Vertical Pod Autoscaler's spec.updatePolicy for every autoscaled workload.
	// +optional
	UpdatePolicy PodUpdatePolicy `json:"updatePolicy,omitzero"`
}

// WorkloadAutoscaling sets Vertical Pod Autoscaler settings for one control plane workload.
// +kubebuilder:validation:XValidation:rule="has(self.updatePolicy) || has(self.resourcePolicy)",message="a workload must set updatePolicy or resourcePolicy"
type WorkloadAutoscaling struct {
	// resource is the resource of the workload, which becomes the kind of the VerticalPodAutoscaler's
	// spec.targetRef.
	// Valid values are:
	// - "deployments": an apps/v1 Deployment.
	// - "statefulsets": an apps/v1 StatefulSet.
	// +required
	Resource WorkloadResource `json:"resource,omitempty"`

	// name is the name of the workload in the control plane namespace, for example "kube-apiserver", and becomes
	// the name of the VerticalPodAutoscaler's spec.targetRef.
	// It must be a DNS-1123 label: 1 to 63 characters, consisting of lowercase alphanumeric characters and "-",
	// and starting and ending with an alphanumeric character.
	// It is not checked against a fixed list, because control plane workloads differ between releases and
	// platforms. A workload that doesn't exist on a given HostedCluster is skipped and reported in that
	// HostedCluster's ControlPlaneVerticalAutoscaling condition.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS-1123 label: lowercase alphanumeric characters or '-', starting and ending with an alphanumeric character"
	Name string `json:"name,omitempty"`

	// updatePolicy sets the Vertical Pod Autoscaler's spec.updatePolicy for the workload. Each field that is set
	// replaces this profile's allWorkloads and earlier profiles' value.
	// +optional
	UpdatePolicy PodUpdatePolicy `json:"updatePolicy,omitzero"`

	// resourcePolicy sets the Vertical Pod Autoscaler's spec.resourcePolicy for the workload. Naming a container
	// in containerPolicies makes it autoscaled.
	// +optional
	ResourcePolicy PodResourcePolicy `json:"resourcePolicy,omitzero"`
}

// PodUpdatePolicy is the Vertical Pod Autoscaler's spec.updatePolicy: when and how recommendations are applied to
// a workload's pods.
// +kubebuilder:validation:MinProperties=1
type PodUpdatePolicy struct {
	// updateMode controls when recommendations are applied to pods.
	// Valid values are:
	// - "Off": recommendations are computed but never applied.
	// - "Initial": recommendations are applied only when a pod is created; running pods are never changed.
	// - "Recreate": recommendations are applied when a pod is created, and running pods whose requests differ
	//   significantly from the recommendation are evicted.
	// - "InPlaceOrRecreate": running pods are resized in place, and are evicted only when an in-place resize is
	//   not possible. This requires a Vertical Pod Autoscaler and Kubernetes version that support in-place pod
	//   resize; otherwise the VerticalPodAutoscaler is rejected, which is reported with reason VPARejected in the
	//   HostedCluster's ControlPlaneVerticalAutoscaling condition.
	// On HostedClusters with a SingleReplica controller availability policy, "Recreate" and "InPlaceOrRecreate"
	// are applied as "Initial", because an eviction would mean API downtime.
	// When no profile sets it, "Off" is used, unlike the Vertical Pod Autoscaler's default of "Recreate", and the
	// workload is reported in the HostedCluster's ControlPlaneVerticalAutoscaling condition.
	// +optional
	UpdateMode UpdateMode `json:"updateMode,omitempty"`

	// minReplicas is the minimum number of running replicas a workload must have for the Vertical Pod Autoscaler
	// updater to evict one of its pods. Set it to 1 to let the updater resize single-replica workloads.
	// It is only used when the resulting updateMode is "Recreate" or "InPlaceOrRecreate".
	// It must be at least 1.
	// When no profile sets it, the updater's --min-replicas flag is used, which defaults to 2.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas,omitempty"`
}

// PodResourcePolicy is the Vertical Pod Autoscaler's spec.resourcePolicy: which containers are autoscaled, and
// within which bounds.
type PodResourcePolicy struct {
	// containerPolicies are the policies of the workload's autoscaled containers, merged by containerName with
	// those of earlier profiles. Containers that no profile names keep the requests of their pod template.
	// It must contain between 1 and 16 entries, and each entry's containerName must be unique.
	// +required
	// +listType=map
	// +listMapKey=containerName
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	ContainerPolicies []ContainerResourcePolicy `json:"containerPolicies,omitempty"`
}

// ContainerResourcePolicy is a Vertical Pod Autoscaler container policy: it autoscales one container, within
// optional bounds.
// +kubebuilder:validation:XValidation:rule="!has(self.minAllowed) || !has(self.maxAllowed) || !has(self.minAllowed.cpu) || !has(self.maxAllowed.cpu) || !isQuantity(self.minAllowed.cpu) || !isQuantity(self.maxAllowed.cpu) || quantity(self.minAllowed.cpu).compareTo(quantity(self.maxAllowed.cpu)) <= 0",message="minAllowed.cpu must not exceed maxAllowed.cpu"
// +kubebuilder:validation:XValidation:rule="!has(self.minAllowed) || !has(self.maxAllowed) || !has(self.minAllowed.memory) || !has(self.maxAllowed.memory) || !isQuantity(self.minAllowed.memory) || !isQuantity(self.maxAllowed.memory) || quantity(self.minAllowed.memory).compareTo(quantity(self.maxAllowed.memory)) <= 0",message="minAllowed.memory must not exceed maxAllowed.memory"
type ContainerResourcePolicy struct {
	// containerName is the name of the container in the workload's pod template.
	// It must be a DNS-1123 label: 1 to 63 characters, consisting of lowercase alphanumeric characters and "-",
	// and starting and ending with an alphanumeric character. The Vertical Pod Autoscaler's "*" is not
	// accepted: every autoscaled container is named.
	// It is not checked at admission, because containers differ between releases and platforms. A container that
	// the workload's pod template doesn't have is skipped and reported in the HostedCluster's
	// ControlPlaneVerticalAutoscaling condition.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="containerName must be a DNS-1123 label: lowercase alphanumeric characters or '-', starting and ending with an alphanumeric character"
	ContainerName string `json:"containerName,omitempty"`

	// minAllowed is the lower bound for the container's recommended requests. cpu and memory are merged with
	// earlier profiles separately.
	// For each resource set in both minAllowed and maxAllowed of this entry, minAllowed must not exceed
	// maxAllowed. When profiles combine into a minimum above the maximum, the minimum is capped at the maximum and
	// reported in the HostedCluster's ControlPlaneVerticalAutoscaling condition.
	// When no profile sets it, recommendations have no lower bound.
	// +optional
	MinAllowed ResourceBounds `json:"minAllowed,omitzero"`

	// maxAllowed is the upper bound for the container's recommended requests. Set it so that the workload's pods
	// still fit on the largest node available to the control plane. cpu and memory are merged with earlier
	// profiles separately.
	// When no profile sets it, recommendations have no upper bound.
	// +optional
	MaxAllowed ResourceBounds `json:"maxAllowed,omitzero"`
}

// ResourceBounds is a bound on a container's cpu and memory requests, serialized like the Vertical Pod
// Autoscaler's minAllowed and maxAllowed resource lists, except that quantities are always strings.
// At least one of cpu and memory must be set.
// +kubebuilder:validation:MinProperties=1
type ResourceBounds struct {
	// cpu is the CPU bound, as a Kubernetes resource quantity string greater than zero, for example "500m"
	// (half a core) or "2" (two cores). It must be 1 to 32 characters.
	// When omitted, CPU is not bounded.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="isQuantity(self) && quantity(self).isGreaterThan(quantity('0'))",message="cpu must be a valid quantity greater than zero"
	CPU string `json:"cpu,omitempty"`

	// memory is the memory bound, as a Kubernetes resource quantity string greater than zero, for example
	// "512Mi" or "4Gi". It must be 1 to 32 characters.
	// When omitted, memory is not bounded.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="isQuantity(self) && quantity(self).isGreaterThan(quantity('0'))",message="memory must be a valid quantity greater than zero"
	Memory string `json:"memory,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneVerticalAutoscalingConfigurationList contains a list of ControlPlaneVerticalAutoscalingConfiguration.
type ControlPlaneVerticalAutoscalingConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard list's metadata.
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	// items is the list of configurations.
	Items []ControlPlaneVerticalAutoscalingConfiguration `json:"items"`
}
