package main

// The operator pings a check of its own every minute. The check is in
// the ClusterProject and at the slug the settings name, and no Check
// resource declares it. The heartbeat pings only if a pass finished in
// the last two minutes. The backstop ticker starts a pass every 30
// seconds even when nothing changes, so a stuck loop stops the
// heartbeat, and the heartbeat check goes down.

import (
	"context"
	"sync"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

const (
	heartbeatName = "healthchecks-operator"
	// heartbeatPeriod is both the check's timeout and its grace period
	// on Healthchecks, so the check goes down two minutes after the
	// last ping.
	heartbeatPeriod = time.Minute
	// heartbeatStale is how old the last pass can be for a ping to go
	// out. It is four backstop ticks.
	heartbeatStale = 2 * time.Minute
)

type heartbeat struct {
	project string
	slug    string
	c       *controller

	mutex sync.Mutex
	// lastPass is when the last pass finished.
	lastPass time.Time
	// pingURL is empty until the upsert succeeds. via is the project
	// client the upsert went through, so a changed URL or key upserts
	// again.
	pingURL string
	via     *healthchecks.Client
	// backoff spaces out the upserts after one fails. Only the pass
	// reads and writes it.
	backoff backoff
}

func newHeartbeat(project, slug string, c *controller) *heartbeat {
	return &heartbeat{project: project, slug: slug, c: c}
}

func (h *heartbeat) subject() string {
	return "heartbeat " + h.slug
}

// ensure upserts the heartbeat check once its project is ready, and
// again when the project's client changes. The pass calls it, so every
// call to the management API comes from the pass.
func (h *heartbeat) ensure(ctx context.Context, projects map[string]*projectState) {
	project, found := projects[h.project]
	if !found || !project.verdict.ready() {
		h.c.log.fault(h.subject(), topicReconcile, "ClusterProject "+h.project+" is not ready, so the operator sends no heartbeat")
		return
	}
	h.mutex.Lock()
	current := h.via == project.client && h.pingURL != ""
	h.mutex.Unlock()
	if current || h.backoff.waiting(h.c.now()) {
		return
	}
	// The heartbeat alerts through the project's default channels, and
	// a project that is ready resolves all of them.
	channels, _ := project.channelIDs(nil)
	result, err := project.client.Upsert(ctx, healthchecks.UpsertRequest{
		Name:     heartbeatName,
		Slug:     h.slug,
		Channels: channels,
		Grace:    heartbeatPeriod,
		Period:   healthchecks.FixedTimeout(heartbeatPeriod),
	})
	if err != nil {
		h.c.log.fault(h.subject(), topicReconcile, "upserting: "+errorText(err))
		h.backoff.failed(h.c.now())
		return
	}
	h.backoff.succeeded()
	h.c.log.clear(h.subject(), topicReconcile)
	if result.Created {
		h.c.log.printf("%s: created check %s in Healthchecks", h.subject(), h.slug)
	}
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.pingURL, h.via = result.PingURL, project.client
}

// passed records that a pass finished at the moment given.
func (h *heartbeat) passed(at time.Time) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.lastPass = at
}

// beat pings the heartbeat check if a pass finished recently.
func (h *heartbeat) beat(ctx context.Context) {
	h.mutex.Lock()
	pingURL, lastPass := h.pingURL, h.lastPass
	h.mutex.Unlock()
	if pingURL == "" || h.c.now().Sub(lastPass) > heartbeatStale {
		return
	}
	err := healthchecks.Ping(ctx, h.c.http, pingURL, healthchecks.PingSuccess, "")
	h.c.readings.observePing(pingSuccess, err)
	if err != nil {
		h.c.log.fault(h.subject(), topicPing, "pinging: "+errorText(err))
		return
	}
	h.c.log.clear(h.subject(), topicPing)
}

// run beats every interval until ctx ends.
func (h *heartbeat) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.beat(ctx)
		}
	}
}
