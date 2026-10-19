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

// targetWatch watches one resource that a hold names, until stop.
type targetWatch struct {
	watch *collectionWatch
	stop  context.CancelFunc
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

// watchTargets watches each resource that a hold names, and stops the
// watches no hold names. A new watch lists its resource in the pass, so
// the pass that starts it reads a full store. A list that fails leaves
// the resource unwatched, and the next pass lists it again.
func (c *controller) watchTargets(ctx context.Context, projects []ClusterProject) {
	named := map[kubeResource]bool{}
	for _, project := range projects {
		if project.Spec.Hold != nil {
			named[project.Spec.Hold.While.resource()] = true
		}
	}
	for resource, target := range c.targets {
		if !named[resource] {
			target.stop()
			delete(c.targets, resource)
			delete(c.targetFaults, resource)
		}
	}
	for resource := range named {
		if c.targets[resource] != nil {
			continue
		}
		watch := newCollectionWatch(c.client, resource, "", "", trimTo[heldObject], c.wake, c.readings.watchRestarted(resource.Resource))
		version, err := watch.list(ctx)
		if err != nil {
			c.targetFaults[resource] = "listing " + resource.String() + ": " + errorText(err)
			continue
		}
		delete(c.targetFaults, resource)
		watching, stop := context.WithCancel(ctx)
		c.targets[resource] = &targetWatch{watch: watch, stop: stop}
		c.targetsRunning.Go(func() { watch.run(watching, version) })
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
	object, reason, message := c.heldObject(hold.While)
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
// returns the reason and the message that say why it cannot.
func (c *controller) heldObject(target HoldTarget) (heldObject, string, string) {
	resource := target.resource()
	watch := c.targets[resource]
	if watch == nil {
		return heldObject{}, reasonTargetUnreadable, c.targetFaults[resource]
	}
	objects, err := decodeSnapshot[heldObject](watch.watch.store)
	if err != nil {
		return heldObject{}, reasonTargetUnreadable, "decoding " + resource.String() + ": " + errorText(err)
	}
	for _, object := range objects {
		if object.Metadata.Namespace == target.Namespace && object.Metadata.Name == target.Name {
			return object, "", ""
		}
	}
	return heldObject{}, reasonTargetNotFound, target.object() + " does not exist"
}

// expired reports whether a hold that started at since has lasted
// maxDuration. A start the operator cannot read has expired, so the
// hold fails open.
func (c *controller) expired(since string, maxDuration time.Duration) bool {
	started, err := time.Parse(time.RFC3339, since)
	return err != nil || !c.now().Before(started.Add(maxDuration))
}
