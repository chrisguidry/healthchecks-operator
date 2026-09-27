package main

// A failed call to Healthchecks puts its object on a backoff. Without
// it, a failure feeds itself: the failure changes the Ready message,
// the status write wakes a watch, and the watch starts a pass at once,
// which calls Healthchecks again. During the backoff a pass skips the
// object, keeps its last verdict, and writes nothing. The backstop
// ticker reaches the object again once the delay has passed.

import "time"

const (
	backoffFirst = 5 * time.Second
	backoffLast  = 5 * time.Minute
)

// backoff is the delay before the next call to Healthchecks for one
// object. Its zero value calls at once.
type backoff struct {
	failures int
	until    time.Time
}

// waiting reports whether the delay after the last failure has not
// passed yet.
func (b *backoff) waiting(now time.Time) bool {
	return now.Before(b.until)
}

// failed starts a delay twice as long as the one before, from 5
// seconds to at most 5 minutes.
func (b *backoff) failed(now time.Time) {
	delay := backoffFirst
	for range b.failures {
		delay *= 2
		if delay >= backoffLast {
			delay = backoffLast
			break
		}
	}
	b.failures++
	b.until = now.Add(delay)
}

// succeeded ends the backoff.
func (b *backoff) succeeded() {
	*b = backoff{}
}
