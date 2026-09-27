package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// scheduleClock is a clock that moves only when a test advances it.
// Its timers fire when the clock reaches their deadline, so a test
// moves the schedule forward without sleeping.
type scheduleClock struct {
	mutex   sync.Mutex
	current time.Time
	timers  []scheduleTimer
}

type scheduleTimer struct {
	deadline time.Time
	fire     chan time.Time
}

func (c *scheduleClock) now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.current
}

// at replaces the schedule's timer. A deadline that has passed
// fires at once, so a test that advances the clock before the loop
// sets its timer does not lose the wake.
func (c *scheduleClock) at(deadline time.Time) <-chan time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	fire := make(chan time.Time, 1)
	if !c.current.Before(deadline) {
		fire <- c.current
		return fire
	}
	c.timers = append(c.timers, scheduleTimer{deadline, fire})
	return fire
}

func (c *scheduleClock) advance(d time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.current = c.current.Add(d)
	waiting := c.timers[:0]
	for _, timer := range c.timers {
		if c.current.Before(timer.deadline) {
			waiting = append(waiting, timer)
			continue
		}
		timer.fire <- c.current
	}
	c.timers = waiting
}

// scheduleCall is one call to a scheduledProber: the clock's time when
// the run started, and the channel that takes the run's result.
type scheduleCall struct {
	at    time.Time
	reply chan probeResult
}

// scheduledProber hands each call to the test and returns the result
// the test replies with, so a test holds a run in flight for as long as
// it needs.
type scheduledProber struct {
	clock *scheduleClock
	calls chan scheduleCall
}

func (p *scheduledProber) probe(ctx context.Context) probeResult {
	call := scheduleCall{p.clock.now(), make(chan probeResult, 1)}
	select {
	case p.calls <- call:
	case <-ctx.Done():
		return probeResult{}
	}
	select {
	case result := <-call.reply:
		return result
	case <-ctx.Done():
		return probeResult{}
	}
}

type scheduleReport struct {
	key    string
	result probeResult
}

type scheduleHarness struct {
	schedule *probeSchedule
	clock    *scheduleClock
	reports  chan scheduleReport
}

var scheduleStart = time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)

// startSchedule runs a schedule on a scheduleClock until the test ends.
// Each report waits for the test to read it, so the test reads reports
// in the order the schedule makes them.
func startSchedule(t *testing.T) *scheduleHarness {
	t.Helper()
	h := &scheduleHarness{clock: &scheduleClock{current: scheduleStart}, reports: make(chan scheduleReport)}
	h.schedule = newProbeSchedule(h.clock.now, func(ctx context.Context, key string, result probeResult) {
		select {
		case h.reports <- scheduleReport{key, result}:
		case <-ctx.Done():
		}
	})
	h.schedule.timerAt = h.clock.at
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		h.schedule.run(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return h
}

func (h *scheduleHarness) prober() *scheduledProber {
	return &scheduledProber{clock: h.clock, calls: make(chan scheduleCall)}
}

// receive reads one value, and fails the test if none arrives within
// testTimeout.
func receive[T any](t *testing.T, from <-chan T) T {
	t.Helper()
	select {
	case value := <-from:
		return value
	case <-time.After(testTimeout):
		t.Fatalf("nothing arrived within %s", testTimeout)
		panic("unreachable")
	}
}

// runOnce waits for the prober's next call, answers it with result,
// and returns the time the run started.
func runOnce(t *testing.T, p *scheduledProber, result probeResult) time.Time {
	t.Helper()
	call := receive(t, p.calls)
	call.reply <- result
	return call.at
}

// mustBeIdle fails the test if the prober has a call waiting. A call
// that arrives later is not caught, so the check never fails a correct
// schedule.
func mustBeIdle(t *testing.T, p *scheduledProber) {
	t.Helper()
	select {
	case call := <-p.calls:
		t.Fatalf("the prober ran at %s", call.at)
	default:
	}
}

var (
	passed = probeResult{passed: true}
	failed = probeResult{reason: "https://example.com/: status 502, want 200"}
)

func TestScheduleProbesANewKeyAtOnce(t *testing.T) {
	h := startSchedule(t)
	p := h.prober()

	h.schedule.set("example/website", time.Minute, p)

	mustMatch(t, runOnce(t, p, failed), scheduleStart)
	mustMatch(t, receive(t, h.reports), scheduleReport{"example/website", failed})
}

func TestScheduleRunsAKeyOneIntervalAfterItsRunStarted(t *testing.T) {
	h := startSchedule(t)
	p := h.prober()
	h.schedule.set("example/website", time.Minute, p)
	call := receive(t, p.calls)
	h.clock.advance(10 * time.Second)
	call.reply <- passed
	receive(t, h.reports)

	h.clock.advance(50 * time.Second)

	mustMatch(t, runOnce(t, p, passed), scheduleStart.Add(time.Minute))
	mustMatch(t, receive(t, h.reports), scheduleReport{"example/website", passed})
}

func TestScheduleKeepsTheDueTimeWhenTheIntervalIsTheSame(t *testing.T) {
	h := startSchedule(t)
	first, second := h.prober(), h.prober()
	h.schedule.set("example/website", time.Minute, first)
	runOnce(t, first, passed)
	receive(t, h.reports)
	h.clock.advance(30 * time.Second)

	h.schedule.set("example/website", time.Minute, second)
	h.clock.advance(30 * time.Second)

	mustMatch(t, runOnce(t, second, failed), scheduleStart.Add(time.Minute))
	mustMatch(t, receive(t, h.reports), scheduleReport{"example/website", failed})
	mustBeIdle(t, first)
}

func TestScheduleRunsAtOnceWhenTheIntervalChanges(t *testing.T) {
	h := startSchedule(t)
	first, second := h.prober(), h.prober()
	h.schedule.set("example/website", time.Minute, first)
	runOnce(t, first, passed)
	receive(t, h.reports)
	h.clock.advance(20 * time.Second)

	h.schedule.set("example/website", 5*time.Minute, second)

	changed := scheduleStart.Add(20 * time.Second)
	mustMatch(t, runOnce(t, second, passed), changed)
	receive(t, h.reports)
	h.clock.advance(5 * time.Minute)
	mustMatch(t, runOnce(t, second, passed), changed.Add(5*time.Minute))
}

func TestScheduleDoesNotStartAKeyWhileItsRunIsInFlight(t *testing.T) {
	h := startSchedule(t)
	slow, other := h.prober(), h.prober()
	h.schedule.set("example/slow", time.Minute, slow)
	call := receive(t, slow.calls)
	h.clock.advance(3 * time.Minute)

	h.schedule.set("example/other", time.Hour, other)
	runOnce(t, other, passed)
	mustMatch(t, receive(t, h.reports), scheduleReport{"example/other", passed})
	mustBeIdle(t, slow)

	call.reply <- failed
	mustMatch(t, receive(t, h.reports), scheduleReport{"example/slow", failed})
	mustMatch(t, runOnce(t, slow, passed), scheduleStart.Add(3*time.Minute))
}

func TestScheduleStopsProbingARemovedKey(t *testing.T) {
	h := startSchedule(t)
	removed, other := h.prober(), h.prober()
	h.schedule.set("example/removed", time.Minute, removed)
	runOnce(t, removed, passed)
	receive(t, h.reports)

	h.schedule.remove("example/removed")
	h.schedule.remove("example/never-set")
	h.clock.advance(time.Hour)

	h.schedule.set("example/other", time.Hour, other)
	runOnce(t, other, passed)
	mustMatch(t, receive(t, h.reports), scheduleReport{"example/other", passed})
	mustBeIdle(t, removed)
}

// A key removed during a run and set again is a new key. The old run's
// result belongs to the removed key, so only the new prober's result
// is reported.
func TestScheduleDropsTheResultOfARunForARemovedKey(t *testing.T) {
	h := startSchedule(t)
	old, replacement := h.prober(), h.prober()
	h.schedule.set("example/website", time.Minute, old)
	call := receive(t, old.calls)

	h.schedule.remove("example/website")
	h.schedule.set("example/website", time.Minute, replacement)
	call.reply <- failed

	runOnce(t, replacement, passed)
	mustMatch(t, receive(t, h.reports), scheduleReport{"example/website", passed})
}

func TestScheduleDropsTheResultOfARunForAKeyRemovedForGood(t *testing.T) {
	h := startSchedule(t)
	removed, other := h.prober(), h.prober()
	h.schedule.set("example/removed", time.Minute, removed)
	call := receive(t, removed.calls)

	h.schedule.remove("example/removed")
	call.reply <- failed
	h.schedule.set("example/other", time.Hour, other)

	runOnce(t, other, passed)
	mustMatch(t, receive(t, h.reports), scheduleReport{"example/other", passed})
}

// passingProber passes at once, for a schedule on the real clock.
type passingProber struct{}

func (passingProber) probe(context.Context) probeResult { return passed }

func TestScheduleRunsOnTheRealClock(t *testing.T) {
	reports := make(chan string)
	schedule := newProbeSchedule(time.Now, func(ctx context.Context, key string, _ probeResult) {
		select {
		case reports <- key:
		case <-ctx.Done():
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go schedule.run(ctx)

	schedule.set("example/website", time.Millisecond, passingProber{})

	mustMatch(t, receive(t, reports), "example/website")
	mustMatch(t, receive(t, reports), "example/website")
}
