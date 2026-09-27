package main

// A Check whose projectRef changes moves its check from one project to
// another. The operator deletes the check from the project that status
// names before it creates the check in the new one, so a check is
// never in two projects at once.

import (
	"context"
	"fmt"
)

// moveOut deletes the check from the project in next.Project, then
// clears next's check, so the upsert that follows creates the check in
// the project named to. A missing or failing old project stops the
// move, and the check stays where it is.
func (c *controller) moveOut(ctx context.Context, state *checkState, next *CheckStatus, to string) verdict {
	from := next.Project
	old, found := c.projects[from]
	if !found {
		return verdict{reasonProjectNotFound, fmt.Sprintf("ClusterProject %s holds the check and does not exist, so the check stays there and is not created in %s", from, to)}
	}
	if !old.verdict.ready() {
		return verdict{reasonProjectNotReady, fmt.Sprintf("ClusterProject %s holds the check and is not ready, so the check stays there and is not created in %s: %s", from, to, old.verdict.message)}
	}
	uuid := next.UUID
	if uuid == "" {
		held, err := old.client.FindBySlug(ctx, next.Slug)
		if err != nil {
			return verdict{reasonDeleteFailed, "finding check " + next.Slug + " in ClusterProject " + from + ": " + errorText(err)}
		}
		if held != nil {
			uuid = held.UUID
		}
	}
	if uuid != "" {
		if err := old.client.Delete(ctx, uuid); err != nil {
			return verdict{reasonDeleteFailed, "deleting check " + next.Slug + " from ClusterProject " + from + ": " + errorText(err)}
		}
		c.log.printf("%s: deleted check %s from ClusterProject %s, because the Check moved to %s", state.subject(), next.Slug, from, to)
	}
	next.Project, next.Slug, next.UUID, next.PingURL = to, "", "", ""
	return synced
}
