---
name: troubleshoot
description: "Diagnose healthchecks-operator from the Ready, Passing, and Holding conditions, the operator's log, and its metrics: a ClusterProject or Check that is not Ready, a check that went down or never appeared in Healthchecks, a Check that will not delete, a ping ConfigMap that is not written, and an operator with no leader. Use when a Check or ClusterProject reports a reason other than Connected or Synced, or when Healthchecks and the cluster disagree."
---

This skill follows the README at https://github.com/chrisguidry/healthchecks-operator. Before the first command, run `kubectl config current-context` and confirm that it names the cluster the person means.

# Troubleshoot

The operator posts one Event for each transition of a condition on a
`ClusterProject` or a `Check`, a Warning when `Ready` or `Passing` turns
`False`: `kubectl describe` lists the recent ones, and the Events of a
`ClusterProject` are in the namespace `default`. A fault in a
`ClusterProject` or a `Check` is in a condition's reason and message, and
in one line of the leader's log. A failed ping, a heartbeat fault, and a failed status write are in
the log only. Start with the overview; the `Reason` column shows only
with `-o wide`:

```sh
kubectl get clusterprojects -o wide
kubectl get checks -A -o wide
```

A `Check` cannot be `Ready` while its `ClusterProject` is not, so fix the
projects first.

## A ClusterProject is not Ready

| Reason | Fix |
|---|---|
| `SecretUnreadable` | The Secret `apiKeySecret` names, or its key, is missing from the operator's namespace. The operator reads Secrets from its own namespace only. Reads back off from 5 seconds to 5 minutes. |
| `HealthchecksError` | Listing the project's channels failed; the message has the HTTP error. Check `spec.url`, and that the key is read-write and from this project. Retries back off from 5 seconds to 5 minutes. |
| `ChannelNotFound` | A name in `spec.channels` matches no integration. Names match exactly. Make it in the web UI; the next pass, within 30 seconds, lists the channels again. |

## A Check is not Ready

Read the message: it names the object or the call that failed.

```sh
kubectl -n <namespace> get check <name> -o jsonpath='{range .status.conditions[*]}{.type} {.status} {.reason}: {.message}{"\n"}{end}'
```

| Reason | Fix |
|---|---|
| `ProjectNotFound` | No `ClusterProject` has the name in `projectRef.name`. |
| `ProjectNotReady` | The project is not `Ready`; the message carries the project's message. |
| `ChannelNotFound` | A name in the `Check`'s own `spec.channels` matches no integration in the project. Make it in the web UI, or fix the name. |
| `InvalidSpec` | A duration such as `grace`, `interval`, or `ping.timeout` does not parse. |
| `ProbeInvalid` | The probe cannot run as written: a URL that is not http or https, an address that is not host:port, a DNS type or lookup it does not take. The message names the field. |
| `CronJobNotFound` | No CronJob of `cronJob.name` exists in the `Check`'s namespace. |
| `CronJobHistory` | The CronJob's `successfulJobsHistoryLimit` or `failedJobsHistoryLimit` is 0. Set both to 1 or more. |
| `UpsertFailed` | Healthchecks refused to create or update the check; the message has its answer. Retries back off from 5 seconds to 5 minutes. |
| `DeleteFailed` | Deleting the check in Healthchecks failed: for a deleted `Check`, or at an old slug, or from an old project. |
| `ConfigMapConflict` | The ConfigMap `ping.configMap` names is controlled by another object, such as another `Check`. Name a different ConfigMap. |
| `ConfigMapFailed` | Writing or releasing the ping ConfigMap failed; the message has the API error. |
| `HoldFailed` | Pausing or resuming the check for a hold failed. The next pass tries again. |
| `FinalizerFailed` | Adding or removing the finalizer failed. |

## A check went down in Healthchecks

Read the `Passing` condition. Its message is the failure reason, the
same text the check's log in Healthchecks shows, such as
`https://example.com/: status 500, want 200`. A failed probe goes to
`/log` and the check goes down only when no probe passes within its
interval plus its `grace`, so a check that goes down has failed for at
least that long. Raise `grace` if brief failures are expected.

A `ping` check has no `Passing` condition: the operator does not see
the workload's pings. Look at the check's log in Healthchecks, and at
the workload's own log for the `curl` or the request that pings it.

## A check stays paused

A hold paused it. Read the project's `Holding` condition. After a hold
ends, a probe check stays paused until a probe that started after the
hold passes, or until its interval plus its `grace` pass and the next
failure takes it down. The `hold-during-a-disruption` skill has the
details.

## A Check will not delete

The finalizer `healthchecks.guid.foo/check` stays until the operator
deletes the check in Healthchecks. `Ready` says why it has not:
usually `ProjectNotFound` or `ProjectNotReady`, because the operator
cannot delete a check in a project it cannot reach. Fix the project,
and the delete finishes on the next pass.

To give up on the check in Healthchecks and delete the `Check` anyway,
remove the finalizer by hand. The check stays in Healthchecks, and you
delete it there:

```sh
kubectl -n <namespace> patch check <name> --type=json \
  -p='[{"op":"remove","path":"/metadata/finalizers"}]'
```

## Pings do not reach Healthchecks

A ping that fails, because the instance does not answer or refuses it,
sets no condition. Look for it in the leader's log and in
`healthchecks_pings_total` with `outcome="error"`. A probe that passes
while its check in Healthchecks goes down points here.

## Nothing happens at all

Only the leader reconciles. Find it, and read its log:

```sh
kubectl -n healthchecks-operator get lease healthchecks-operator
kubectl -n healthchecks-operator get pods -l app=healthchecks-operator
kubectl -n healthchecks-operator logs <leader pod>
```

The `Lease`'s holder is the pod's name, an underscore, and eight hex
digits; drop the suffix to get the pod. Each pod logs when it waits,
leads, or loses the `Lease`. The leader logs each change it makes in
Healthchecks. It logs a fault once when it starts, however its text
changes while it lasts, and logs again when it clears. With no holder in the
`Lease`, check the `Role` that grants the `Lease` and the pods' status.

## Metrics

The pods serve metrics on port 9200:

| Metric | Use |
|---|---|
| `healthchecks_leader` | 1 on the pod that leads. Exactly one pod should be 1. |
| `healthchecks_reconcile_errors_total` | Faults the reconcile loop hit. |
| `healthchecks_probes_total` | Probe runs, by probe kind and result. |
| `healthchecks_pings_total` | Pings sent to Healthchecks, by ping and by whether Healthchecks accepted it. |
| `healthchecks_watch_restarts_total` | Restarted watches on the Kubernetes API. A steady climb points at the API server or RBAC. |
| `healthchecks_reconcile_duration_seconds`, `healthchecks_probe_duration_seconds` | How long each pass and each probe take. |
| `healthchecks_passes_total` | Passes, by what woke them: `start`, `event` for a watched change, or `backstop`. |
| `healthchecks_status_writes_total` | Status writes that landed, by kind. A steady operator writes none. |

`/healthz` on port 9200 fails while a pod leads and has not yet listed
what it watches, and the pod's readiness probe reads it.
