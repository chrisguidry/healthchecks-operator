package main

// A ClusterProject is the operator's connection to one Healthchecks
// project: the API key from a Secret in the operator's namespace, one
// client, and the project's channels. The operator lists the channels
// once for each client, and again only when the project or a Check
// names a channel that the list does not hold, so a steady pass makes
// no call to Healthchecks for a project.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// The reasons a ClusterProject's Ready condition gives.
const (
	reasonConnected        = "Connected"
	reasonSecretUnreadable = "SecretUnreadable"
	reasonHealthchecks     = "HealthchecksError"
	reasonChannelNotFound  = "ChannelNotFound"
)

// projectState is what the operator holds for one ClusterProject
// between passes.
type projectState struct {
	url    string
	apiKey string
	// keyReadAt is when the key was last read, and keyGeneration the
	// ClusterProject's generation then.
	keyReadAt     time.Time
	keyGeneration int64
	client        *healthchecks.Client
	// channels is every channel in the project, listed by client. It
	// is valid only while listed is true.
	channels []healthchecks.Channel
	listed   bool
	// relist asks the next pass to list the channels again, because a
	// Check named a channel that the list does not hold.
	relist bool
	// backoff spaces out the calls to Healthchecks after one fails.
	backoff backoff
	// keyFailedGeneration is the ClusterProject's generation when a read
	// of its key failed and started the backoff, and zero otherwise.
	keyFailedGeneration int64
	// resolved is spec.channels, resolved to IDs.
	resolved []ResolvedChannel
	verdict  verdict
	// holding says spec.hold pauses the project's checks now, and since
	// is when the hold started, as the Holding condition states it.
	holding bool
	since   string
}

// usable reports whether the project's client works: its key reads and
// its channels list. A project whose only fault is a channel name can
// still delete a check.
func (p *projectState) usable() bool {
	return p.client != nil && p.listed
}

// reconcileProjects reconciles every ClusterProject and forgets the
// ones that are gone.
func (c *controller) reconcileProjects(ctx context.Context, projects []ClusterProject) {
	c.watchTargets(ctx, projects)
	live := map[string]bool{}
	for index := range projects {
		project := &projects[index]
		live[project.Metadata.Name] = true
		c.reconcileProject(ctx, project)
	}
	for name := range c.projects {
		if !live[name] {
			delete(c.projects, name)
			c.log.forget("clusterproject " + name)
		}
	}
}

func (c *controller) reconcileProject(ctx context.Context, project *ClusterProject) {
	name := project.Metadata.Name
	state, held := c.projects[name]
	if !held {
		state = &projectState{}
		c.projects[name] = state
	}
	// A person fixes a Secret the project names wrongly by editing the
	// project, so the edit ends the backoff the failed read started.
	if state.keyFailedGeneration != 0 && state.keyFailedGeneration != project.Metadata.Generation {
		state.backoff.succeeded()
		state.keyFailedGeneration = 0
	}
	if state.backoff.waiting(c.now()) {
		return
	}
	state.verdict = c.connectProject(ctx, project, state)
	state.resolved = nil
	if state.usable() {
		var missing []string
		state.resolved, missing = resolveChannels(state.channels, project.Spec.Channels)
		if len(missing) > 0 {
			// A person may make the channel after this list, so the next
			// pass lists the channels again.
			state.relist = true
			state.verdict = verdict{reasonChannelNotFound, "no channel in the project is named " + quoteNames(missing)}
		}
	}

	status := ClusterProjectStatus{
		Channels:   state.resolved,
		Conditions: withCondition(project.Status.Conditions, projectReady(state.verdict, project.Metadata.Generation, c.now())),
	}
	status.Conditions = c.reconcileHold(project, state, status.Conditions)
	if !reflect.DeepEqual(status, project.Status) {
		if encoded, err := json.Marshal(status); err == nil {
			c.watches[clusterProjectsResource].store.wroteStatus("/"+name, encoded)
		}
		if err := c.client.applyStatus(ctx, clusterProjectsResource, clusterProjectKind, "", name, status, nil); err != nil {
			c.log.fault("clusterproject "+name, topicStatus, "writing status: "+errorText(err))
			c.readings.reconcileFailed(clusterProjectKind)
			return
		}
		c.readings.wroteStatus(clusterProjectKind)
		c.postTransitions(referenceTo(clusterProjectKind, project.Metadata), project.Status.Conditions, status.Conditions)
	}
	c.note("clusterproject "+name, clusterProjectKind, state.verdict)
}

// reconcileHold judges the project's hold, and returns conditions with
// the Holding condition it states, or with none for a project that has
// no hold.
func (c *controller) reconcileHold(project *ClusterProject, state *projectState, conditions []Condition) []Condition {
	was := state.holding
	state.holding, state.since = false, ""
	if project.Spec.Hold == nil {
		if was {
			c.log.printf("clusterproject %s: the hold ended, because the spec no longer states one", project.Metadata.Name)
		}
		return withoutCondition(conditions, holdingCondition)
	}
	previous, _ := findCondition(conditions, holdingCondition)
	holding, holds := c.holdingVerdict(project.Spec.Hold, previous, project.Metadata.Generation)
	conditions = withCondition(conditions, holding)
	if holds {
		held, _ := findCondition(conditions, holdingCondition)
		state.holding, state.since = true, held.LastTransitionTime
	}
	if holds != was || holding.Reason != previous.Reason {
		c.log.printf("clusterproject %s: %s", project.Metadata.Name, holding.Message)
	}
	return conditions
}

// keyReadPeriod is how often the operator reads a project's API key
// again with nothing to prompt it. The key's Secret is named by each
// ClusterProject, so following it with a watch would need list and
// watch on Secrets of names RBAC cannot know in advance. A key that
// Healthchecks refuses, or a change to the ClusterProject, reads it at
// once.
const keyReadPeriod = 5 * time.Minute

// readKey returns the project's API key, read again when it is due. A
// read that fails for a reason that may clear, such as an API server
// that is slow to answer, keeps the key that last worked, so one failed
// read does not make every Check of the project not Ready.
func (c *controller) readKey(ctx context.Context, project *ClusterProject, state *projectState) (string, error) {
	now := c.now()
	fresh := state.apiKey != "" && state.keyGeneration == project.Metadata.Generation &&
		now.Sub(state.keyReadAt) < keyReadPeriod && (state.client == nil || !state.client.KeyRefused())
	if fresh {
		return state.apiKey, nil
	}
	ref := project.Spec.APIKeySecret
	key, err := c.client.secretValue(ctx, c.namespace, ref.Name, ref.Key)
	if err != nil {
		if transient(err) && state.apiKey != "" && state.keyGeneration == project.Metadata.Generation {
			c.log.fault("clusterproject "+project.Metadata.Name, topicKey, "keeping the API key that worked: "+errorText(err))
			return state.apiKey, nil
		}
		return "", err
	}
	c.log.clear("clusterproject "+project.Metadata.Name, topicKey)
	state.keyReadAt, state.keyGeneration = now, project.Metadata.Generation
	return key, nil
}

// connectProject reads the API key and lists the channels when the
// client is new or a Check asked for a new list.
func (c *controller) connectProject(ctx context.Context, project *ClusterProject, state *projectState) verdict {
	key, err := c.readKey(ctx, project, state)
	if err != nil {
		// No event announces a Secret that appears or changes, so a pass
		// would read it again every time without the backoff.
		state.client, state.listed, state.apiKey = nil, false, ""
		state.backoff.failed(c.now())
		state.keyFailedGeneration = project.Metadata.Generation
		return verdict{reasonSecretUnreadable, errorText(err)}
	}
	state.keyFailedGeneration = 0
	if state.client == nil || state.url != project.Spec.URL || state.apiKey != key || state.client.KeyRefused() {
		state.client = healthchecks.NewClient(project.Spec.URL, key, c.http)
		state.url, state.apiKey, state.listed = project.Spec.URL, key, false
	}
	if state.listed && !state.relist {
		return synced
	}
	channels, err := state.client.Channels(ctx)
	if err != nil {
		state.listed = false
		state.backoff.failed(c.now())
		return verdict{reasonHealthchecks, "listing channels: " + errorText(err)}
	}
	state.backoff.succeeded()
	state.channels, state.listed, state.relist = channels, true, false
	return synced
}

// resolveChannels finds each name in channels, and returns the names
// that match no channel.
func resolveChannels(channels []healthchecks.Channel, names []string) ([]ResolvedChannel, []string) {
	var resolved []ResolvedChannel
	var missing []string
	for _, name := range names {
		found := false
		for _, channel := range channels {
			if channel.Name == name {
				resolved = append(resolved, ResolvedChannel{Name: name, ID: channel.ID})
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	return resolved, missing
}

// channelIDs is the channel IDs for a Check. A Check's own list of
// names replaces the project's list. A name that matches no channel
// asks the next pass to list the channels again, because a person may
// have made the channel since the last list.
func (p *projectState) channelIDs(names []string) ([]string, verdict) {
	resolved := p.resolved
	if names != nil {
		var missing []string
		resolved, missing = resolveChannels(p.channels, names)
		if len(missing) > 0 {
			p.relist = true
			return nil, verdict{reasonChannelNotFound, "no channel in the project is named " + quoteNames(missing)}
		}
	}
	ids := []string{}
	for _, channel := range resolved {
		ids = append(ids, channel.ID)
	}
	return ids, synced
}

func quoteNames(names []string) string {
	quoted := make([]string, len(names))
	for index, name := range names {
		quoted[index] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

func projectReady(v verdict, generation int64, now time.Time) Condition {
	condition := Condition{
		Type:               readyCondition,
		Status:             ConditionTrue,
		ObservedGeneration: generation,
		Reason:             reasonConnected,
		Message:            "the API key works, and every channel resolves",
		LastTransitionTime: timestamp(now),
	}
	if !v.ready() {
		condition.Status, condition.Reason, condition.Message = ConditionFalse, v.reason, v.message
	}
	return condition
}
