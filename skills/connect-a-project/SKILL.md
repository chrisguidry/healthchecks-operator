---
name: connect-a-project
description: "Connect healthchecks-operator to one Healthchecks project with a ClusterProject: make a read-write API key, store it in a Secret, name the project's integrations, and read the Ready condition. Use when a cluster needs its first ClusterProject, when adding another Healthchecks project or instance, when rotating an API key, or when a ClusterProject is not Ready."
---

This skill follows the README at https://github.com/chrisguidry/healthchecks-operator#connect-a-healthchecks-project. Before the first command, run `kubectl config current-context` and confirm that it names the cluster the person means.

# Connect a Healthchecks project

Every Healthchecks API key belongs to one project, so the operator
connects to projects, not to accounts. A `ClusterProject` is
cluster-scoped and names one project on one Healthchecks instance:
healthchecks.io or a self-hosted one. A `Check` in any namespace can
name it.

## Make the API key

In the Healthchecks web UI, open the project's **Settings**, and create
an API key under **API Access**. Make a read-write key. A read-only key
cannot create checks, and it omits the fields the operator reads.

Store it in a Secret in the operator's namespace,
`healthchecks-operator` by default. The operator reads the key from its
own namespace only.

```sh
kubectl -n healthchecks-operator create secret generic internal-healthchecks \
  --from-literal=api-key=<key>
```

Do not paste the key into a manifest that goes into git. Use the
cluster's secret tooling, such as SOPS or External Secrets, to write
the same Secret.

## Name the integrations

`channels` names the integrations, such as email, Pushover, or Slack,
that every check in the project notifies. The operator finds each one
by its name in Healthchecks, because channel IDs differ between
instances and change when a channel is made again.

The operator cannot create integrations: the Healthchecks API has no
endpoint for that. Make each one in the web UI first, under the
project's **Integrations**, and copy its name exactly.

A `Check` may set its own `spec.channels`, which replaces the project's
list for that check.

## Declare the project

```yaml
apiVersion: healthchecks.guid.foo/v1alpha1
kind: ClusterProject
metadata:
  name: internal
spec:
  url: https://healthchecks.example.com
  apiKeySecret:
    name: internal-healthchecks
    key: api-key
  channels: [Pushover]
```

`url` is the instance's base URL, `https://healthchecks.io` for the
hosted service. The object's name is the name every `Check` uses in its
`projectRef`, so choose it once.

## Confirm it connected

```sh
kubectl get clusterprojects -o wide
```

The `Reason` column shows only with `-o wide`. `Ready` is `True`
with the reason `Connected` when the key reads and
every channel name matched. `status.channels` lists each name with the
ID it resolved to. Otherwise `Ready` is `False`, and the reason says
what failed:

| Reason | Meaning | Fix |
|---|---|---|
| `SecretUnreadable` | The Secret or its key is missing from the operator's namespace. | Create the Secret in that namespace with the key `apiKeySecret.key` names. The operator reads it again with a backoff from 5 seconds to 5 minutes, or at once after an edit to the ClusterProject. |
| `HealthchecksError` | The call to list channels failed: a wrong URL, a rejected key, or an instance that does not answer. The message has the error. | Check `url`, and that the key is read-write and belongs to this project. The operator retries with a backoff from 5 seconds to 5 minutes. |
| `ChannelNotFound` | A name in `channels` matches no integration. The message names it. | Make the integration in the web UI, or fix the name. The operator lists the channels again on its next pass, within 30 seconds. |

A project whose only fault is a channel name can still delete checks,
but no `Check` that names it creates or updates its check until the
project is `Ready`.

## Rotate the key

Make a new key in the project's settings, write it into the same Secret,
and then revoke the old key. The operator reads the Secret every 5
minutes, at once when the `ClusterProject` changes, and on the next pass
after Healthchecks refuses the old key, so revoking the old key moves
the operator to the new one. No `Check` changes, because checks are
found by slug, not by key.

A read of the Secret that fails for a reason that may clear, such as an
API server that does not answer in time, keeps the key that worked, and
the operator logs `keeping the API key that worked`. A Secret or a key
that is gone makes the project `SecretUnreadable` on the next read.

## Next

The `declare-a-check` skill makes the first check in the project.
