package main

// A hold pauses a project's checks while a condition on another object
// matches, such as a rollout that reboots every node and fails every
// probe on its way. The operator watches the resource that the hold
// names, as it watches its own, so the pass reads the object from a
// store and the object's change wakes the pass.
//
// A hold fails open. An object the operator cannot read, a condition
// that does not match, and a hold past its maxDuration all leave the
// checks to alert, because a hold that never ends would silence them.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// holdingCondition is the ClusterProject condition that says whether
// the hold pauses the project's checks. Its lastTransitionTime to True
// is when the hold started.
const holdingCondition = "Holding"

// The reasons the Holding condition gives.
const (
	reasonMatched          = "Matched"
	reasonNotMatched       = "NotMatched"
	reasonExpired          = "Expired"
	reasonTargetNotFound   = "TargetNotFound"
	reasonTargetUnreadable = "TargetUnreadable"
	reasonInvalidHold      = "InvalidHold"
)

// heldObject is what the operator reads of the object a hold names.
type heldObject struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		Conditions []Condition `json:"conditions,omitempty"`
	} `json:"status"`
}

// targetWatch watches the one object that a hold names, selected by
// its name, until stop. Its first list runs in the watch's goroutine,
// with the watch's backoff, so a target the operator cannot list never
// holds up a pass. Until the list lands, the watch is not synced, and a
// pass leaves the Holding condition as it was.
type targetWatch struct {
	watch *collectionWatch
	stop  context.CancelFunc

	mutex  sync.Mutex
	synced bool
	fault  string
}

func (t *targetWatch) state() (bool, string) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return t.synced, t.fault
}

// run lists the object until a list works, then watches it until ctx
// ends.
func (t *targetWatch) run(ctx context.Context) {
	wait := t.watch.pause
	for {
		version, err := t.watch.list(ctx)
		if err == nil {
			t.mutex.Lock()
			t.synced, t.fault = true, ""
			t.mutex.Unlock()
			poke(t.watch.wake)
			t.watch.run(ctx, version)
			return
		}
		t.mutex.Lock()
		t.fault = "listing " + t.watch.resource.String() + ": " + errorText(err)
		t.mutex.Unlock()
		if !sleep(ctx, wait) {
			return
		}
		wait = min(wait*2, t.watch.backoff)
	}
}

// key names the object a hold watches.
func (t HoldTarget) key() string {
	return t.resource().String() + " " + t.Namespace + "/" + t.Name
}

// resource is the resource that holds the object, from its apiVersion
// and its plural.
func (t HoldTarget) resource() kubeResource {
	group, version, found := strings.Cut(t.APIVersion, "/")
	if !found {
		group, version = "", t.APIVersion
	}
	return kubeResource{Group: group, Version: version, Resource: t.Resource}
}

// object names the object, for a message.
func (t HoldTarget) object() string {
	if t.Namespace == "" {
		return t.resource().String() + " " + t.Name
	}
	return t.resource().String() + " " + t.Namespace + "/" + t.Name
}

// describe names the object and the condition, for a message.
func (t HoldTarget) describe() string {
	condition := t.Condition.Type + "=" + string(t.Condition.Status)
	if t.Condition.Reason != "" {
		condition += " with reason " + t.Condition.Reason
	}
	return t.object() + " has " + condition
}

// matches reports whether the object holds the condition.
func (c HoldCondition) matches(object heldObject) bool {
	found, ok := findCondition(object.Status.Conditions, c.Type)
	return ok && found.Status == c.Status && (c.Reason == "" || found.Reason == c.Reason)
}

// watchTargets watches each object that a hold names, and stops the
// watches no hold names. A new watch reads its object in its own
// goroutine.
func (c *controller) watchTargets(ctx context.Context, projects []ClusterProject) {
	named := map[string]HoldTarget{}
	for _, project := range projects {
		if project.Spec.Hold != nil {
			named[project.Spec.Hold.While.key()] = project.Spec.Hold.While
		}
	}
	for key, target := range c.targets {
		if _, held := named[key]; !held {
			target.stop()
			delete(c.targets, key)
		}
	}
	for key, held := range named {
		if c.targets[key] != nil {
			continue
		}
		resource := held.resource()
		watch := newCollectionWatch(c.client, resource, held.Namespace, "", trimTo[heldObject], c.wake, c.readings.watchRestarted(resource.Resource))
		watch.fields = "metadata.name=" + held.Name
		watch.pause, watch.backoff = c.targetPause, c.targetBackoff
		watching, stop := context.WithCancel(ctx)
		target := &targetWatch{watch: watch, stop: stop}
		c.targets[key] = target
		c.targetsRunning.Go(func() { target.run(watching) })
	}
}

// holdingVerdict judges a project's hold: the Holding condition, and
// whether the hold pauses the checks now. previous is the Holding
// condition the project holds, which carries when the hold started
// and whether it expired.
func (c *controller) holdingVerdict(hold *Hold, previous Condition, generation int64) (Condition, bool) {
	condition := Condition{
		Type:               holdingCondition,
		Status:             ConditionFalse,
		ObservedGeneration: generation,
		LastTransitionTime: timestamp(c.now()),
	}
	maxDuration, err := time.ParseDuration(hold.MaxDuration)
	if err != nil {
		condition.Reason, condition.Message = reasonInvalidHold, "maxDuration: "+errorText(err)
		return condition, false
	}
	object, read, reason, message := c.heldObject(hold.While)
	holding := previous.Status == ConditionTrue
	if !read && (holding || previous.Reason == reasonExpired) {
		// The watch has not read the object yet, as after a restart, so
		// a hold that stands, or one that expired, stays as it was: a
		// restart during a rollout neither releases the hold nor starts
		// its maxDuration again. The maxDuration still ends a hold the
		// watch never reads.
		if holding && c.expired(previous.LastTransitionTime, maxDuration) {
			condition.Reason = reasonExpired
			condition.Message = fmt.Sprintf("the checks alert, because the hold reached its maxDuration of %s; the next hold starts after the condition stops matching and matches again", hold.MaxDuration)
			return condition, false
		}
		return previous, holding
	}
	if reason != "" {
		condition.Reason, condition.Message = reason, message
		return condition, false
	}
	switch {
	case !hold.While.Condition.matches(object):
		condition.Reason = reasonNotMatched
		condition.Message = "the checks alert, because it is not true that " + hold.While.describe()
	case previous.Status == ConditionTrue && c.expired(previous.LastTransitionTime, maxDuration),
		previous.Status == ConditionFalse && previous.Reason == reasonExpired:
		condition.Reason = reasonExpired
		condition.Message = fmt.Sprintf("the checks alert, because the hold reached its maxDuration of %s; the next hold starts after the condition stops matching and matches again", hold.MaxDuration)
	default:
		condition.Status, condition.Reason = ConditionTrue, reasonMatched
		condition.Message = "the checks are paused, because " + hold.While.describe()
	}
	return condition, condition.Status == ConditionTrue
}

// heldObject finds the object a hold names in its watch's store, or
// returns the reason and the message that say why it cannot. read is
// false while the watch has not yet read the object.
func (c *controller) heldObject(target HoldTarget) (object heldObject, read bool, reason, message string) {
	watch := c.targets[target.key()]
	if watch == nil {
		return heldObject{}, false, reasonTargetUnreadable, target.object() + " is not watched"
	}
	if synced, fault := watch.state(); !synced {
		if fault == "" {
			fault = "reading " + target.object() + " for the first time"
		}
		return heldObject{}, false, reasonTargetUnreadable, fault
	}
	objects, err := decodeSnapshot[heldObject](watch.watch.store)
	if err != nil {
		return heldObject{}, true, reasonTargetUnreadable, "decoding " + target.resource().String() + ": " + errorText(err)
	}
	for _, found := range objects {
		if found.Metadata.Namespace == target.Namespace && found.Metadata.Name == target.Name {
			return found, true, "", ""
		}
	}
	return heldObject{}, true, reasonTargetNotFound, target.object() + " does not exist"
}

// expired reports whether a hold that started at since has lasted
// maxDuration. A start the operator cannot read has expired, so the
// hold fails open.
func (c *controller) expired(since string, maxDuration time.Duration) bool {
	started, err := time.Parse(time.RFC3339, since)
	return err != nil || !c.now().Before(started.Add(maxDuration))
}
