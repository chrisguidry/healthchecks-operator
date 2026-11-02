---
name: install
description: "Install healthchecks-operator from its kustomize base, pin it to a release, give it a heartbeat check, and watch a leader take the Lease. Use when a cluster has no ClusterProject or Check resources yet, when upgrading the operator or running a development build, or when removing it."
---

This skill follows the README at https://github.com/chrisguidry/healthchecks-operator#install. Before the first command, run `kubectl config current-context` and confirm that it names the cluster the person means.

# Install the operator

`deploy/` in the repository is a kustomize base. It installs:

- the `ClusterProject` and `Check` CRDs, in the group
  `healthchecks.guid.foo`;
- the `healthchecks-operator` `ServiceAccount`, `ClusterRole`, and
  `ClusterRoleBinding`, and a `Role` for the leader `Lease`;
- the `healthchecks-operator` `Deployment`, with two replicas.

The base sets the namespace `healthchecks-operator` and does not create
it. Create it yourself, or set `namespace` in your own kustomization to
one you already run. The `ServiceAccount` and the binding's subject
follow it. The base leaves the namespace out so that removing the
operator never deletes a namespace other workloads share.

You need `kubectl` with cluster-admin rights, because the base creates
CRDs and a `ClusterRole`.

## Pick a release

Releases are calendar versions, `YYYY.MM.DD-NNN`, and each one is a git
tag and an image tag of the same name. List them:

```sh
git ls-remote --tags https://github.com/chrisguidry/healthchecks-operator
```

Pin the base and the image to the same version. The base names
`ghcr.io/chrisguidry/healthchecks-operator:latest`, and an unpinned
image moves on every release.

## Apply the base

In your own kustomization:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - https://github.com/chrisguidry/healthchecks-operator//deploy?ref=<version>

images:
  - name: ghcr.io/chrisguidry/healthchecks-operator
    newTag: <version>
```

With Flux, point a `GitRepository` at the tag and a `Kustomization` at
`./deploy`, with the same `images` entry. The README's Install section
has the full pair.

For a one-time install from a checkout of the tag:

```sh
kubectl create namespace healthchecks-operator
kubectl apply -k deploy
```

Watch it start, and see which pod leads:

```sh
kubectl -n healthchecks-operator rollout status deployment/healthchecks-operator
kubectl -n healthchecks-operator get lease healthchecks-operator
```

One replica leads and does all the work. The other waits. When the
leader stops for a rollout or a drain, the standby takes over in about
two seconds. When the leader's node fails, the standby takes over about
fifteen seconds later. The `healthchecks_leader` metric is 1 on the
leader, and each pod's log says when it waits, leads, or loses the
`Lease`.

The operator does nothing until a `ClusterProject` exists. The
`connect-a-project` skill is the next step.

## Metrics

The pods serve Prometheus metrics on port 9200, named `metrics`. If the
cluster runs the Prometheus Operator, add the PodMonitor component:

```yaml
components:
  - https://github.com/chrisguidry/healthchecks-operator//deploy/monitoring?ref=<version>
```

## Add a heartbeat

When the operator stops, every check it pings goes down one grace period
later, which looks like an outage of everything at once. A heartbeat
check tells the two apart. Patch two environment variables onto the
`operator` container:

- `HEARTBEAT_PROJECT`: the name of a `ClusterProject`.
- `HEARTBEAT_SLUG`: the slug of the heartbeat check in that project.

```yaml
patches:
  - target:
      kind: Deployment
      name: healthchecks-operator
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value: {name: HEARTBEAT_PROJECT, value: external}
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value: {name: HEARTBEAT_SLUG, value: healthchecks-operator}
```

The operator creates that check and pings it every minute while its
reconcile loop runs. Put it in a project on a different Healthchecks
instance from the one the cluster runs, so it still alerts when that
cluster is down. A hold never pauses the heartbeat.

## Run a development build

Every push to `main` publishes an image versioned from the latest
release plus a suffix, such as `2026.09.27-001-dev-003-abcdef01`. It
never moves `:latest`. Pin the base to the full commit sha and the
image to that version; the build's step summary in GitHub Actions
prints both lines.

## Remove the operator

Delete every `Check` first, while the operator runs. Each `Check` holds
the finalizer `healthchecks.guid.foo/check`, and the operator removes it
only after it deletes the check from Healthchecks, with its history.
To keep the checks in Healthchecks, the operator must not run when the
`Check`s go: remove the `Deployment`, then remove the finalizer from
each `Check` by hand before deleting the CRDs.

```sh
kubectl delete checks --all -A
kubectl delete -k deploy
```

The operator creates its `Lease` itself, so the base does not hold it.
Delete it with
`kubectl -n healthchecks-operator delete lease healthchecks-operator`,
or delete the namespace if nothing else runs there.
