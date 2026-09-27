package main

// A deleted Check keeps its finalizer until its check is gone from
// Healthchecks. The finalizer comes off last, with a patch pinned to
// the resourceVersion the pass read, so the operator never removes it
// while the check still exists.

import (
	"context"
	"errors"
	"fmt"
)

// releaseCheck deletes a deleted Check's check in Healthchecks, then
// removes the finalizer. A project that is missing or has no working
// client keeps the finalizer, and Ready says why.
func (c *controller) releaseCheck(ctx context.Context, check *Check, state *checkState) {
	c.stopProbe(state)
	if !check.Metadata.holds(checkFinalizer) {
		return
	}
	v := synced
	if !state.deleted {
		v = c.deleteCheck(ctx, check, state)
	}
	if v.remote() {
		state.backoff.failed(c.now())
	}
	if !v.ready() {
		next := state.status
		next.Conditions = withCondition(next.Conditions, checkReady(v, state.generation, c.now()))
		c.writeCheckStatus(ctx, state, next)
		c.note(state.subject(), checkKind, v)
		return
	}
	err := c.client.setFinalizers(ctx, checksResource, state.namespace, state.name, check.Metadata.ResourceVersion, check.Metadata.withoutFinalizer(checkFinalizer), nil)
	if errors.Is(err, errConflict) || errors.Is(err, errNotFound) {
		return
	}
	if err != nil {
		c.note(state.subject(), checkKind, verdict{reasonFinalizerFailed, "removing the finalizer: " + errorText(err)})
		return
	}
	c.log.printf("%s: removed the finalizer %s", state.subject(), checkFinalizer)
}

// deleteCheck deletes the check by the uuid in status, or finds it by
// slug when status has no uuid. The check is in the project that
// status names, which differs from the spec's while a move waits.
func (c *controller) deleteCheck(ctx context.Context, check *Check, state *checkState) verdict {
	name := state.status.Project
	if name == "" {
		name = check.Spec.ProjectRef.Name
	}
	project, found := c.projects[name]
	if !found {
		return verdict{reasonProjectNotFound, fmt.Sprintf("ClusterProject %s does not exist, so the check stays in Healthchecks and the finalizer stays", name)}
	}
	if !project.usable() {
		return verdict{reasonProjectNotReady, fmt.Sprintf("ClusterProject %s is not ready, so the check stays in Healthchecks and the finalizer stays: %s", name, project.verdict.message)}
	}
	slug, uuid := state.status.Slug, state.status.UUID
	if slug == "" {
		slug = check.EffectiveSlug()
	}
	if uuid == "" {
		held, err := project.client.FindBySlug(ctx, slug)
		if err != nil {
			return verdict{reasonDeleteFailed, "finding check " + slug + ": " + errorText(err)}
		}
		if held == nil {
			state.deleted = true
			return synced
		}
		uuid = held.UUID
	}
	if err := project.client.Delete(ctx, uuid); err != nil {
		return verdict{reasonDeleteFailed, "deleting check " + slug + ": " + errorText(err)}
	}
	c.log.printf("%s: deleted check %s from Healthchecks", state.subject(), slug)
	state.deleted = true
	return synced
}
