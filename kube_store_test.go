package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// storedVersions returns the resourceVersion of each stored object,
// keyed by namespace/name.
func storedVersions(t *testing.T, store *objectStore) map[string]string {
	t.Helper()
	versions := map[string]string{}
	for _, object := range store.snapshot() {
		meta, err := readStoredMeta(object)
		mustSucceed(t, err)
		versions[meta.key()] = meta.Metadata.ResourceVersion
	}
	return versions
}

// newTestStore builds a store that trims with trim, and returns it
// with the keys of the objects it skipped. An object with no readable
// metadata has an empty key.
func newTestStore(trim func(json.RawMessage) (json.RawMessage, error)) (*objectStore, *[]string) {
	skipped := &[]string{}
	store := newObjectStore(trim, func(key, _ string) {
		*skipped = append(*skipped, key)
	})
	return store, skipped
}

func storedObject(namespace, name, version string) json.RawMessage {
	return json.RawMessage(`{"metadata":{"name":"` + name + `","namespace":"` + namespace + `","resourceVersion":"` + version + `"}}`)
}

// A list holds the whole collection, so replace drops what the list
// does not hold.
func TestReplaceDropsWhatTheListDoesNotHold(t *testing.T) {
	store, _ := newTestStore(trimTo[storedMeta])
	store.put("shop/gear", storedObject("shop", "gear", "1"))
	store.put("shop/bolt", storedObject("shop", "bolt", "2"))

	store.replace([]json.RawMessage{storedObject("shop", "gear", "3")})

	mustMatchMap(t, storedVersions(t, store), map[string]string{"shop/gear": "3"})
}

// jobObject is a Job that started at startTime. A startTime that is
// not a time does not decode into batchJob.
func jobObject(name, version, startTime string) json.RawMessage {
	return json.RawMessage(`{"metadata":{"name":"` + name + `","namespace":"example","resourceVersion":"` + version + `"},"status":{"startTime":"` + startTime + `"}}`)
}

// A Job that does not decode costs only itself: the list's other Jobs
// reach the store, and a relist that finds the same fault reports
// nothing new.
func TestReplaceSkipsAnObjectThatDoesNotDecode(t *testing.T) {
	store, skipped := newTestStore(trimTo[batchJob])
	list := []json.RawMessage{
		jobObject("backup-1", "7", "yesterday"),
		jobObject("backup-2", "8", "2026-09-27T12:00:00Z"),
		json.RawMessage(`{"metadata":"backup-3"}`),
	}

	store.replace(list)
	store.replace(list)

	mustMatchMap(t, storedVersions(t, store), map[string]string{"example/backup-2": "8"})
	mustMatch(t, strings.Join(*skipped, ","), ",example/backup-1")
}

// A snapshot is the caller's to keep: a later change to the store does
// not reach it.
func TestASnapshotKeepsWhatItRead(t *testing.T) {
	store, _ := newTestStore(trimTo[storedMeta])
	store.put("shop/gear", storedObject("shop", "gear", "1"))
	store.put("house/bolt", storedObject("house", "bolt", "2"))
	snapshot := store.snapshot()

	store.remove("shop/gear")
	store.put("house/bolt", storedObject("house", "bolt", "3"))

	mustMatch(t, string(snapshot[0]), string(storedObject("house", "bolt", "2")))
	mustMatch(t, string(snapshot[1]), string(storedObject("shop", "gear", "1")))
}

// fullJob is a Job as the API server sends it, with the pod template,
// the managedFields, and the status fields that the operator does not
// read.
const fullJob = `{
  "apiVersion": "batch/v1",
  "kind": "Job",
  "metadata": {
    "name": "backup-29310720",
    "namespace": "example",
    "uid": "0f6a1c2e-6d0b-4a8e-9a55-1f2d3c4b5a69",
    "resourceVersion": "48213",
    "generation": 1,
    "creationTimestamp": "2026-09-27T12:00:00Z",
    "labels": {"batch.kubernetes.io/controller-uid": "0f6a1c2e-6d0b-4a8e-9a55-1f2d3c4b5a69", "batch.kubernetes.io/job-name": "backup-29310720"},
    "annotations": {"batch.kubernetes.io/cronjob-scheduled-timestamp": "2026-09-27T12:00:00Z"},
    "ownerReferences": [{"apiVersion": "batch/v1", "kind": "CronJob", "name": "backup", "uid": "7c1d9e0a-2b3f-4c5d-8e6f-a0b1c2d3e4f5", "controller": true, "blockOwnerDeletion": true}],
    "managedFields": [
      {"manager": "kube-controller-manager", "operation": "Update", "apiVersion": "batch/v1", "time": "2026-09-27T12:00:00Z", "fieldsType": "FieldsV1", "fieldsV1": {"f:metadata": {"f:annotations": {".": {}, "f:batch.kubernetes.io/cronjob-scheduled-timestamp": {}}, "f:labels": {".": {}}, "f:ownerReferences": {".": {}, "k:{\"uid\":\"7c1d9e0a-2b3f-4c5d-8e6f-a0b1c2d3e4f5\"}": {}}}, "f:spec": {"f:backoffLimit": {}, "f:completionMode": {}, "f:completions": {}, "f:parallelism": {}, "f:suspend": {}, "f:template": {"f:spec": {"f:containers": {"k:{\"name\":\"backup\"}": {".": {}, "f:args": {}, "f:image": {}, "f:imagePullPolicy": {}, "f:name": {}, "f:resources": {}, "f:volumeMounts": {}}}, "f:restartPolicy": {}, "f:volumes": {}}}}}},
      {"manager": "kube-controller-manager", "operation": "Update", "apiVersion": "batch/v1", "time": "2026-09-27T12:01:30Z", "fieldsType": "FieldsV1", "subresource": "status", "fieldsV1": {"f:status": {"f:completionTime": {}, "f:conditions": {}, "f:ready": {}, "f:startTime": {}, "f:succeeded": {}, "f:terminating": {}, "f:uncountedTerminatedPods": {}}}}
    ]
  },
  "spec": {
    "parallelism": 1, "completions": 1, "backoffLimit": 6, "completionMode": "NonIndexed", "suspend": false,
    "selector": {"matchLabels": {"batch.kubernetes.io/controller-uid": "0f6a1c2e-6d0b-4a8e-9a55-1f2d3c4b5a69"}},
    "template": {
      "metadata": {"labels": {"batch.kubernetes.io/controller-uid": "0f6a1c2e-6d0b-4a8e-9a55-1f2d3c4b5a69", "batch.kubernetes.io/job-name": "backup-29310720"}},
      "spec": {
        "restartPolicy": "OnFailure",
        "containers": [{
          "name": "backup",
          "image": "registry.example.com/backup:2026.09.27",
          "args": ["--source", "/data", "--destination", "s3://backups.example.com/example", "--retain", "30d"],
          "env": [{"name": "AWS_REGION", "value": "us-east-1"}, {"name": "AWS_ACCESS_KEY_ID", "valueFrom": {"secretKeyRef": {"name": "backup-credentials", "key": "access-key"}}}],
          "resources": {"requests": {"cpu": "100m", "memory": "256Mi"}, "limits": {"memory": "1Gi"}},
          "volumeMounts": [{"name": "data", "mountPath": "/data", "readOnly": true}],
          "terminationMessagePath": "/dev/termination-log", "terminationMessagePolicy": "File", "imagePullPolicy": "IfNotPresent"
        }],
        "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "data"}}],
        "dnsPolicy": "ClusterFirst", "schedulerName": "default-scheduler", "securityContext": {}, "terminationGracePeriodSeconds": 30
      }
    }
  },
  "status": {
    "startTime": "2026-09-27T12:00:00Z",
    "completionTime": "2026-09-27T12:01:30Z",
    "succeeded": 1, "ready": 0, "terminating": 0,
    "uncountedTerminatedPods": {},
    "conditions": [
      {"type": "SuccessCriteriaMet", "status": "True", "lastProbeTime": "2026-09-27T12:01:30Z", "lastTransitionTime": "2026-09-27T12:01:30Z"},
      {"type": "Complete", "status": "True", "lastProbeTime": "2026-09-27T12:01:30Z", "lastTransitionTime": "2026-09-27T12:01:30Z"}
    ]
  }
}`

// The store holds a Job at the size of what the operator reads, and
// the stored Job decodes to what the whole Job decodes to.
func TestTheStoreTrimsAJob(t *testing.T) {
	var whole batchJob
	mustSucceed(t, json.Unmarshal([]byte(fullJob), &whole))
	store, _ := newTestStore(trimTo[batchJob])

	store.put("example/backup-29310720", json.RawMessage(fullJob))

	stored := store.snapshot()[0]
	var trimmed batchJob
	mustSucceed(t, json.Unmarshal(stored, &trimmed))
	mustMatch(t, reflect.DeepEqual(trimmed, whole), true)
	mustMatch(t, len(stored), 675)
}

// A Check keeps what the finalizer patch and the pass read.
func TestTheStoreKeepsACheckVersionAndFinalizers(t *testing.T) {
	check := httpCheck("example", "website", "https://example.com/")
	check.Metadata.UID = "uid-1"
	check.Metadata.ResourceVersion = "42"
	check.Metadata.Generation = 3
	check.Metadata.Finalizers = []string{checkFinalizer}
	check.Metadata.DeletionTimestamp = "2026-09-27T12:00:00Z"
	check.Status = CheckStatus{Slug: "example-website", UUID: "b1c2", PingURL: "https://hc.example.com/ping/b1c2"}
	object, err := json.Marshal(check)
	mustSucceed(t, err)
	store, _ := newTestStore(trimTo[Check])

	store.put("example/website", object)

	stored, err := decodeSnapshot[Check](store)
	mustSucceed(t, err)
	mustMatch(t, reflect.DeepEqual(stored, []Check{check}), true)
}
