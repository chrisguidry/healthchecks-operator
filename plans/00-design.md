# healthchecks-operator design

healthchecks-operator creates checks on a [Healthchecks](https://healthchecks.io)
instance from Kubernetes resources, and it sends the pings for most of those
checks itself. It probes HTTP endpoints and TLS certificates on a timer, and it
reports the result of each run of a CronJob. For a workload that pings
Healthchecks itself, it writes the check's ping URL into a ConfigMap.

## What it replaces

On a cluster without this operator, each monitored thing needs three parts:

- Terraform that creates the check and writes its ping URL into a ConfigMap.
- A CronJob that reads the ConfigMap: a site prober, a certificate checker,
  or a `curl` container at the end of a backup Job.
- A Healthchecks schedule that someone keeps in step with the CronJob's
  schedule by hand.

With the operator, each monitored thing is one `Check` resource next to the
workload it watches. The operator creates the check, runs the probe, and
sends the ping. No workload reads a ping URL, except one that pings
Healthchecks itself, and that one reads it from a ConfigMap the operator
writes.

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
  http: { ... }                   # exactly one of http, tls, cronJob, ping
```

- `projectRef.kind` has one legal value today, `ClusterProject`. The field
  exists so that `Project` can be added without a schema change. When
  `Project` exists, a `Check` can name a `Project` only in its own namespace.
- A CEL rule (`x-kubernetes-validations`) requires exactly one of `http`,
  `tls`, `cronJob`, and `ping`. The Kubernetes volume source uses the same
  pattern.
- An `http`, `tls`, or `cronJob` `Check` does not set a Healthchecks
  schedule. The operator derives the schedule from the probe, so the check
  and the thing it watches cannot disagree. A `ping` `Check` states its own,
  because the operator does not see the workload that pings.

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
  expect. A request with `followRedirects: true` follows them, and holds
  the last response to its expectations, for a page that redirects to a
  sign-in page.
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

#### ping

The workload pings the check itself: a Deployment that pings after its own
backup, a script that sends `/start` and `/fail` with a log as the body, or
an application whose settings take a ping URL. The operator manages the
check, and writes its ping URL into a ConfigMap when the spec names one. It
sends no ping and runs no probe.

```yaml
ping:
  configMap:                        # optional; without it, nothing is written
    name: database-backup-healthcheck
    key: HEALTHCHECK_URL            # optional; default HEALTHCHECK_URL
  schedule: "0 3 * * *"             # exactly one of schedule and timeout
  timeZone: America/New_York        # optional, with schedule only; default UTC
  timeout: 24h                      # Go duration, at least 1m
```

- CEL rules require exactly one of `schedule` and `timeout`, allow
  `timeZone` only with `schedule`, and hold `timeout` to at least `1m`.
- `schedule` and `timeZone` become the check's `schedule` and `tz`, with the
  same translation as a CronJob's schedule (see Cron syntax). `timeout`
  becomes the check's `timeout`.
- The ConfigMap is in the `Check`'s namespace. The operator writes it with
  server-side apply, field manager `healthchecks-operator` and `force`, so it
  takes over a ConfigMap that Terraform or a person created. The apply states
  one key, the ping URL from `.status.pingURL`, the label
  `app.kubernetes.io/managed-by: healthchecks-operator`, and an owner
  reference to the `Check` with `controller: true` and
  `blockOwnerDeletion: false`. The garbage collector deletes the ConfigMap
  after the `Check` is gone. `blockOwnerDeletion: true` would need RBAC on
  the `Check`'s finalizers, and nothing needs the `Check` to wait.
- The operator watches only the ConfigMaps with that label. It applies the
  ConfigMap when the store does not hold one with the ping URL under the key
  and an owner reference from the `Check`, so a steady pass writes nothing. A
  ConfigMap that another tool created has no label, so the store does not
  hold it, and the first pass applies it. The store takes the apply's answer
  at once, so the pass after it does not write again while the watch event
  is on its way.
- The operator does not write a ConfigMap that another object controls,
  and the `Check`'s `Ready` is `False` with a message that names the
  controller. Every `Check` applies as the same field manager, so two
  `Check`s on one ConfigMap would each replace the other's owner reference
  on every pass.
- The operator does not write a ConfigMap while `.status.pingURL` is
  empty, and `Ready` is `False`.
- `.status.configMap` names the ConfigMap the operator wrote. When
  `spec.ping.configMap` names another ConfigMap, or none, or the `Check`
  changes to another kind, the operator writes the new ConfigMap first,
  then releases the one status names. A failed write leaves the old one as
  it is. The operator releases a ConfigMap only when the store holds it
  with this `Check` as its controller. One that holds a key the operator
  did not write gets an apply that states no fields, which removes the
  key, the label, and the owner reference that the operator owns, and
  leaves the rest. Any other one is deleted with `preconditions.uid` set to
  the uid in the store, so an object made again under the name stays.
- The operator does not see the workload's pings, so a `ping` check has no
  `Passing` condition. A `Passing` condition that an earlier kind left is
  removed.

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
- `probe`: which probe block the spec sets, `http`, `tls`, `cronJob`, or
  `ping`, so `kubectl get checks` can show it in a column.
- `lastReportedJob`: the name of the last Job the operator pinged for, for
  a `cronJob` check.
- `configMap`: the name of the ConfigMap the operator wrote the ping URL
  into, for a `ping` check.
- Conditions:
  - `Ready`: the check exists in Healthchecks and matches the spec.
  - `Passing`: for `http` and `tls`, the last probe passed. For `cronJob`,
    the last run the operator reported succeeded. When it is `False`, the
    message is the failure reason, the same text as the ping body. A
    `cronJob` check has no `Passing` until the operator reports its first
    finished run. A `ping` check has no `Passing`.

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
  `Check`s, CronJobs, Jobs, and the operator's ConfigMaps from the stores, with no request to the
  API server, reconciles each one, and stops the probes of `Check`s that
  are gone. A 30-second ticker also wakes the loop, for the parts of a
  pass that time moves: a backoff that ends, and the heartbeat. A pass
  calls the Healthchecks management API only when a spec, a CronJob
  schedule, or a channel list changed.
- A pass reads a Secret by name from the API server, and writes
  finalizers, status, and a `ping` check's ConfigMap there. A finalizer patch states the
  `resourceVersion` from the store. When the store is behind, the patch
  gets `409 Conflict`, and the pass leaves the object alone. The newer
  version's event wakes the next pass, which reads it from the store.
- The operator watches Jobs in all namespaces and matches each Job to a
  `Check` by owner reference.
- The operator watches ConfigMaps in all namespaces with the label selector
  `app.kubernetes.io/managed-by=healthchecks-operator`, so its memory holds
  the ConfigMaps it wrote and no others.
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
- `configmaps`: list, watch, create, patch, and delete, in all namespaces.
  patch is the server-side apply of a `ping` check's ConfigMap, and the API
  server authorizes an apply to a ConfigMap that does not exist as create.
  delete removes the one it wrote before when the name changes. RBAC cannot limit a list or
  a watch to a label, so these verbs reach every ConfigMap. The label
  selector on the watch keeps the others out of memory.

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
