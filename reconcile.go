package main

// The operator's loop is level-triggered. A watch event or the backstop
// ticker wakes it, and each pass lists every ClusterProject, Check,
// CronJob, and Job, then reconciles each object against what the
// operator holds in memory. A lost event costs at most one tick, and a
// restarted operator starts correct.
//
// The memory is what keeps a steady pass quiet: each Check's last
// upsert request, and each project's client and channels. A pass calls
// the Healthchecks management API only when one of those changed.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// The resources the loop lists and watches.
var (
	clusterProjectsResource = kubeResource{Group: "healthchecks.guid.foo", Version: "v1alpha1", Resource: "clusterprojects"}
	checksResource          = kubeResource{Group: "healthchecks.guid.foo", Version: "v1alpha1", Resource: "checks"}
	cronJobsResource        = kubeResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
	jobsResource            = kubeResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

// backstopInterval is how often the loop runs a pass with nothing to
// prompt it. The heartbeat depends on it: a pass at least this often
// means a loop that finished no pass in two minutes is stuck.
const backstopInterval = 30 * time.Second

// healthchecksTimeout bounds each call to a Healthchecks instance, a
// ping included, so an instance that stops answering does not stop
// the loop.
const healthchecksTimeout = 30 * time.Second

type controller struct {
	client *kubeClient
	// namespace holds the Secrets that a ClusterProject names.
	namespace string
	readings  *metrics
	now       func() time.Time
	http      *http.Client
	log       *lineLog
	wake      chan struct{}
	backstop  time.Duration
	schedule  *probeSchedule
	// heartbeat is nil when the settings name no heartbeat check.
	heartbeat *heartbeat

	// projects is read and written by the pass only.
	projects map[string]*projectState

	// mutex guards the map of checks, which the probe schedule's
	// goroutine reads to report each result. Each checkState has its
	// own mutex for its fields.
	mutex  sync.Mutex
	checks map[string]*checkState
}

func newController(config settings, client *kubeClient, readings *metrics, now func() time.Time, log io.Writer) *controller {
	c := &controller{
		client:    client,
		namespace: config.namespace,
		readings:  readings,
		now:       now,
		http:      healthchecks.NewHTTPClient(healthchecksTimeout),
		log:       newLineLog(log),
		wake:      make(chan struct{}, 1),
		backstop:  backstopInterval,
		projects:  map[string]*projectState{},
		checks:    map[string]*checkState{},
	}
	c.schedule = newProbeSchedule(now, c.report)
	if config.heartbeatProject != "" {
		c.heartbeat = newHeartbeat(config.heartbeatProject, config.heartbeatSlug, c)
	}
	return c
}

// run lists each watched resource, starts its watch from that list,
// and runs a pass at once and then on every wake and every tick, until
// ctx ends. A failed first list ends the operator, so the failure shows
// in the pod's restarts instead of in a retry loop.
func (c *controller) run(ctx context.Context) error {
	watches := []*collectionWatch{}
	versions := []string{}
	for _, resource := range []kubeResource{clusterProjectsResource, checksResource, cronJobsResource, jobsResource} {
		watch := newCollectionWatch(c.client, resource, "", c.wake, c.readings.watchRestarted(resource.Resource))
		version, err := watch.list(ctx)
		if err != nil {
			return fmt.Errorf("listing %s: %w", resource, err)
		}
		watches = append(watches, watch)
		versions = append(versions, version)
	}

	// run returns only after the goroutines it started stop, so none of
	// them calls an API after it.
	var running sync.WaitGroup
	defer running.Wait()
	for index, watch := range watches {
		running.Go(func() { watch.run(ctx, versions[index]) })
	}
	running.Go(func() { c.schedule.run(ctx) })
	if c.heartbeat != nil {
		running.Go(func() { c.heartbeat.run(ctx, time.Minute) })
	}

	ticker := time.NewTicker(c.backstop)
	defer ticker.Stop()
	for {
		c.pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-c.wake:
		case <-ticker.C:
		}
	}
}

// world is one pass's read of the cluster.
type world struct {
	projects []ClusterProject
	checks   []Check
	// cronJobs is keyed by namespace/name, and jobs by namespace.
	cronJobs map[string]batchCronJob
	jobs     map[string][]batchJob
}

func (c *controller) read(ctx context.Context) (*world, error) {
	var projects ClusterProjectList
	var checks CheckList
	var cronJobs struct {
		Items []batchCronJob `json:"items"`
	}
	var jobs struct {
		Items []batchJob `json:"items"`
	}
	lists := []struct {
		resource kubeResource
		out      any
	}{
		{clusterProjectsResource, &projects},
		{checksResource, &checks},
		{cronJobsResource, &cronJobs},
		{jobsResource, &jobs},
	}
	for _, list := range lists {
		if err := c.client.list(ctx, list.resource, "", list.out); err != nil {
			return nil, fmt.Errorf("listing %s: %w", list.resource, err)
		}
	}
	w := &world{
		projects: projects.Items,
		checks:   checks.Items,
		cronJobs: map[string]batchCronJob{},
		jobs:     map[string][]batchJob{},
	}
	for _, cronJob := range cronJobs.Items {
		w.cronJobs[objectKey(cronJob.Metadata)] = cronJob
	}
	for _, job := range jobs.Items {
		w.jobs[job.Metadata.Namespace] = append(w.jobs[job.Metadata.Namespace], job)
	}
	return w, nil
}

// objectKey names a namespaced object as namespace/name.
func objectKey(meta ObjectMeta) string {
	return meta.Namespace + "/" + meta.Name
}

// pass reconciles every ClusterProject, then every Check, and forgets
// the Checks that are gone. The whole pass is one
// healthchecks_reconcile_duration_seconds observation. Only a pass that
// read the cluster counts for the heartbeat.
func (c *controller) pass(ctx context.Context) {
	began := time.Now()
	defer func() { c.readings.observePass(time.Since(began)) }()

	w, err := c.read(ctx)
	if err != nil {
		c.log.fault("reading the cluster", topicReconcile, errorText(err))
		return
	}
	c.log.clear("reading the cluster", topicReconcile)

	c.reconcileProjects(ctx, w.projects)
	if c.heartbeat != nil {
		c.heartbeat.ensure(ctx, c.projects)
	}
	live := map[string]bool{}
	for index := range w.checks {
		check := &w.checks[index]
		live[objectKey(check.Metadata)] = true
		c.reconcileCheck(ctx, check, w)
	}
	c.forgetChecksExcept(live)
	if c.heartbeat != nil {
		c.heartbeat.passed(c.now())
	}
}

// readyCondition is the condition type that says whether a
// ClusterProject or a Check is ready.
const readyCondition = "Ready"

// verdict is what a reconcile found: ready, or the reason and the
// message that say why not. The message carries the source's own text
// word for word.
type verdict struct {
	reason  string
	message string
}

// synced is the verdict of an object that is ready.
var synced = verdict{}

func (v verdict) ready() bool {
	return v.reason == ""
}

// remote reports whether the verdict is a failed call to Healthchecks,
// which puts the object on a backoff.
func (v verdict) remote() bool {
	return v.reason == reasonUpsertFailed || v.reason == reasonDeleteFailed || v.reason == reasonHealthchecks
}

// note counts and logs a verdict. The log writes a failure once, and
// again only when its message changes, so a fault that lasts writes
// one line and not one line each pass.
func (c *controller) note(subject, kind string, v verdict) {
	if v.ready() {
		c.log.clear(subject, topicReconcile)
		return
	}
	c.readings.reconcileFailed(kind)
	c.log.fault(subject, topicReconcile, v.message)
}

// timestamp writes a moment the way the API server holds one.
func timestamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// errorText is an error's text for a log line or a condition message.
// It keeps the source's text word for word, and drops the newline that
// ends a JSON body, so a log line stays one line. A network error
// leaves out the local and remote addresses: the local port changes on
// every connection, and a message that changes on every failure writes
// a new status each time.
func errorText(err error) string {
	var request *url.Error
	var network *net.OpError
	if errors.As(err, &request) && errors.As(err, &network) {
		return fmt.Sprintf("%s %q: %s %s: %s", request.Op, request.URL, network.Op, network.Net, strings.TrimSpace(network.Err.Error()))
	}
	return strings.TrimSpace(err.Error())
}
