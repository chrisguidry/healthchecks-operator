# healthchecks-operator

healthchecks-operator creates checks on a [Healthchecks](https://healthchecks.io)
instance from Kubernetes resources, and it sends the pings for those checks
itself. It probes HTTP endpoints and TLS certificates on a timer, and it
reports each run of a CronJob. A workload never reads a ping URL.

It works with healthchecks.io and with a self-hosted Healthchecks.

## Install

`deploy/` is a kustomize base. It installs the two CRDs, the RBAC, and one
Deployment into the namespace `healthchecks-operator`, and it names the image
`ghcr.io/chrisguidry/healthchecks-operator:latest`. It does not create the
namespace: create it yourself, or set `targetNamespace` to one you already
run, so removing the operator never deletes a namespace other workloads
share. Take the base through your own GitOps and pin a release tag. With
Flux:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: healthchecks-operator
spec:
  interval: 12h
  url: https://github.com/chrisguidry/healthchecks-operator
  ref:
    tag: 2026.09.27-001
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: healthchecks-operator
spec:
  interval: 1h
  prune: true
  sourceRef:
    kind: GitRepository
    name: healthchecks-operator
  path: ./deploy
  images:
    - name: ghcr.io/chrisguidry/healthchecks-operator
      newTag: 2026.09.27-001
```

Or apply it once with `kubectl create namespace healthchecks-operator` and
`kubectl apply -k deploy`.

`deploy/monitoring/` is a kustomize Component with a PodMonitor for the
metrics on port 9200. Add it to `components` if you run the Prometheus
Operator.

Releases are tagged by date, as `YYYY.MM.DD-NNN`. A push to `main` publishes
a development build that does not move `:latest`.

## Connect a Healthchecks project

Every Healthchecks API key belongs to one project, so the operator connects
to projects, not to accounts. Make a read-write API key in the project's
settings, and put it in a Secret in the operator's namespace:

```sh
kubectl -n healthchecks-operator create secret generic internal-healthchecks \
  --from-literal=api-key=...
```

Then declare the project:

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

`channels` names the integrations that every check in the project notifies.
The operator finds each one by name. It cannot create integrations, because
the Healthchecks API has no endpoint for that, so make them in the web UI
first. `kubectl get clusterprojects` shows whether the key works and every
name matched.

## Declare checks

A `Check` is one check in Healthchecks and the probe that feeds it. It is in
the namespace of the workload it watches, and it has exactly one of
`http`, `tls`, or `cronJob`. The operator sets the check's period in
Healthchecks from the probe, so you never write it twice.

### HTTP

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
  tags: [website]
  grace: 15m
  http:
    interval: 5m
    requests:
      - url: http://example.com/
        expect:
          status: [301, 308]
          headers:
            Location: https://example.com/
      - url: https://proxy.example.net/
        headers:
          - name: Host
            value: example.com
          - name: Authorization
            valueFrom:
              secretKeyRef: { name: probe-token, key: header }
        expect:
          status: [200]
          bodyContains: "Welcome"
```

Every `interval`, the operator sends each request in order. Requests do not
follow redirects, so a redirect is something to expect. The check passes only
if every request gets the status, the headers, and the body text it expects.
A header takes a `value`, or a `valueFrom` that reads a Secret in the
`Check`'s namespace. Set `tlsVerify: false` on a request to skip certificate
verification. The check's timeout in Healthchecks is `interval`.

### TLS certificate expiry

```yaml
spec:
  tls:
    interval: 24h
    host: example.com
    minRemaining: 336h
```

The check fails when the certificate does not verify, or when less than
`minRemaining` is left before it expires. `host` takes an optional port, and
the port is 443 when it is absent.

### A CronJob's runs

```yaml
spec:
  cronJob:
    name: database-backup
```

The operator watches the Jobs of the named CronJob, in the `Check`'s
namespace. It pings `/start` when a Job starts, and success or `/fail` when
the Job finishes, so Healthchecks records how long each run takes. The
check's schedule and time zone in Healthchecks are copied from the CronJob.
The CronJob itself does not change.

Keep `successfulJobsHistoryLimit` and `failedJobsHistoryLimit` at 1 or more.
The operator reads the finished Jobs to report a run that finished while it
was not running.

### Names and identity

The check's slug in Healthchecks is `<namespace>-<name>`, and its name is
`<namespace>/<name>`. Set `spec.slug` and `spec.displayName` to change
them. The slug identifies the check: the operator creates or updates the
check by slug, so a reinstall finds the checks it made before.

Deleting a `Check` deletes its check in Healthchecks, with its history.

### When a probe fails

The operator sends the reason as the body of the failure ping, and
Healthchecks shows it in the check's log:

```
https://proxy.example.net/: status 502, want 200
example.com:443: certificate expires 2026-10-03T00:00:00Z, 6d left, want at least 14d
backup-29842019: BackoffLimitExceeded: Job has reached the specified backoff limit
```

The same reason is the message of the `Check`'s `Passing` condition.
`kubectl get checks -A` lists every check with its `Ready` and `Passing`
state.

## Heartbeat

When the operator stops, every check it pings goes down one grace period
later. To tell that apart from a real outage, give the operator a check of
its own. Set these two environment variables on the Deployment with a
kustomize patch:

- `HEARTBEAT_PROJECT`: the name of a `ClusterProject`.
- `HEARTBEAT_SLUG`: the slug of the heartbeat check in that project.

The operator creates that check and pings it every minute while its
reconcile loop runs. Put it in a project on a different Healthchecks
instance, so it still alerts when the cluster that runs your Healthchecks is
down.

## Development

`make test` runs every check CI runs: gofmt, vet, the race detector, and the
coverage gate. The tests run against fake Kubernetes and Healthchecks API
servers built on `httptest`, so they need no cluster. `make e2e` runs the
operator in a k3s cluster in Docker against the official Healthchecks
image, in about two minutes, and needs only Docker and kubectl. `plans/00-design.md`
is the design, and the source of each fact it depends on.

## License

MIT
