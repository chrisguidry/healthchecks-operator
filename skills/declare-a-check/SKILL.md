---
name: declare-a-check
description: "Declare a Check that makes one check in Healthchecks and feeds it: an http, tls, tcp, or dns probe on a timer, the runs of a CronJob, or a ping URL written into a ConfigMap for a workload that pings itself. Use when something in the cluster or on the network should alert when it stops working, when choosing which probe fits, or when setting grace, tags, channels, slug, or display name."
---

This skill follows the README at https://github.com/chrisguidry/healthchecks-operator#declare-checks. Before the first command, run `kubectl config current-context` and confirm that it names the cluster the person means.

# Declare a check

A `Check` is one check in Healthchecks and the probe that feeds it. Put
it in the namespace of the workload it watches. It names a
`ClusterProject` that is `Ready` (the `connect-a-project` skill) and
exactly one probe block. The operator sets the check's period in
Healthchecks from the probe, so it is never written twice.

## Choose the probe

| Block | Use it for | The operator |
|---|---|---|
| `http` | A web endpoint, its status, headers, and body. | Sends each request every `interval`. |
| `tls` | A certificate that must not expire. | Connects to `host` every `interval` and checks the time left. |
| `tcp` | A port that must be open, or must stay shut, or a server banner. | Opens each connection every `interval`. |
| `dns` | What one DNS server answers for a name. | Asks each server every `interval`. |
| `cronJob` | A CronJob in the same namespace. | Pings `/start` when a Job starts and success or `/fail` when it ends. |
| `ping` | A workload that pings Healthchecks itself. | Creates the check and writes its ping URL to a ConfigMap. It sends no ping. |

Every `interval` is at least `1m`. The API server rejects a `Check` that
names no probe or more than one.

## Fields on every check

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
  # channels: [Email]     # replaces the project's channels for this check
  # description: ...
  # slug: example-website # default <namespace>-<name>
  # displayName: ...      # default <namespace>/<name>
  http: ...
```

`grace` is how long a failure may last before anyone hears about it,
and it is `1h` when absent, the web UI's default. A
failed probe goes to the check's `/log`, which records the reason and
leaves the check up. The check goes down only when no probe passes
within its interval plus `grace`, so a failure the next probe recovers
from never notifies. A check's very first result goes to `/fail`
instead, so a new check that fails from the start goes down at once.

The slug identifies the check in Healthchecks. The operator creates or
updates by slug, so a reinstall finds the checks it made before.
Changing `slug` later deletes the check at the old slug, with its
history, and makes a new one. Changing `projectRef` moves the check:
the operator deletes it from the old project before it creates it in
the new one.

Deleting a `Check` deletes its check in Healthchecks, with its history.

## http

```yaml
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

Requests do not follow redirects, so a redirect is something to expect.
`followRedirects: true` follows them and checks the last response. A
header takes `value` or `valueFrom`, which reads a Secret in the
`Check`'s namespace. `tlsVerify: false` skips certificate verification.
`name` labels a request in the failure reason in place of its URL. The
operator sends every request even when one fails, and the check passes
only when all of them get what they expect.

## tls

```yaml
  tls:
    interval: 24h
    host: example.com        # port 443 when absent
    minRemaining: 336h
```

Fails when the certificate does not verify, or when less than
`minRemaining` is left before it expires.

## tcp

```yaml
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

With no expectation, the port must accept. `bannerContains` reads the
first bytes the server sends, for protocols where the server speaks
first. `closed: true` passes when the port refuses or does not answer,
and it cannot be combined with `bannerContains`. A host name that does
not resolve fails either way.

## dns

```yaml
  dns:
    interval: 5m
    queries:
      - server: 192.0.2.53     # host or host:port, port 53 when absent
        lookup: www.example.com
        expect:
          answers: [192.0.2.10, 192.0.2.11]
      - server: 192.0.2.53
        lookup: 192.0.2.10
        type: PTR
        expect:
          answers: [www.example.com]
```

The probe asks the server exactly what is written, with no search
domains and no hosts file. `type` is `A` (the default), `AAAA`, `CNAME`,
`NS`, `PTR`, or `TXT`; a `PTR` lookup takes an address. With no
expectation, the answer must hold one record of the type. `answers` is
the exact set, in any order.

## cronJob

```yaml
  cronJob:
    name: database-backup
```

The CronJob is in the `Check`'s namespace and does not change. The
check's schedule and time zone in Healthchecks are copied from it. Keep
its `successfulJobsHistoryLimit` and `failedJobsHistoryLimit` at 1 or
more: the operator reads the finished Jobs to report a run that ended
while it was not running, and `Ready` is `False` with the reason
`CronJobHistory` when a limit is 0.

## ping

```yaml
  grace: 1h
  ping:
    configMap:
      name: database-backup-healthcheck
      key: HEALTHCHECK_URL     # the default
    schedule: "0 3 * * *"
    timeZone: America/New_York
```

Set exactly one of `schedule`, a cron expression, and `timeout`, a
duration of at least `1m`. `timeZone` goes only with `schedule`. The
workload reads the URL from the ConfigMap, for example with
`envFrom: [{configMapRef: {name: database-backup-healthcheck}}]` and
`curl -fsS "$HEALTHCHECK_URL"` after its work succeeds.

Without `configMap`, read the URL with
`kubectl -n <namespace> get check <name> -o jsonpath='{.status.pingURL}'`. The
ConfigMap is owned by the `Check` and deleted with it. A ConfigMap that
another `Check` holds is not written, so two `Check`s cannot share
one. Recreating a `ping` `Check` under the same name makes a new check
with a new ping URL, so edit it in place instead. A `ping` check has no
`Passing` condition; Healthchecks shows whether it is up.

## Confirm it works

```sh
kubectl get checks -A -o wide
```

The `Reason` column shows only with `-o wide`. `Ready` is `True`
with the reason `Synced` once the check exists in
Healthchecks; `status.slug` and `status.pingURL` name it. `Passing`
shows the last probe's result, and its message is the failure reason
the check's log in Healthchecks also shows. The `troubleshoot` skill
covers a check that is not `Ready`.
