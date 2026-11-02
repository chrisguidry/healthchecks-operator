---
name: hold-during-a-disruption
description: "Pause every check in a ClusterProject while a condition on another Kubernetes object matches, such as a rollout that reboots each node, and resume them when it stops: write spec.hold, grant the operator read access to the object, and read the Holding condition. Use when planned work fails checks on purpose and should not notify, or when checks stay paused or resume as down after a hold."
---

This skill follows the README at https://github.com/chrisguidry/healthchecks-operator#hold-checks-during-a-disruption. Before the first command, run `kubectl config current-context` and confirm that it names the cluster the person means.

# Hold checks during a disruption

A `hold` on a `ClusterProject` pauses the project's checks while one
condition on one object matches, and resumes them when it stops
matching. It pauses only the checks the operator makes from `Check`
resources, never the heartbeat.

## Find the condition

Pick the object whose condition is true exactly while the work runs,
and read its conditions:

```sh
kubectl get <resource> <name> [-n <namespace>] -o jsonpath='{range .status.conditions[*]}{.type} {.status} {.reason}{"\n"}{end}'
```

Name the `reason` when the status alone does not say the work is under
way. A Deployment keeps `Progressing` at `True` after its rollout
finishes, with the reason `NewReplicaSetAvailable`, so a hold on
`Progressing=True` with no reason would match for good. An absent
`reason` matches any reason.

## Write the hold

```yaml
apiVersion: healthchecks.guid.foo/v1alpha1
kind: ClusterProject
metadata:
  name: internal
spec:
  # url, apiKeySecret, and channels as before
  hold:
    while:
      apiVersion: example.com/v1
      resource: rollouts
      # namespace: example   # for a namespaced object
      name: fleet
      condition: {type: Progressing, status: "True", reason: RollingOut}
    maxDuration: 3h
```

`while` names the object by its resource, the plural an RBAC rule uses,
not its kind. `maxDuration` is required: when the hold has matched that
long, the operator resumes the checks, and the next hold starts only
after the condition stops matching and matches again. Set it a little
longer than the work ever takes.

## Grant read access

The operator's `ClusterRole` does not cover the object a hold names.
Grant it with a `ClusterRole`, even for a namespaced object: the
operator lists and watches the resource across every namespace, so a
`Role` is not enough. Put the operator's namespace in the subject:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: healthchecks-operator-hold
rules:
  - apiGroups: [example.com]
    resources: [rollouts]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: healthchecks-operator-hold
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: healthchecks-operator-hold
subjects:
  - kind: ServiceAccount
    name: healthchecks-operator
    namespace: healthchecks-operator
```

## Read the Holding condition

```sh
kubectl get clusterprojects -o wide
```

The `Holding` column and condition say what the hold does now. While
`Holding` is `True`, its `lastTransitionTime` says when the hold
started; while it is `False`, when the hold last ended or failed.

| Status, reason | Meaning |
|---|---|
| `True`, `Matched` | The condition matches, and the project's checks are paused. |
| `False`, `NotMatched` | The condition does not match. Nothing is paused. |
| `False`, `Expired` | The hold matched for `maxDuration`, and the checks resumed. It stays `Expired` until the condition stops matching. |
| `False`, `TargetNotFound` | The object does not exist. Nothing is paused. |
| `False`, `TargetUnreadable` | The operator cannot read the object, usually for want of the RBAC above. Nothing is paused. |
| `False`, `InvalidHold` | `maxDuration` does not parse. |

A hold that cannot read its object pauses nothing, so test it before the
work starts: the condition should read `NotMatched`, not
`TargetUnreadable` or `TargetNotFound`.

## What happens to the checks

While the hold matches, the operator pauses each check in the project
that is not already down or paused, and sets `manual_resume` on it
first. Healthchecks records every ping as ignored and sends no alert.
Probes go on during the hold.

When the hold ends, the operator clears `manual_resume` and resumes each
paused check:

- An `http`, `tls`, `tcp`, or `dns` check runs its probe at once. The
  first pass from a probe that started after the hold resumes it
  quietly. A failure goes to `/log` and the check stays paused, because
  the restarted workloads may still be starting. If no probe passes
  within the check's interval plus its `grace`, the next failure goes
  to `/fail` and the check goes down, which notifies.
- A `cronJob` check gets its latest Job's result.
- A `ping` check gets a success, and its timer starts again.

A check that stays paused after a hold has not passed a probe yet; read
its `Passing` condition for the reason. The hold neither pauses nor
resumes a check that was already down or paused when it started.

`manual_resume` is the setting "Ignore the ping, stay in the paused
state" under "Pinging a Paused Check" in a check's filtering rules in
Healthchecks. The operator leaves it alone on every check a hold does
not pause. Without it, any ping, such as the next passing probe, takes
a paused check out of the paused state. So to pause a probe check by
hand and keep it paused, choose that setting when you pause it. A check
the hold paused ends the hold with the setting cleared, Healthchecks'
default, so choose it again if you had chosen it before.
