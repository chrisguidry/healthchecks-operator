package main

import "slices"

// The wire types are hand-written, the way the liken-sh operators write
// theirs. The Kubernetes API is HTTPS that serves JSON, and importing
// client-go for a dozen structs brings informers, work queues, and a
// release cadence this program does not use. Each type carries only the
// fields this operator reads or writes; the API server fills in the
// rest.

// ObjectMeta carries what this operator reads or writes: name and
// namespace for the URL, uid for the identity of one object,
// resourceVersion for the conditional write, and the finalizers and
// deletionTimestamp that say whether the object is being deleted. A
// ClusterProject has no namespace of its own, so its Namespace is
// always empty.
type ObjectMeta struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// UID tells an object from an earlier one of the same name, which a
	// delete and a create leave behind.
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Generation      int64             `json:"generation,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Finalizers      []string          `json:"finalizers,omitempty"`
	// DeletionTimestamp is set once a delete arrives while finalizers
	// remain. The object stays until the last finalizer is removed.
	DeletionTimestamp string `json:"deletionTimestamp,omitempty"`
}

// holds reports whether the object carries the finalizer.
func (m ObjectMeta) holds(finalizer string) bool {
	return slices.Contains(m.Finalizers, finalizer)
}

// withFinalizer and withoutFinalizer return a new list, so a patch that
// fails leaves the caller's copy of the object alone. Every other
// finalizer on the object, another controller's included, stays.
func (m ObjectMeta) withFinalizer(finalizer string) []string {
	return append(slices.Clone(m.Finalizers), finalizer)
}

func (m ObjectMeta) withoutFinalizer(finalizer string) []string {
	var kept []string
	for _, held := range m.Finalizers {
		if held != finalizer {
			kept = append(kept, held)
		}
	}
	return kept
}

// A list's own resourceVersion is the revision of the whole collection,
// which is what a watch resumes from.
type ListMeta struct {
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// SecretKeyRef names one key in a Secret. spec.apiKeySecret on a
// ClusterProject and a header's valueFrom.secretKeyRef on a Check both
// use it: both name a key in a Secret and nothing else.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// A Check is one Healthchecks check and the probe that feeds it. It is
// namespaced, because the workload it watches has a namespace.
type Check struct {
	APIVersion string      `json:"apiVersion,omitempty"`
	Kind       string      `json:"kind,omitempty"`
	Metadata   ObjectMeta  `json:"metadata"`
	Spec       CheckSpec   `json:"spec"`
	Status     CheckStatus `json:"status,omitempty"`
}

type CheckList struct {
	Metadata ListMeta `json:"metadata"`
	Items    []Check  `json:"items"`
}

// EffectiveSlug is the slug the operator sends to Healthchecks. An
// explicit spec.slug wins; otherwise the namespace and name make a slug
// unique across the cluster.
func (c *Check) EffectiveSlug() string {
	if c.Spec.Slug != "" {
		return c.Spec.Slug
	}
	return c.Metadata.Namespace + "-" + c.Metadata.Name
}

// EffectiveDisplayName is the name the operator sends to Healthchecks.
// An explicit spec.displayName wins; otherwise the namespace and name
// read the way a person points at the workload in kubectl.
func (c *Check) EffectiveDisplayName() string {
	if c.Spec.DisplayName != "" {
		return c.Spec.DisplayName
	}
	return c.Metadata.Namespace + "/" + c.Metadata.Name
}

// ProjectRef names the project a Check belongs to. Kind has one legal
// value today, ClusterProject, checked by a CEL rule rather than an
// enum so that a later Project kind is one rule edit, not a schema
// change to this field's type.
type ProjectRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// CheckSpec names the project, the check's identity and defaults on
// Healthchecks, and the one probe that feeds it. A CEL rule holds a
// Check to exactly one of HTTP, TLS, and CronJob; ProbeKind reads
// whichever one is set.
type CheckSpec struct {
	ProjectRef  ProjectRef `json:"projectRef"`
	DisplayName string     `json:"displayName,omitempty"`
	Slug        string     `json:"slug,omitempty"`
	Description string     `json:"description,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
	// Grace is a Go duration string, the check's grace period on
	// Healthchecks.
	Grace    string        `json:"grace,omitempty"`
	Channels []string      `json:"channels,omitempty"`
	HTTP     *HTTPProbe    `json:"http,omitempty"`
	TLS      *TLSProbe     `json:"tls,omitempty"`
	CronJob  *CronJobProbe `json:"cronJob,omitempty"`
}

// ProbeKind names which probe block a CheckSpec carries.
type ProbeKind int

const (
	ProbeKindNone ProbeKind = iota
	ProbeKindHTTP
	ProbeKindTLS
	ProbeKindCronJob
)

func (k ProbeKind) String() string {
	switch k {
	case ProbeKindHTTP:
		return "http"
	case ProbeKindTLS:
		return "tls"
	case ProbeKindCronJob:
		return "cronJob"
	default:
		return "none"
	}
}

// ProbeKind answers which probe block is set, so a caller switches on
// one typed value instead of chaining nil checks across HTTP, TLS, and
// CronJob.
func (s CheckSpec) ProbeKind() ProbeKind {
	switch {
	case s.HTTP != nil:
		return ProbeKindHTTP
	case s.TLS != nil:
		return ProbeKindTLS
	case s.CronJob != nil:
		return ProbeKindCronJob
	default:
		return ProbeKindNone
	}
}

// HTTPProbe sends every request in Requests, in order, on every
// Interval. The check pings success only if every request meets its
// expectations. Requests do not follow redirects, so a redirect is
// something a request can expect.
type HTTPProbe struct {
	// Interval is a Go duration string, at least 1m, and the check's
	// timeout on Healthchecks.
	Interval string        `json:"interval"`
	Requests []HTTPRequest `json:"requests"`
}

// HTTPRequest is one request the probe sends, and what it expects back.
type HTTPRequest struct {
	URL string `json:"url"`
	// TLSVerify is a pointer because absent means true: the probe
	// verifies the server's certificate unless a request turns that off.
	TLSVerify *bool               `json:"tlsVerify,omitempty"`
	Headers   []RequestHeader     `json:"headers,omitempty"`
	Expect    ResponseExpectation `json:"expect,omitempty"`
}

// RequestHeader is one request header, in the shape of a container's
// env: a literal Value or a Secret read through ValueFrom, never both.
// Go's net/http client takes the Host header from the request's own
// Host field, not from its headers, so the probe copies a Host entry
// into that field, and Host behaves like any other header here.
type RequestHeader struct {
	Name      string           `json:"name"`
	Value     string           `json:"value,omitempty"`
	ValueFrom *HeaderValueFrom `json:"valueFrom,omitempty"`
}

// HeaderValueFrom reads a header's value from a Secret in the Check's
// own namespace.
type HeaderValueFrom struct {
	SecretKeyRef SecretKeyRef `json:"secretKeyRef"`
}

// ResponseExpectation is what a passing response looks like. An absent
// field does not filter on that trait.
type ResponseExpectation struct {
	Status       []int32           `json:"status,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	BodyContains string            `json:"bodyContains,omitempty"`
}

// TLSProbe connects to Host, completes a TLS handshake, and reads the
// leaf certificate's expiry. The check fails if the certificate does
// not verify, or if less than MinRemaining remains before it expires.
type TLSProbe struct {
	// Interval is a Go duration string, at least 1m, and the check's
	// timeout on Healthchecks.
	Interval string `json:"interval"`
	Host     string `json:"host"`
	// MinRemaining is a Go duration string, greater than zero.
	MinRemaining string `json:"minRemaining"`
}

// CronJobProbe reports each run of the named CronJob in the Check's own
// namespace, instead of running a probe of its own.
type CronJobProbe struct {
	Name string `json:"name"`
}

// CheckStatus is the check's identity on Healthchecks, and whether it
// is ready and passing. The Passing condition holds the result of the
// last probe: its message is the failure reason, and its
// lastTransitionTime is when the result changed. Only the operator
// writes it.
type CheckStatus struct {
	// Project is the ClusterProject that holds the check in status. A
	// changed projectRef deletes the check from this project before it
	// creates the check in the new one.
	Project         string      `json:"project,omitempty"`
	Slug            string      `json:"slug,omitempty"`
	UUID            string      `json:"uuid,omitempty"`
	PingURL         string      `json:"pingURL,omitempty"`
	LastReportedJob string      `json:"lastReportedJob,omitempty"`
	Conditions      []Condition `json:"conditions,omitempty"`
}
