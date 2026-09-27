package main

// The tests of how a ping Check hands over its ConfigMap: a rename, a
// ConfigMap another object controls, and a ConfigMap that holds keys
// another writer set.

import (
	"net/http"
	"strings"
	"testing"
)

// renameConfigMap points the backup Check at another ConfigMap.
func (h *harness) renameConfigMap(name string) {
	h.editCheck("example", "database-backup", func(spec map[string]any) {
		spec["ping"].(map[string]any)["configMap"] = map[string]any{"name": name}
	})
}

// requestIndex is the position of the first request of method to path.
func (h *harness) requestIndex(method, path string) int {
	for index, request := range h.api.requests() {
		if request.Method == method && request.Path == path {
			return index
		}
	}
	return -1
}

// The new ConfigMap exists before the old one goes, so a workload that
// reads the new name never finds neither.
func TestARenameWritesTheNewConfigMapBeforeItDeletesTheOldOne(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	h.pass()

	h.renameConfigMap("backup-ping")
	h.pass()

	written := h.requestIndex("PATCH", "/api/v1/namespaces/example/configmaps/backup-ping")
	deleted := h.requestIndex("DELETE", "/api/v1/namespaces/example/configmaps/database-backup-healthcheck")
	mustMatch(t, written >= 0 && deleted > written, true)
}

func TestARenameThatCannotWriteKeepsTheOldConfigMap(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	h.pass()
	h.api.refuse("PATCH", "/api/v1/namespaces/example/configmaps/backup-ping", http.StatusForbidden)

	h.renameConfigMap("backup-ping")
	h.pass()

	check := h.check("example", "database-backup")
	mustMatch(t, h.api.read(configMapsResource, "example", "database-backup-healthcheck", &configMap{}), true)
	mustMatch(t, check.Status.ConfigMap, "database-backup-healthcheck")
	mustMatch(t, conditionOf(check, "Ready").Reason, "ConfigMapFailed")
}

// A ConfigMap that another object controls now is not the operator's
// to delete, even when status names it.
func TestARenameLeavesAConfigMapTheCheckDoesNotControl(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	h.pass()
	h.api.update(configMapsResource, "example", "database-backup-healthcheck", func(object map[string]any) {
		metadataOf(object)["ownerReferences"] = []any{map[string]any{"kind": "Deployment", "name": "exporter", "uid": "uid-other", "controller": true}}
	})

	h.renameConfigMap("backup-ping")
	h.pass()

	mustMatch(t, h.api.read(configMapsResource, "example", "database-backup-healthcheck", &configMap{}), true)
	mustMatch(t, h.api.requestCount("DELETE", "/api/v1/namespaces/example/configmaps/database-backup-healthcheck", false), 0)
	mustMatch(t, h.check("example", "database-backup").Status.ConfigMap, "backup-ping")
}

// A ConfigMap with a key the operator did not write belongs partly to
// another writer. The operator gives up its own fields and leaves the
// rest.
func TestARenameReleasesAConfigMapThatHoldsOtherKeys(t *testing.T) {
	h := startHarness(t)
	h.api.create(configMapsResource, map[string]any{
		"metadata": map[string]any{"namespace": "example", "name": "database-backup-healthcheck"},
		"data":     map[string]any{"HEALTHCHECK_URL": "https://hc-ping.example.net/old", "BUCKET": "backups"},
	})
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	h.pass()

	h.renameConfigMap("backup-ping")
	h.pass()

	held := h.heldConfigMap("database-backup-healthcheck")
	mustMatchMap(t, held.Data, map[string]string{"BUCKET": "backups"})
	mustMatch(t, len(held.Metadata.Labels), 0)
	mustMatch(t, len(held.Metadata.OwnerReferences), 0)
	mustMatch(t, h.check("example", "database-backup").Status.ConfigMap, "backup-ping")
}

// Two Checks that name one ConfigMap would each replace the other's
// owner reference on every pass. The second one does not write, and
// says which object controls the ConfigMap.
func TestTwoChecksOnOneConfigMapDoNotFight(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("shared-healthcheck", "")))
	other := pingCheck(backupConfigMap("shared-healthcheck", ""))
	other.Metadata.Name = "database-restore"
	h.api.create(checksResource, other)

	h.pass()
	h.pass()
	h.pass()

	mustMatch(t, h.api.requestCount("PATCH", "/api/v1/namespaces/example/configmaps/shared-healthcheck", false), 1)
	mustMatch(t, conditionOf(h.check("example", "database-backup"), "Ready").Status, ConditionTrue)
	ready := conditionOf(h.check("example", "database-restore"), "Ready")
	mustMatch(t, ready.Reason, "ConfigMapConflict")
	mustMatch(t, ready.Message, "ConfigMap shared-healthcheck is controlled by Check database-backup with uid uid-3, not this Check, so the operator does not write it")
}

// A person who removes the operator's label takes the ConfigMap out of
// the watch. The operator writes it again.
func TestAConfigMapThatLosesItsLabelIsWrittenAgain(t *testing.T) {
	h := startHarness(t)
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	h.pass()

	h.api.update(configMapsResource, "example", "database-backup-healthcheck", func(object map[string]any) {
		delete(metadataOf(object), "labels")
	})
	h.pass()

	mustMatchMap(t, h.heldConfigMap("database-backup-healthcheck").Metadata.Labels, map[string]string{"app.kubernetes.io/managed-by": "healthchecks-operator"})
}

// A check with no ping URL, such as one a read-only key returns, writes
// no ConfigMap with an empty URL in it.
func TestAPingCheckWithNoPingURLWritesNoConfigMap(t *testing.T) {
	h := startHarness(t)
	check := pingCheck(backupConfigMap("database-backup-healthcheck", ""))
	check.Metadata.UID = "uid-check"
	state := &checkState{namespace: "example", name: "database-backup"}

	v := h.c.syncConfigMap(t.Context(), &check, state, &world{configMaps: map[string]configMap{}}, &CheckStatus{})

	mustMatch(t, v.reason, "ConfigMapFailed")
	mustMatch(t, strings.Contains(v.message, "no ping URL"), true)
	mustMatch(t, len(h.api.requests()), 0)
}

// Binary data is data another writer set too, so a ConfigMap with one
// key and binary data is released, not deleted.
func TestARenameReleasesAConfigMapThatHoldsBinaryData(t *testing.T) {
	h := startHarness(t)
	h.api.create(configMapsResource, map[string]any{
		"metadata":   map[string]any{"namespace": "example", "name": "database-backup-healthcheck"},
		"data":       map[string]any{"HEALTHCHECK_URL": "https://hc-ping.example.net/old"},
		"binaryData": map[string]any{"key.bin": "AAEC"},
	})
	h.api.create(checksResource, pingCheck(backupConfigMap("database-backup-healthcheck", "")))
	h.pass()

	h.renameConfigMap("backup-ping")
	h.pass()

	held := h.heldConfigMap("database-backup-healthcheck")
	mustMatchMap(t, held.BinaryData, map[string]string{"key.bin": "AAEC"})
}
