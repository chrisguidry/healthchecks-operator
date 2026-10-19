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
// holds a read-write API key for the project, the default integrations
// for the project's checks, and when to hold the checks.
type ClusterProjectSpec struct {
	URL          string       `json:"url"`
	APIKeySecret SecretKeyRef `json:"apiKeySecret"`
	Channels     []string     `json:"channels,omitempty"`
	Hold         *Hold        `json:"hold,omitempty"`
}

// Hold pauses the project's checks while another object's condition
// matches, such as a rollout that reboots every node, and resumes them
// when it stops matching or when MaxDuration passes.
type Hold struct {
	While HoldTarget `json:"while"`
	// MaxDuration is the longest a hold lasts, a Go duration string.
	MaxDuration string `json:"maxDuration"`
}

// HoldTarget is one object, named by its resource the way an RBAC rule
// names it, and the condition on it that holds the checks.
type HoldTarget struct {
	APIVersion string `json:"apiVersion"`
	Resource   string `json:"resource"`
	// Namespace is empty for a cluster-scoped object.
	Namespace string        `json:"namespace,omitempty"`
	Name      string        `json:"name"`
	Condition HoldCondition `json:"condition"`
}

// HoldCondition matches one condition in the object's
// status.conditions. An empty Reason matches any reason.
type HoldCondition struct {
	Type   string          `json:"type"`
	Status ConditionStatus `json:"status"`
	Reason string          `json:"reason,omitempty"`
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
