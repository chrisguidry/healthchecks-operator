# healthchecks-operator

healthchecks-operator creates checks on a [Healthchecks](https://healthchecks.io)
instance from Kubernetes resources, and it sends the pings for most of those
checks itself. It probes HTTP endpoints, TLS certificates, TCP ports, and DNS
answers on a timer, and it reports each run of a CronJob. For a workload that pings Healthchecks itself,
the operator writes the check's ping URL into a ConfigMap.

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
`http`, `tls`, `tcp`, `dns`, `cronJob`, or `ping`. The operator sets the check's period in
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
      - name: the site through the proxy
        url: https://proxy.example.net/
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
follow redirects, so a redirect is something to expect. Set
`followRedirects: true` on a request to follow them and check the last
response, for a page that redirects to a sign-in page. A followed redirect
keeps a `Host` header only when its `Location` is relative, sends none of
the request's headers to another host, and is never followed from https to
http. Cookies set along the way come back on the next hop. The check passes only
if every request gets the status, the headers, and the body text it expects.
The operator sends every request even when one fails, and the failure reason
names every request that failed and every expectation it missed. A request's
`name` labels it in that reason in place of its URL.
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

### TCP ports

```yaml
spec:
  tcp:
    interval: 5m
    connections:
      - name: ssh
        address: example.com:22
        expect:
          bannerContains: "SSH-2.0-"
      - address: example.com:443
      - name: the old ssh port stays shut
        address: example.com:2222
        expect:
          closed: true
```

Every `interval`, the operator opens each connection in order. With no
expectation, the port must accept the connection. `bannerContains` is text
that the first bytes the server sends must contain, for a protocol where the
server speaks first, such as SSH or SMTP. `closed: true` turns the check
around: the port must refuse the connection or not answer. A host name that
does not resolve fails the connection either way, because it says nothing
about the port. Like an `http` check, a `tcp` check opens every connection
even when one fails, and its reason names every connection that failed.

### DNS answers

```yaml
spec:
  dns:
    interval: 5m
    queries:
      - server: 192.0.2.53
        lookup: www.example.com
        expect:
          answers: [192.0.2.10, 192.0.2.11]
      - name: the public answer
        server: 9.9.9.9
        lookup: www.example.com
        type: AAAA
      - server: 192.0.2.53
        lookup: 192.0.2.10
        type: PTR
        expect:
          answers: [www.example.com]
```

Every `interval`, the operator asks each server its question exactly as
written, with no search domains and no hosts file, so the check sees what
that server answers. `type` is `A`, `AAAA`, `CNAME`, `NS`, `PTR`, or `TXT`,
and the default is `A`. A `PTR` lookup takes an address, the way `dig -x`
does. With no expectation, the answer must hold at least one record of the
type. `answers` is the exact set it must hold, in any order. Addresses match
in any spelling, names match in any case, with or without the final dot, and
a `TXT` record's strings are joined. A failure names the response code, such
as `NXDOMAIN`, or the records that are missing and unexpected. The probe asks
over UDP up to three times, and asks again over TCP when the answer is
truncated.

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

### Pinged by the workload

Some workloads ping Healthchecks themselves: a Deployment that pings after
its own backup, a script that sends `/start` and `/fail` with a log as the
body, or an application whose settings take a ping URL. For a `ping` check,
the operator creates the check and writes its ping URL into a ConfigMap for
the workload to read. It sends no ping and runs no probe.

```yaml
apiVersion: healthchecks.guid.foo/v1alpha1
kind: Check
metadata:
  name: database-backup
  namespace: example
spec:
  projectRef:
    kind: ClusterProject
    name: internal
  grace: 1h
  ping:
    configMap:
      name: database-backup-healthcheck
      key: HEALTHCHECK_URL
    schedule: "0 3 * * *"
    timeZone: America/New_York
---
apiVersion: batch/v1
kind: Job
metadata:
  name: database-backup
  namespace: example
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: backup
          image: registry.example.com/database-backup
          command: [sh, -c, 'backup.sh && curl -fsS "$HEALTHCHECK_URL"']
          envFrom:
            - configMapRef:
                name: database-backup-healthcheck
```

Set exactly one of `schedule` and `timeout`. `schedule` is a cron expression
that becomes the check's schedule in Healthchecks, with the same translation
of `@daily` and `?` that a CronJob's schedule gets. `timeZone` goes with
`schedule`, and is UTC when it is absent. `timeout` is a duration of at
least `1m` that becomes the check's timeout.

`configMap` is optional. Without it, the operator writes nothing, and you
copy the ping URL from `kubectl get check -o jsonpath='{.status.pingURL}'`.
`key` is `HEALTHCHECK_URL` when it is absent. The ConfigMap is in the
`Check`'s namespace. The operator writes it with server-side apply, labels
it `app.kubernetes.io/managed-by: healthchecks-operator`, and gives it an
owner reference to the `Check`, so it is deleted when the `Check` is
deleted. When `configMap` names another ConfigMap, or none, the operator
writes the new one first, then deletes the one it wrote before. If that
one holds keys the operator did not write, the operator removes only its
key, its label, and its owner reference. A ConfigMap that another object
controls is not written: the `Check` is not `Ready`, and the message names
that object. So two `Check`s cannot share one ConfigMap.

A ConfigMap that already exists, for example one that Terraform wrote, is
taken over: the operator writes the ping URL into it and adds the label
and the owner reference. Remove it from Terraform with a `removed` block,
the same way as the check itself. To keep the check's ping URL and
history, set `spec.slug` to the existing check's slug, as in
[Take over an existing check](#take-over-an-existing-check). Without it,
the operator creates a new check with a new ping URL.

Deleting a `ping` `Check` deletes its check in Healthchecks and its
ConfigMap, through the ConfigMap's owner reference, even when the
ConfigMap holds keys other writers set. A `Check` made again under the same name creates a new check
with a new ping URL. Pods that are already running keep the old URL, and
their pings go nowhere. A new pod that reads the ConfigMap does not start
until the operator writes the new ConfigMap. To change a `ping` `Check`,
edit it in place.

The operator does not see the workload's pings, so a `ping` check has no
`Passing` condition. Healthchecks shows whether the check is up.

### Names and identity

The check's slug in Healthchecks is `<namespace>-<name>`, and its name is
`<namespace>/<name>`. Set `spec.slug` and `spec.displayName` to change
them. The slug identifies the check: the operator creates or updates the
check by slug, so a reinstall finds the checks it made before.

Deleting a `Check` deletes its check in Healthchecks, with its history.

### Take over an existing check

A `Check` whose slug matches a check that already exists in the project
takes that check over, with its ping URL and its history. Set `spec.slug`
to the existing check's slug, and `spec.displayName` to its name. An
`http`, `tls`, `tcp`, or `dns` probe runs at once on a check it takes over, because that
check may have gone longer without a ping than the probe's interval.

If Terraform made the check with the `healthchecksio` provider, replace
the resource with a `removed` block, so Terraform forgets the check
without deleting it:

```hcl
removed {
  from = healthchecksio_check.website
  lifecycle {
    destroy = false
  }
}
```

### When a probe fails

A failed probe does not take the check down at once. The operator sends
the reason to the check's `/log` endpoint, which records it in the
check's log and leaves the check's status alone. The check goes down
only when no probe passes within its interval plus its `grace`, so a
brief failure that the next probe recovers from never notifies. Set
`grace` to how long a failure may last before you want to hear about
it.

Two failures go to `/fail` instead, and take the check down at once:

- A Check's first result. Healthchecks does not time a check that has
  never had a success, so a log would leave a new check that fails from
  the start in its `new` state for good.
- The first result after a hold (see "Hold checks during a
  disruption"). A paused check resumes only on a success or a failure,
  so a log would leave it paused.

In both cases, the reason is the ping's body. Healthchecks shows it in
the check's log:

```
https://proxy.example.net/: status 502, want 200
ssh: banner does not contain "SSH-2.0-", got "220 example.com ESMTP\r\n"
www.example.com A @192.0.2.53: missing 192.0.2.11; unexpected 192.0.2.12
example.com:443: certificate expires 2026-10-03T00:00:00Z, 6d left, want at least 14d
backup-29842019: BackoffLimitExceeded: Job has reached the specified backoff limit
```

When more than one request fails, the reason starts with a count and gives
each failed request a line of its own. One request that misses several
expectations lists them all:

```
2 of 3 requests failed
https://example.com/: status 500, want 200; body does not contain "Welcome"
the site through the proxy: status 502, want 200; body does not contain "Welcome"
```

The same reason is the message of the `Check`'s `Passing` condition. For a
`cronJob` check, `Passing` follows the last run the operator reported. A
`ping` check has no `Passing` condition.
`kubectl get checks -A` lists every check with its probe kind and its `Ready`
and `Passing` state.

## Hold checks during a disruption

Some work fails every check on purpose for a while, such as a rollout that
reboots each node in turn. A `hold` on a `ClusterProject` pauses the
project's checks while a condition on another object matches, and resumes
them when it stops matching:

```yaml
spec:
  hold:
    while:
      apiVersion: example.com/v1
      resource: rollouts
      name: fleet
      condition: {type: Progressing, status: "True", reason: RollingOut}
    maxDuration: 3h
```

`while` names one object by its resource, the way an RBAC rule names it, and
one condition in its `status.conditions`. An absent `reason` matches any
reason. A Deployment keeps `Progressing` at `True` after a rollout finishes,
with the reason `NewReplicaSetAvailable`, so name the reason when the
condition's status alone does not say the work is under way.

While the hold matches, the operator pauses each check in the project that is
not already down or paused, and sets `manual_resume` on it. Healthchecks then
records every ping to the check as ignored, a failure included, and sends no
alert for a missed ping. The operator's probes go on during the hold. When
the condition stops matching, the operator clears `manual_resume` and resumes
each paused check with a ping. A success resumes the check with no
notification, and a failure makes it down, which notifies.

- An `http`, `tls`, `tcp`, or `dns` check runs its probe at once, and the
  first result from a probe that started after the hold resumes it. A probe
  from inside the hold does not count, because it often ran while the thing
  it watches was still coming back.
- A `cronJob` check gets its latest Job's result.
- A `ping` check gets a success, because its workload pings it. Its timer
  starts again, so a workload that stopped during the hold makes it down at
  its next deadline.

`maxDuration` is required. When the hold has matched for that long, the
operator resumes the checks, and the next hold starts only after the
condition stops matching and matches again. A hold whose object the operator
cannot read, or that does not exist, pauses nothing. The `Holding` condition
on the `ClusterProject` says which of these is true, and its
`lastTransitionTime` says when the hold started.

The operator's ClusterRole does not cover the object a hold names. Grant it
yourself:

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

The hold pauses only the checks the operator makes from `Check` resources.
It does not pause the heartbeat.

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

## Replicas and failover

The Deployment runs two replicas, on different nodes when the cluster has
more than one. One leads and does the work, and the other waits. They
elect the leader through the Lease `healthchecks-operator` in the
operator's namespace:

```sh
kubectl -n healthchecks-operator get lease healthchecks-operator
```

When the leader stops for a rollout or a drain, the standby takes over in
about two seconds. When the leader's node fails, the standby takes over
about fifteen seconds later. The `healthchecks_leader` metric is 1 on the
leader. Each pod's log says when it waits, leads, or loses the Lease.

## Development

`make test` runs every check CI runs: gofmt, vet, the race detector, and the
coverage gate. The tests run against fake Kubernetes and Healthchecks API
servers built on `httptest`, so they need no cluster. `make e2e` runs the
operator in a k3s cluster in Docker against the official Healthchecks
image, in about two minutes, and needs only Docker and kubectl. `plans/00-design.md`
is the design, and the source of each fact it depends on.

## License

MIT
