package main

import (
	"testing"
	"time"
)

// settle runs passes until the operator's writes are done, then empties
// the wake channel, so a test reads only the wakes that its own change
// makes.
func (h *harness) settle() {
	h.t.Helper()
	for range 3 {
		h.pass()
	}
	eventually(h.t, "the stores to hold the cluster", h.synced)
	select {
	case <-h.c.wake:
	default:
	}
}

// woke reports whether a change woke the loop, once the stores hold
// it. A watch applies an event to the store just before it wakes the
// loop, so the check waits a moment after the stores agree.
func (h *harness) woke() bool {
	h.t.Helper()
	eventually(h.t, "the stores to hold the cluster", h.synced)
	select {
	case <-h.c.wake:
		return true
	case <-time.After(20 * time.Millisecond):
		return false
	}
}

func TestOnlyAChangeThatAPassReadsWakesIt(t *testing.T) {
	cases := []struct {
		name   string
		change func(h *harness)
		wakes  bool
	}{
		{"a Job's count of active pods", func(h *harness) {
			h.api.update(jobsResource, "example", "backup-1", func(object map[string]any) {
				object["status"].(map[string]any)["active"] = 1
			})
		}, false},
		{"a Job that finishes", func(h *harness) { h.finishRun("backup-1", "Complete") }, true},
		{"a Job in a namespace with no Check", func(h *harness) {
			h.api.create(jobsResource, map[string]any{"metadata": map[string]any{"namespace": "elsewhere", "name": "migrate"}})
		}, false},
		{"the operator's own status write", func(h *harness) {
			h.editCheck("example", "database-backup", func(spec map[string]any) { spec["description"] = "Backup" })
			h.woke()
			h.pass()
		}, false},
		{"a change to a Check's spec", func(h *harness) {
			h.editCheck("example", "database-backup", func(spec map[string]any) { spec["description"] = "Backup" })
		}, true},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			h := startHarness(t)
			h.api.create(cronJobsResource, backupCronJob())
			h.api.create(checksResource, backupCheck())
			h.pass()
			h.startRun("backup-1")
			h.settle()

			one.change(h)

			mustMatch(t, h.woke(), one.wakes)
		})
	}
}

// The operator compares the status it composes with the copy the API
// server holds, so a status that another writer changed is put right.
func TestAStatusThatAnotherWriterChangedIsPutRight(t *testing.T) {
	h := startHarness(t)
	h.api.create(cronJobsResource, backupCronJob())
	h.api.create(checksResource, backupCheck())
	h.settle()
	slug := h.check("example", "database-backup").Status.Slug

	h.api.update(checksResource, "example", "database-backup", func(object map[string]any) {
		object["status"].(map[string]any)["slug"] = "edited"
	})
	h.pass()

	mustMatch(t, h.check("example", "database-backup").Status.Slug, slug)
}

// A pass records the namespaces whose Jobs it reads as it reads the
// Checks, before it reads the Jobs, so a Job that lands just after the
// pass read them, in the namespace of a namespace's first cronJob Check,
// wakes the next pass.
func TestAJobAfterTheReadOfANewCronJobCheckWakes(t *testing.T) {
	h := startHarness(t)
	h.settle()
	h.api.create(cronJobsResource, backupCronJob())
	h.api.create(checksResource, backupCheck())
	h.woke()
	_, err := h.c.read()
	mustSucceed(t, err)

	h.startRun("backup-1")

	mustMatch(t, h.woke(), true)
}
