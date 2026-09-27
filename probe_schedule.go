package main

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// probeSchedule runs every registered prober at its interval from one
// timer in one goroutine, and hands each result to report. A cluster
// with a thousand Checks has one timer and a heap of a thousand
// entries, and it has a goroutine for a probe only while that probe
// runs.
type probeSchedule struct {
	now    func() time.Time
	report func(ctx context.Context, key string, result probeResult)
	// timerAt returns a channel that receives once the clock reaches
	// deadline. It is a field so a test replaces the timer along with
	// the clock.
	timerAt func(deadline time.Time) <-chan time.Time

	mutex   sync.Mutex
	entries map[string]*scheduleEntry
	// queue holds every entry that is not running, earliest due first.
	// A running entry leaves the queue, so it cannot start again until
	// its run finishes.
	queue scheduleQueue
	// wake has a buffer of one, so set and remove never block, and
	// several calls between two passes wake the loop once.
	wake     chan struct{}
	finished chan finishedRun
}

type scheduleEntry struct {
	key      string
	interval time.Duration
	prober   prober
	due      time.Time
	// index is the entry's place in the queue, or -1 while it runs.
	index   int
	running bool
	// removed marks a running entry whose key was removed. The loop
	// drops its result and deletes it when the run finishes.
	removed bool
	// generation counts how often the key was removed and set again
	// during one run, so the run's result, which belongs to the removed
	// key, is not reported for the new one.
	generation int
}

// finishedRun is one run's result on its way back to the loop.
type finishedRun struct {
	entry      *scheduleEntry
	generation int
	started    time.Time
	result     probeResult
}

// newProbeSchedule builds a schedule. now is the clock. report is
// called from the schedule's goroutine with the key a prober was
// registered under.
func newProbeSchedule(now func() time.Time, report func(ctx context.Context, key string, result probeResult)) *probeSchedule {
	s := &probeSchedule{
		now:      now,
		report:   report,
		entries:  map[string]*scheduleEntry{},
		wake:     make(chan struct{}, 1),
		finished: make(chan finishedRun),
	}
	timer := time.NewTimer(0)
	timer.Stop()
	s.timerAt = func(deadline time.Time) <-chan time.Time {
		timer.Reset(deadline.Sub(s.now()))
		return timer.C
	}
	return s
}

// set registers or replaces the prober at key. A new key is due at
// once. A replaced key keeps its due time if interval did not change.
// A changed interval makes the key due at once, because Healthchecks
// takes the new interval as the check's timeout right away, and the
// last ping may already be older than a shorter timeout.
func (s *probeSchedule) set(key string, interval time.Duration, p prober) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	defer s.poke()
	entry, exists := s.entries[key]
	if !exists {
		entry = &scheduleEntry{key: key, interval: interval, prober: p, due: s.now()}
		s.entries[key] = entry
		heap.Push(&s.queue, entry)
		return
	}
	if entry.removed {
		// The key was removed during a run and is new again. The run
		// finishes first, then the key is due at once.
		entry.removed = false
		entry.generation++
		entry.interval = interval
		entry.prober = p
		entry.due = s.now()
		return
	}
	entry.prober = p
	if entry.interval == interval {
		return
	}
	entry.interval = interval
	entry.due = s.now()
	if !entry.running {
		heap.Fix(&s.queue, entry.index)
	}
}

// remove stops probing key. A key that is not registered is a no-op.
func (s *probeSchedule) remove(key string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	entry, exists := s.entries[key]
	if !exists || entry.removed {
		return
	}
	if entry.running {
		entry.removed = true
		return
	}
	heap.Remove(&s.queue, entry.index)
	delete(s.entries, key)
	s.poke()
}

func (s *probeSchedule) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run probes until ctx ends.
func (s *probeSchedule) run(ctx context.Context) {
	for {
		var timer <-chan time.Time
		if next, ok := s.startDue(ctx); ok {
			timer = s.timerAt(next)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-timer:
		case run := <-s.finished:
			if s.finish(run) {
				s.report(ctx, run.entry.key, run.result)
			}
		}
	}
}

// startDue starts every entry that is due, each in its own goroutine,
// and returns the due time of the earliest entry left in the queue.
func (s *probeSchedule) startDue(ctx context.Context) (time.Time, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	now := s.now()
	for s.queue.Len() > 0 && !s.queue[0].due.After(now) {
		entry := heap.Pop(&s.queue).(*scheduleEntry)
		entry.running = true
		go s.probe(ctx, entry, entry.prober, entry.generation, now)
	}
	if s.queue.Len() == 0 {
		return time.Time{}, false
	}
	return s.queue[0].due, true
}

func (s *probeSchedule) probe(ctx context.Context, entry *scheduleEntry, p prober, generation int, started time.Time) {
	result := p.probe(ctx)
	select {
	case s.finished <- finishedRun{entry, generation, started, result}:
	case <-ctx.Done():
	}
}

// finish puts a run's entry back in the queue, due one interval after
// the run started, so a slow target does not push later runs back.
// It reports whether the result belongs to a key that is still set.
func (s *probeSchedule) finish(run finishedRun) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	entry := run.entry
	entry.running = false
	if entry.removed {
		delete(s.entries, entry.key)
		return false
	}
	current := run.generation == entry.generation
	if current {
		entry.due = run.started.Add(entry.interval)
	}
	heap.Push(&s.queue, entry)
	return current
}

// scheduleQueue is a min-heap of entries by due time, for
// container/heap.
type scheduleQueue []*scheduleEntry

func (q scheduleQueue) Len() int           { return len(q) }
func (q scheduleQueue) Less(i, j int) bool { return q[i].due.Before(q[j].due) }

func (q scheduleQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *scheduleQueue) Push(x any) {
	entry := x.(*scheduleEntry)
	entry.index = len(*q)
	*q = append(*q, entry)
}

func (q *scheduleQueue) Pop() any {
	old := *q
	entry := old[len(old)-1]
	old[len(old)-1] = nil
	entry.index = -1
	*q = old[:len(old)-1]
	return entry
}
