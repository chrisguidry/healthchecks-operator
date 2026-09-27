package main

// A ClusterProject is one project on one Healthchecks instance. It is
// cluster-scoped: every Healthchecks v3 API key belongs to exactly one
// project, and there are no account-wide keys, so the project is the
// unit the operator talks to.
type ClusterProject struct {
	APIVersion string               `json:"apiVersion,omitempty"`
	Kind       string               `json:"kind,omitempty"`
	Metadata   ObjectMeta           `json:"metadata"`
	Spec       ClusterProjectSpec   `json:"spec"`
	Status     ClusterProjectStatus `json:"status,omitempty"`
}

type ClusterProjectList struct {
	Metadata ListMeta         `json:"metadata"`
	Items    []ClusterProject `json:"items"`
}

// ClusterProjectSpec names the Healthchecks instance, the Secret that
// holds a read-write API key for the project, and the default
// integrations for the project's checks.
type ClusterProjectSpec struct {
	URL          string       `json:"url"`
	APIKeySecret SecretKeyRef `json:"apiKeySecret"`
	Channels     []string     `json:"channels,omitempty"`
}

// ClusterProjectStatus is the channels the operator resolved for this
// project, and whether the project is ready. Only the operator writes
// it.
type ClusterProjectStatus struct {
	Channels   []ResolvedChannel `json:"channels,omitempty"`
	Conditions []Condition       `json:"conditions,omitempty"`
}

// ResolvedChannel is one name from spec.channels, resolved to the ID
// Healthchecks reports for it.
type ResolvedChannel struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}
