package main

// A Check becomes one check in Healthchecks. Each pass builds the
// upsert request from the spec, and sends it only when it differs from
// the last request this process sent for the Check, or when status has
// no uuid. After a restart, the first pass sends one upsert for each
// Check, and Healthchecks finds the check by its slug.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// checkFinalizer keeps a Check until the operator has deleted its check
// in Healthchecks. Without it, a deleted Check would leave a check that
// goes down one grace period later and alerts for nothing.
const checkFinalizer = "healthchecks.guid.foo/check"

// The reasons a Check's Ready condition gives.
const (
	reasonSynced            = "Synced"
	reasonProjectNotFound   = "ProjectNotFound"
	reasonProjectNotReady   = "ProjectNotReady"
	reasonCronJobNotFound   = "CronJobNotFound"
	reasonCronJobHistory    = "CronJobHistory"
	reasonInvalidSpec       = "InvalidSpec"
	reasonUpsertFailed      = "UpsertFailed"
	reasonDeleteFailed      = "DeleteFailed"
	reasonFinalizerFailed   = "FinalizerFailed"
	reasonProbeInvalid      = "ProbeInvalid"
	reasonConfigMapFailed   = "ConfigMapFailed"
	reasonConfigMapConflict = "ConfigMapConflict"
)

// checkState is what the operator holds for one Check between passes.
// The pass and the probe schedule's report both write the Check's
// status, and each write states the whole status, so both work from
// status here under mutex.
type checkState struct {
	mutex      sync.Mutex
	uid        string
	namespace  string
	name       string
	generation int64
	// status is what the API server holds: what the operator last
	// wrote, or what the first pass read.
	status CheckStatus
	// sent is the last upsert request that succeeded, and via is the
	// project client it went through. Both are empty after a restart.
	sent *healthchecks.UpsertRequest
	via  *healthchecks.Client
	// probe names the probe spec registered with the schedule, or is
	// empty when none is.
	probe string
	// probeURL is the ping URL the registered probe reports to. When it
	// changes, the probe starts again at once.
	probeURL string
	// runs is where a cronJob check's reporting stands.
	runs cronJobRuns
	// deleted says the check is gone from Healthchecks, so a retried
	// finalizer removal does not delete it again.
	deleted bool
	// backoff spaces out the calls to Healthchecks after one fails.
	backoff backoff
}

func (s *checkState) subject() string {
	return "check " + s.namespace + "/" + s.name
}

// checkState returns the state for a Check, and starts a new one for a
// Check the operator has not seen, or for a new object under the name
// of an old one.
func (c *controller) checkState(check *Check) *checkState {
	key := objectKey(check.Metadata)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	held := c.checks[key]
	if held != nil && held.uid == check.Metadata.UID {
		return held
	}
	state := &checkState{
		uid:       check.Metadata.UID,
		namespace: check.Metadata.Namespace,
		name:      check.Metadata.Name,
		status:    check.Status,
		runs:      cronJobRuns{lastReportedJob: check.Status.LastReportedJob},
	}
	if held != nil {
		// The old object's prober stays registered under the same key,
		// so the new state names it, and the pass replaces or removes it.
		held.mutex.Lock()
		state.probe, state.probeURL = held.probe, held.probeURL
		held.mutex.Unlock()
	}
	c.checks[key] = state
	return state
}

// lookupCheck returns the state at key, or nil.
func (c *controller) lookupCheck(key string) *checkState {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.checks[key]
}

// forgetChecksExcept stops the probes of the Checks that are gone, and
// forgets them.
func (c *controller) forgetChecksExcept(live map[string]bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	for key, state := range c.checks {
		if !live[key] {
			c.schedule.remove(key)
			delete(c.checks, key)
			c.log.forget(state.subject())
		}
	}
}

// reconcileCheck brings one Check's check in Healthchecks, its probe,
// and its status up to its spec.
func (c *controller) reconcileCheck(ctx context.Context, check *Check, w *world) {
	state := c.checkState(check)
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.generation = check.Metadata.Generation
	if state.backoff.waiting(c.now()) {
		return
	}
	if check.Metadata.DeletionTimestamp != "" {
		c.releaseCheck(ctx, check, state)
		return
	}
	kind := check.Spec.ProbeKind()
	if kind != ProbeKindHTTP && kind != ProbeKindTLS {
		c.stopProbe(state)
	}

	project, v := c.projectFor(check)
	if v.ready() && !check.Metadata.holds(checkFinalizer) {
		err := c.client.setFinalizers(ctx, checksResource, state.namespace, state.name, check.Metadata.ResourceVersion, check.Metadata.withFinalizer(checkFinalizer), nil)
		if errors.Is(err, errConflict) || errors.Is(err, errNotFound) {
			// The Check changed or went since the store held it. The
			// change's event updates the store and wakes the next pass,
			// which reads the newer Check. Nothing retries before then.
			return
		}
		if err != nil {
			v = verdict{reasonFinalizerFailed, "adding the finalizer: " + errorText(err)}
		} else {
			c.log.printf("%s: added the finalizer %s", state.subject(), checkFinalizer)
		}
	}

	next := state.status
	if v.ready() {
		v = c.syncCheck(ctx, check, state, project, w, &next)
	}
	next.Conditions = withCondition(next.Conditions, checkReady(v, state.generation, c.now()))
	// Only a probe or a reported run sets Passing. The operator sees
	// none of a ping check's pings, so it has no Passing at all.
	if (kind == ProbeKindCronJob && !fromRun(next.Conditions)) || kind == ProbeKindPing {
		next.Conditions = withoutCondition(next.Conditions, passingCondition)
	}
	next.Probe = kind.String()
	next.LastReportedJob = ""
	if kind == ProbeKindCronJob {
		next.LastReportedJob = state.runs.lastReportedJob
	}
	if v.remote() {
		state.backoff.failed(c.now())
	}
	c.writeCheckStatus(ctx, state, next)
	c.note(state.subject(), checkKind, v)
}

// projectFor returns the Check's project when it is ready, or the
// verdict that names the project and says what is wrong with it.
func (c *controller) projectFor(check *Check) (*projectState, verdict) {
	name := check.Spec.ProjectRef.Name
	project, found := c.projects[name]
	if !found {
		return nil, verdict{reasonProjectNotFound, fmt.Sprintf("ClusterProject %s does not exist", name)}
	}
	if !project.verdict.ready() {
		return nil, verdict{reasonProjectNotReady, fmt.Sprintf("ClusterProject %s is not ready: %s", name, project.verdict.message)}
	}
	return project, synced
}

// syncCheck upserts the check when its request changed, writes the
// ping URL into a ping check's ConfigMap, then registers the probe or
// reports the CronJob's runs. It fills in next's slug, uuid, ping URL,
// and ConfigMap.
func (c *controller) syncCheck(ctx context.Context, check *Check, state *checkState, project *projectState, w *world, next *CheckStatus) verdict {
	request, v := c.upsertRequest(check, project, w)
	if !v.ready() {
		return v
	}
	if v := c.upsertCheck(ctx, state, check.Spec.ProjectRef.Name, project, request, next); !v.ready() {
		return v
	}
	if v := c.syncConfigMap(ctx, check, state, w, next); !v.ready() {
		return v
	}
	switch check.Spec.ProbeKind() {
	case ProbeKindPing:
		return synced
	case ProbeKindCronJob:
		finished := c.reportRuns(ctx, state, w.cronJobs[cronJobKey(check)], w.jobs[state.namespace], next.PingURL)
		if finished == nil && !fromRun(next.Conditions) {
			finished = lastReported(w.jobs[state.namespace], state.runs.lastReportedJob)
		}
		if finished != nil {
			next.Conditions = withCondition(next.Conditions, runPassing(*finished, state.generation, c.now()))
		}
		return synced
	}
	return c.registerProbe(state, check, next.PingURL)
}

// upsertCheck sends the request unless this process already sent the
// same one through the same client, and status holds its result. A
// changed project or slug deletes the check at the old place first,
// so there is never a second check for one Check.
func (c *controller) upsertCheck(ctx context.Context, state *checkState, name string, project *projectState, request healthchecks.UpsertRequest, next *CheckStatus) verdict {
	if next.Project != "" && next.Project != name {
		if v := c.moveOut(ctx, state, next, name); !v.ready() {
			return v
		}
	}
	if next.Project == name && state.via == project.client && reflect.DeepEqual(state.sent, &request) && next.UUID != "" && next.Slug == request.Slug {
		return synced
	}
	if next.Slug != "" && next.Slug != request.Slug {
		if v := c.deleteBySlug(ctx, state, project, next.Slug, request.Slug); !v.ready() {
			return v
		}
	}
	result, err := project.client.Upsert(ctx, request)
	if err != nil {
		return verdict{reasonUpsertFailed, "upserting check " + request.Slug + ": " + errorText(err)}
	}
	switch {
	case result.Created:
		c.log.printf("%s: created check %s in Healthchecks", state.subject(), request.Slug)
	case state.sent != nil:
		// An update after a restart restates what Healthchecks holds, so
		// only an update of a request this process sent is a change.
		c.log.printf("%s: updated check %s in Healthchecks", state.subject(), request.Slug)
	}
	state.backoff.succeeded()
	state.sent, state.via = &request, project.client
	next.Project, next.Slug, next.UUID, next.PingURL = name, request.Slug, result.UUID, result.PingURL
	return synced
}

// deleteBySlug deletes the check at the slug a Check had before its
// slug changed.
func (c *controller) deleteBySlug(ctx context.Context, state *checkState, project *projectState, old, current string) verdict {
	found, err := project.client.FindBySlug(ctx, old)
	if err != nil {
		return verdict{reasonDeleteFailed, "finding the check at the old slug " + old + ": " + errorText(err)}
	}
	if found == nil {
		return synced
	}
	if err := project.client.Delete(ctx, found.UUID); err != nil {
		return verdict{reasonDeleteFailed, "deleting the check at the old slug " + old + ": " + errorText(err)}
	}
	c.log.printf("%s: deleted check %s from Healthchecks, because the slug changed to %s", state.subject(), old, current)
	return synced
}

// writeCheckStatus applies next when it differs from what the API
// server holds. A steady check writes nothing.
func (c *controller) writeCheckStatus(ctx context.Context, state *checkState, next CheckStatus) {
	if reflect.DeepEqual(next, state.status) {
		return
	}
	err := c.client.applyStatus(ctx, checksResource, checkKind, state.namespace, state.name, next, nil)
	if errors.Is(err, errNotFound) {
		return
	}
	if err != nil {
		c.readings.reconcileFailed(checkKind)
		c.log.fault(state.subject(), topicStatus, "writing status: "+errorText(err))
		return
	}
	c.log.clear(state.subject(), topicStatus)
	state.status = next
}

func checkReady(v verdict, generation int64, now time.Time) Condition {
	condition := Condition{
		Type:               readyCondition,
		Status:             ConditionTrue,
		ObservedGeneration: generation,
		Reason:             reasonSynced,
		Message:            "the check exists in Healthchecks and matches the spec",
		LastTransitionTime: timestamp(now),
	}
	if !v.ready() {
		condition.Status, condition.Reason, condition.Message = ConditionFalse, v.reason, v.message
	}
	return condition
}
