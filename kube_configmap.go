package main

// A ping Check writes its ping URL into a ConfigMap that the workload
// reads. The operator watches only the ConfigMaps it labels as its
// own, so its memory holds those and not every ConfigMap in the
// cluster.

// configMapsResource is the core ConfigMap kind.
var configMapsResource = kubeResource{Version: "v1", Resource: "configmaps"}

// managedByLabel marks each ConfigMap the operator writes, and the
// ConfigMap watch selects on it.
const managedByLabel = "app.kubernetes.io/managed-by"

// managedBySelector selects the ConfigMaps the operator wrote.
const managedBySelector = managedByLabel + "=" + fieldManager

// configMap holds the fields of a ConfigMap that the operator reads:
// the data, and the owner references that say which Check controls it.
type configMap struct {
	Metadata   configMapMetadata `json:"metadata"`
	Data       map[string]string `json:"data,omitempty"`
	BinaryData map[string]string `json:"binaryData,omitempty"`
}

// keys counts the ConfigMap's keys, binary ones included.
func (m configMap) keys() int {
	return len(m.Data) + len(m.BinaryData)
}

type configMapMetadata struct {
	ObjectMeta
	OwnerReferences []ownerReference `json:"ownerReferences,omitempty"`
}

// controller returns the owner reference that controls the ConfigMap,
// and false when none does.
func (m configMap) controller() (ownerReference, bool) {
	for _, ref := range m.Metadata.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref, true
		}
	}
	return ownerReference{}, false
}

// controlledBy reports whether the object whose uid is uid is the
// ConfigMap's controller.
func (m configMap) controlledBy(uid string) bool {
	owner, controlled := m.controller()
	return controlled && owner.UID == uid
}

// configMapApply is the whole ConfigMap the operator applies: one key,
// the label, and the owner reference to the Check. The owner reference
// lets the garbage collector delete the ConfigMap when the Check goes.
// blockOwnerDeletion is false, so a Check's deletion never waits on the
// ConfigMap, and the operator needs no RBAC on the Check's finalizers
// subresource for it.
type configMapApply struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string                `json:"name"`
		Namespace       string                `json:"namespace"`
		Labels          map[string]string     `json:"labels"`
		OwnerReferences []ownerReferenceApply `json:"ownerReferences"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

type ownerReferenceApply struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         bool   `json:"controller"`
	BlockOwnerDeletion bool   `json:"blockOwnerDeletion"`
}

// pingConfigMap is the ConfigMap that holds a Check's ping URL under
// key.
func pingConfigMap(check *Check, name, key, pingURL string) configMapApply {
	apply := configMapApply{APIVersion: "v1", Kind: "ConfigMap", Data: map[string]string{key: pingURL}}
	apply.Metadata.Name = name
	apply.Metadata.Namespace = check.Metadata.Namespace
	apply.Metadata.Labels = map[string]string{managedByLabel: fieldManager}
	apply.Metadata.OwnerReferences = []ownerReferenceApply{{
		APIVersion: checksResource.Group + "/" + checksResource.Version,
		Kind:       checkKind,
		Name:       check.Metadata.Name,
		UID:        check.Metadata.UID,
		Controller: true,
	}}
	return apply
}

// emptyConfigMap names a ConfigMap and states no field of it. Applied,
// it gives up every field this field manager owns.
func emptyConfigMap(namespace, name string) any {
	type metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	return struct {
		APIVersion string   `json:"apiVersion"`
		Kind       string   `json:"kind"`
		Metadata   metadata `json:"metadata"`
	}{"v1", "ConfigMap", metadata{name, namespace}}
}
