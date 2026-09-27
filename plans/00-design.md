# healthchecks-operator design

healthchecks-operator creates checks on a [Healthchecks](https://healthchecks.io)
instance from Kubernetes resources, and it sends the pings for those checks
itself. It probes HTTP endpoints and TLS certificates on a timer, and it
reports the result of each run of a CronJob.

## What it replaces

On a cluster without this operator, each monitored thing needs three parts:

- Terraform that creates the check and writes its ping URL into a ConfigMap.
- A CronJob that reads the ConfigMap: a site prober, a certificate checker,
  or a `curl` container at the end of a backup Job.
- A Healthchecks schedule that someone keeps in step with the CronJob's
  schedule by hand.

With the operator, each monitored thing is one `Check` resource next to the
workload it watches. The operator creates the check, runs the probe, and
sends the ping. No workload reads a ping URL.

## Resources

The API group is `healthchecks.guid.foo`, version `v1alpha1`.

### ClusterProject

A `ClusterProject` is one project on one Healthchecks instance. It is
cluster-scoped. Every Healthchecks v3 API key belongs to exactly one project,
and there are no account-wide keys, so the project is the unit the operator
talks to.

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

- `apiKeySecret` names a Secret in the operator's namespace. A cluster-scoped
  resource has no namespace of its own, so the operator reads its Secrets
  from one fixed namespace. cert-manager's `ClusterIssuer` uses the same
  rule.
- The key must be a read-write key. A read-only key does not return
  `ping_url`, and it cannot create checks.
- `channels` lists the default integrations for every check in the project,
  by name. The operator resolves each name to an ID with
  `GET /api/v3/channels/`. A name that matches no channel sets `Ready` to
  `False` with the name in the message. Names are used because channel IDs
  differ between instances and change when a channel is made again.
- The operator does not create channels. The v3 API has no endpoint to
  create, update, or delete an integration.

The name `ClusterProject` leaves `Project` free for a namespaced kind later.
A namespaced `Project` would let each namespace hold its own API key, for
clusters where a namespace is a team or a customer. The operator does not
have it yet.

### Check

A `Check` is one Healthchecks check and the probe that feeds it. It is
namespaced.

```yaml
apiVersion: healthchecks.guid.foo/v1alpha1
kind: Check
metadata:
  name: website
  namespace: example
spec:
  projectRef:
    kind: ClusterProject
    name: internal
  displayName: example.com        # optional; default is "<namespace>/<name>"
  slug: example-website           # optional; default is "<namespace>-<name>"
  description: Public website
  tags: [internet, website]
  grace: 15m
  channels: [Pushover]            # optional; replaces the project's list
  http: { ... }                   # exactly one of http, tls, cronJob
```

- `projectRef.kind` has one legal value today, `ClusterProject`. The field
  exists so that `Project` can be added without a schema change. When
  `Project` exists, a `Check` can name a `Project` only in its own namespace.
- A CEL rule (`x-kubernetes-validations`) requires exactly one of `http`,
  `tls`, and `cronJob`. The Kubernetes volume source uses the same pattern.
- The `Check` does not set a Healthchecks schedule. The operator derives the
  schedule from the probe, so the check and the thing it watches cannot
  disagree.

#### http

The operator sends each request in order on every interval. The check pings
success only if every request meets its expectations.

```yaml
http:
  interval: 5m
  requests:
    - url: http://example.com/
      expect:
        status: [301, 308]
        headers:
          Location: https://example.com/
    - url: https://example.com/
      expect:
        status: [200, 401]
        bodyContains: "Welcome"
    - url: https://proxy.example.net/
      tlsVerify: false
      headers:
        - name: Host
          value: example.com
        - name: Authorization
          valueFrom:
            secretKeyRef: { name: probe-token, key: header }
      expect:
        status: [200, 401]
        bodyContains: "Welcome"
```

- Requests do not follow redirects, so a redirect is something a request can
  expect.
- `headers` is a list, in the shape of a container's `env`. Each entry has a
  `value` or a `valueFrom.secretKeyRef`. The Secret must be in the `Check`'s
  namespace.
- Go's `net/http` client takes the `Host` header from `Request.Host`, not
  from `Request.Header`. The prober copies a `Host` entry into
  `Request.Host`, so that `Host` is written like any other header.
- The Healthchecks timeout is `interval`.
- `interval` must be at least `1m`. Healthchecks asks clients not to ping one
  check more than 5 times a minute.

#### tls

The operator connects, completes a TLS handshake, and reads the leaf
certificate's expiry.

```yaml
tls:
  interval: 24h
  host: example.com:443
  minRemaining: 336h
```

`host` is `host` or `host:port`, and the port is 443 when it is absent.
The check fails if the certificate does not verify, or if less than
`minRemaining` remains before it expires. The Healthchecks timeout is
`interval`.

#### cronJob

The operator reports each run of a CronJob in the `Check`'s namespace.

```yaml
cronJob:
  name: database-backup
```

- When a Job that the CronJob owns starts, the operator pings `/start`. When
  the Job gets the `Complete` condition, the operator pings success. When it
  gets the `Failed` condition, the operator pings `/fail`. The start ping
  lets Healthchecks record how long each run takes.
- The operator copies the CronJob's `schedule` and `timeZone` into the
  Healthchecks check's `schedule` and `tz`. A change to the CronJob changes
  the check on the next reconcile.
- The CronJob's pod template does not change. It needs no ping URL, no
  ConfigMap, and no `curl` container.
- The CronJob must keep at least one finished Job
  (`successfulJobsHistoryLimit` and `failedJobsHistoryLimit` of 1 or more),
  so the operator can find a run that finished while it was down. If either
  limit is 0, `Ready` is `False` and the message says why.
- The operator matches Jobs to the CronJob by owner reference.

## Identity

The slug identifies a `Check`'s check in Healthchecks. Every reconcile of a
`Check` spec is one call to `POST /api/v3/checks/` with `unique: ["slug"]`.
Healthchecks updates the check with that slug if one exists, and creates it
if not. The reconcile needs no stored state, so a lost status, a reinstall,
or a cluster rebuilt from git finds the same check instead of making a
second one.

The operator writes the returned `uuid` and `ping_url` into `.status`. Probes
ping the `ping_url` from status and do not call the management API. If
status is empty, the next reconcile fills it in.

Changing `slug` on a `Check` makes a new check in Healthchecks. The operator
deletes the check at the old slug, which it reads from `.status.slug`.

## Deletion

A `Check` has the finalizer `healthchecks.guid.foo/check`. When the `Check`
is deleted, the operator deletes its check in Healthchecks, then removes the
finalizer. The operator adds and removes the finalizer with a merge patch
pinned to the `resourceVersion` it read, and the finalizer comes off last.
Deleting a namespace deletes its checks and their history.

If the `ClusterProject` is gone or its key does not work, the finalizer
stays, and the `Check` reports why. The operator does not remove a
finalizer while the check still exists in Healthchecks.

## Status

Status is written with server-side apply on the `/status` subresource,
field manager `healthchecks-operator`. Each condition has
`observedGeneration`, and `lastTransitionTime` changes only when the
condition's status changes. The operator does not write a status that is
the same as the current one.

`Check` status:

- `slug`, `uuid`, `pingURL`: the check in Healthchecks.
- `probe`: which probe block the spec sets, `http`, `tls`, or `cronJob`, so
  `kubectl get checks` can show it in a column.
- `lastReportedJob`: the name of the last Job the operator pinged for, for
  a `cronJob` check.
- Conditions:
  - `Ready`: the check exists in Healthchecks and matches the spec.
  - `Passing`: the last probe passed. When it is `False`, the message is
    the failure reason. This condition is not set for a `cronJob` check.

The operator writes status only when something in it changes. A probe
that gets the same result as the one before writes nothing, so a steady
check makes no API writes.

`ClusterProject` status has a `Ready` condition, and the list of resolved
channels.

## Failure reasons

When a probe fails, the ping body is the reason, for example
`https://proxy.example.net/: status 502, want 200 or 401`. Healthchecks
keeps the first 100 kB of each ping body and shows it in the check's log.
The same text is the message of the `Passing` condition.

## Restarts and missed runs

- On start, and after a watch drops, the operator lists every `Check` and
  every Job before it watches again.
- For a `cronJob` check, the operator compares the CronJob's finished Jobs
  with `lastReportedJob`. It pings once for each Job that finished after
  that one, in order. A backup that finished while the operator restarted
  still reports.
- An `http` or `tls` probe that was due while the operator was down runs as
  soon as the operator starts.

## Operator heartbeat

The operator pings a check of its own every minute. The heartbeat names a
`ClusterProject` and a slug in the operator's settings, and it does not use
a `Check`. The heartbeat pings only if the reconcile loop finished a pass in
the last two minutes. The 30-second ticker starts a pass even when nothing
changes, so a stuck loop stops the heartbeat.

When the operator stops, every check it pings goes down one grace period
later, and each sends its own alert. A red heartbeat check tells the reader
that those alerts share one cause. Point the heartbeat at a project on a
different Healthchecks instance from the one the operator watches, so that
it alerts when the cluster itself is down.

## Runtime

The operator follows the patterns of the liken-sh operators
(`equipment-operator`, `media-operator`).

- One module, one flat `package main`. Files are grouped by prefix:
  `project_*.go`, `check_*.go`, `probe_http_*.go`, `probe_tls_*.go`,
  `probe_cronjob_*.go`. Each source file has a `_test.go` file.
- The Healthchecks management API client and the ping sender are the
  package `healthchecks/`, with a fake Healthchecks server for tests in
  `healthchecks/healthcheckstest/`. equipment-operator keeps each protocol
  driver in its own directory in the same way.
- No client-go and no controller-runtime. A small `net/http` Kubernetes
  client reads the in-cluster service account, lists, watches with
  `?watch=true&allowWatchBookmarks=true`, and applies status. A dropped
  watch or a `410 Gone` starts a new list.
- Each watch keeps its collection in a store in memory, keyed by
  namespace/name. The first list fills the store, and every list
  replaces it, so an object deleted while a stream was down is gone
  from it. `ADDED` and `MODIFIED` replace an object, and `DELETED`
  removes it, in the order the events arrive. A `BOOKMARK` moves only
  the resume version. Every store has its first list before the first
  pass.
- A store holds each object trimmed to the operator's Go type for its
  collection, so a Job keeps its metadata, owner references, and status,
  and drops its pod template and managedFields. An object that does not
  decode stays out of the store, and an older copy under its key leaves
  it. The rest of the list or the stream applies. The operator writes one
  line for each object and error.
- An event changes its store and then wakes the reconcile loop, through
  a channel with a buffer of one. Each pass reads `ClusterProject`s,
  `Check`s, CronJobs, and Jobs from the stores, with no request to the
  API server, reconciles each one, and stops the probes of `Check`s that
  are gone. A 30-second ticker also wakes the loop, for the parts of a
  pass that time moves: a backoff that ends, and the heartbeat. A pass
  calls the Healthchecks management API only when a spec, a CronJob
  schedule, or a channel list changed.
- A pass reads a Secret by name from the API server, and writes
  finalizers and status there. A finalizer patch states the
  `resourceVersion` from the store. When the store is behind, the patch
  gets `409 Conflict`, and the pass leaves the object alone. The newer
  version's event wakes the next pass, which reads it from the store.
- The operator watches Jobs in all namespaces and matches each Job to a
  `Check` by owner reference.
- `http` and `tls` probes run from one timer heap in one goroutine. Each
  probe is an entry at its next due time. There is no goroutine and no
  requeue for each `Check`.
- In steady state, the only outbound traffic is probes and pings.
- One replica, `strategy: Recreate`, no leader election. Two pods never run
  at the same time, so no check gets two pings for one probe.

### RBAC

- `ClusterProject`, `Check`: get, list, watch, and patch, and patch on
  `status`.
- `cronjobs`, `jobs`: get, list, watch.
- `secrets`: get only, in all namespaces. The operator reads each Secret by
  name when it needs one, and it does not list or watch Secrets, so it does
  not keep every Secret in memory. A changed Secret takes effect on the next
  probe.

### Probe interface

Each probe kind returns pass or fail with a reason. A probe does not call
Healthchecks. The `check_*.go` files send the pings. A new probe kind is a
new `probe_<kind>_*.go` group, one field on `Check`, and one entry in the
CEL rule.

## Project conventions

Taken from the liken-sh operators:

- CRDs are YAML written by hand in `deploy/`, with CEL rules,
  `subresources.status`, and printer columns. Go types mirror the YAML by
  hand. Go tests load the YAML and check the schema, the CEL cost estimate,
  printer columns, and example objects.
- Tests use `httptest` servers for the Kubernetes API, the Healthchecks API,
  and probe targets. No mocks, no envtest, no fake clientset. The clock is an
  injectable field.
- `.testcoverage.yml` sets a coverage floor that only rises, checked by
  `go-test-coverage`.
- `make test` runs gofmt, a check for packages without tests, `go vet`,
  `go test -race`, and coverage. pre-commit runs the same, plus
  `go mod tidy -diff` and shellcheck.
- The image is built `FROM scratch` with `CGO_ENABLED=0`, and published to
  `ghcr.io/chrisguidry/healthchecks-operator`. A calendar tag
  (`2026.09.27-001`) is a release. A push to `main` publishes a dev build
  that does not move `:latest`.
- Metrics use a private Prometheus registry with the prefix `healthchecks_`,
  on port 9200. Logs are one line on stderr for each operation a person
  caused. Probes and steady passes log nothing.
- `deploy/` is a kustomize base that names the image `:latest`. A cluster
  owner takes it through their own GitOps, for example a Flux
  `GitRepository` at a release tag, and adds their own patches and image
  pin. `deploy/monitoring/` is a kustomize Component with the PodMonitor.
- Documentation is the README. There is no docs site.

## Not in v1

- A namespaced `Project`.
- A probe kind that creates its own Job from a pod template.
- Channel management. It needs a channel API in Healthchecks first.
- Adoption of existing checks. Cutover creates new checks and deletes the
  old ones, so ping history does not carry over.

## Cron syntax

Healthchecks parses cron with [cronsim](https://github.com/cuu508/cronsim),
which accepts less than a CronJob does. cronsim rejects every `@` macro and
the `?` wildcard. Before it copies a CronJob's schedule, the operator
translates them:

| CronJob | Healthchecks |
| --- | --- |
| `@yearly`, `@annually` | `0 0 1 1 *` |
| `@monthly` | `0 0 1 * *` |
| `@weekly` | `0 0 * * 0` |
| `@daily`, `@midnight` | `0 0 * * *` |
| `@hourly` | `0 * * * *` |
| `?` in a day field | `*` |

If Healthchecks rejects a schedule after translation, `Ready` is `False`, and
the message is the text Healthchecks returned.

## Sources

- Healthchecks Management API v3, <https://healthchecks.io/docs/api/>, read
  2026-09-27: project-scoped keys, `unique` on create, the read-only
  `GET /api/v3/channels/`, and the fields that read-only keys omit.
- Healthchecks Pinging API, <https://healthchecks.io/docs/http_api/>, read
  2026-09-27: `/start`, `/fail`, ping bodies up to 100 kB, and the limit of 5
  pings a minute for one check.
- Healthchecks API routes, `hc/api/urls.py` on `master` (v4.5-dev),
  <https://github.com/healthchecks/healthchecks/blob/master/hc/api/urls.py>,
  read 2026-09-27: `channels/` is the only channel route.
- cert-manager cluster resource namespace,
  <https://cert-manager.io/docs/configuration/#cluster-resource-namespace>:
  the rule for where a cluster-scoped issuer reads its Secrets.
- cronsim, <https://github.com/cuu508/cronsim>, `main` on 2026-09-27, and
  the `check_schedule` validator in Healthchecks' `hc/api/views.py`: tested
  by hand, cronsim rejects `@daily`, `@hourly`, `@weekly`, `@monthly`,
  `@yearly`, `@annually`, `@midnight`, and `?`.
- Go `net/http`, `Request.Host` doc comment, Go 1.27: "For client requests,
  Host optionally overrides the Host header to send."
