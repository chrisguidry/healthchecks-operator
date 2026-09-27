package main

// An http or tls Check has a prober in the probe schedule, registered
// under the Check's namespace/name. The schedule hands each result to
// report, which pings the check and records the result in the Passing
// condition.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

const passingCondition = "Passing"

// The reasons a Check's Passing condition gives.
const (
	reasonProbePassed = "ProbePassed"
	reasonProbeFailed = "ProbeFailed"
)

// probeIdentity names a Check's probe spec. The pass registers a new
// prober only when it changes, so an unchanged probe keeps its place
// in the schedule.
func probeIdentity(spec CheckSpec) string {
	var block any
	switch spec.ProbeKind() {
	case ProbeKindHTTP:
		block = spec.HTTP
	case ProbeKindTLS:
		block = spec.TLS
	default:
		return ""
	}
	encoded, _ := json.Marshal(block)
	return spec.ProbeKind().String() + " " + string(encoded)
}

// registerProbe sets the Check's prober in the schedule when its probe
// spec changed. A prober that cannot be built stops the old one, so
// the check goes down instead of reporting on a spec that no longer
// exists.
func (c *controller) registerProbe(state *checkState, check *Check, pingURL string) verdict {
	identity := probeIdentity(check.Spec)
	if identity == state.probe && pingURL == state.probeURL {
		return synced
	}
	interval, err := probeInterval(check.Spec)
	if err != nil {
		c.stopProbe(state)
		return verdict{reasonInvalidSpec, errorText(err)}
	}
	var built prober
	var kind string
	if check.Spec.ProbeKind() == ProbeKindHTTP {
		built, err = newHTTPProber(*check.Spec.HTTP, state.namespace, c.client)
		kind = probeHTTP
	} else {
		built, err = newTLSProber(*check.Spec.TLS)
		kind = probeTLS
	}
	if err != nil {
		c.stopProbe(state)
		return verdict{reasonProbeInvalid, errorText(err)}
	}
	key := state.namespace + "/" + state.name
	if state.probe != "" && pingURL != state.probeURL {
		// The probe now pings another check, one this Check took over
		// by its slug or found in another project. That check may have
		// gone longer without a ping than the interval allows, so the
		// key starts again, and a new key is due at once.
		c.schedule.remove(key)
	}
	c.schedule.set(key, interval, timedProber{built, kind, c.readings})
	state.probe, state.probeURL = identity, pingURL
	return synced
}

// stopProbe removes the Check's prober from the schedule, if it has
// one. The caller holds state's mutex.
func (c *controller) stopProbe(state *checkState) {
	if state.probe == "" {
		return
	}
	c.schedule.remove(state.namespace + "/" + state.name)
	state.probe, state.probeURL = "", ""
}

// report pings the check with one probe's result, and records the
// result in the Passing condition. A failure's reason is the ping body
// and the condition's message. A result that matches the one before
// writes nothing.
func (c *controller) report(ctx context.Context, key string, result probeResult) {
	state := c.lookupCheck(key)
	if state == nil {
		return
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	// A run that started before its Check went, or before it became a
	// cronJob check, finishes with no probe to report for.
	if state.probe == "" || state.status.PingURL == "" {
		return
	}
	kind, label := healthchecks.PingSuccess, pingSuccess
	if !result.passed {
		kind, label = healthchecks.PingFail, pingFail
	}
	err := healthchecks.Ping(ctx, c.http, state.status.PingURL, kind, result.reason)
	c.readings.observePing(label, err)
	if err != nil {
		c.log.fault(state.subject(), topicPing, "pinging: "+errorText(err))
	} else {
		c.log.clear(state.subject(), topicPing)
	}

	next := state.status
	next.Conditions = withCondition(next.Conditions, probePassing(result, state.generation, c.now()))
	c.writeCheckStatus(ctx, state, next)
}

func probePassing(result probeResult, generation int64, now time.Time) Condition {
	condition := Condition{
		Type:               passingCondition,
		Status:             ConditionTrue,
		ObservedGeneration: generation,
		Reason:             reasonProbePassed,
		Message:            "the last probe passed",
		LastTransitionTime: timestamp(now),
	}
	if !result.passed {
		condition.Status, condition.Reason, condition.Message = ConditionFalse, reasonProbeFailed, result.reason
	}
	return condition
}

// timedProber records each run of the prober it wraps in the probe
// metrics. It times the run on the wall clock, because the controller's
// clock is a fixture that a test holds still.
type timedProber struct {
	prober   prober
	kind     string
	readings *metrics
}

func (p timedProber) probe(ctx context.Context) probeResult {
	began := time.Now()
	result := p.prober.probe(ctx)
	p.readings.observeProbe(p.kind, result.passed, time.Since(began))
	return result
}
