---
name: adopt-an-existing-check
description: "Take over a check that already exists in Healthchecks, made by hand or by Terraform, so it keeps its ping URL and its history: set spec.slug and spec.displayName on a Check, tell Terraform to forget the check without deleting it, and confirm the takeover. Use when moving checks from Terraform's healthchecksio provider or from the web UI onto the operator, or when a workload already pings a URL that must not change."
---

This skill follows the README at https://github.com/chrisguidry/healthchecks-operator#take-over-an-existing-check. Before the first command, run `kubectl config current-context` and confirm that it names the cluster the person means.

# Adopt an existing check

The operator creates or updates a check by its slug. A `Check` whose
slug matches a check that already exists in the project takes that
check over, with its ping URL, its history, and its log. Nothing is
created and nothing is deleted.

## Find the check's slug

In the Healthchecks web UI, open the check and read its slug and its
name. The slug is also the last part of the check's slug-based ping URL,
`<ping endpoint>/<ping key>/<slug>`, and the `slug` field of the check
in `GET /api/v3/checks/`. A check with no slug cannot be found by slug:
give it one in the web UI first. A `Check`'s slug takes only lowercase
letters, digits, and hyphens, and starts and ends with a letter or
digit, so a check whose slug has an underscore or a capital must get a
new slug in the web UI before a `Check` can name it.

Make sure the `ClusterProject` points at the project that holds the
check, with a key from that project. A slug is unique only within one
project.

## Write the Check

Set `spec.slug` to the existing slug, and `spec.displayName` to the
check's name, so the name in Healthchecks does not change. Without
`slug`, the operator uses `<namespace>-<name>` and creates a new check
with a new ping URL.

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
  slug: website
  displayName: Website
  grace: 15m
  http:
    interval: 5m
    requests:
      - url: https://example.com/
        expect:
          status: [200]
```

The operator writes the check's period, grace, tags, description, and
channels from the `Check`, so these replace what the check had. Copy the
values you want to keep into the `Check`.

An `http`, `tls`, `tcp`, or `dns` probe runs at once on a check it takes
over, because the check may have gone longer without a ping than the
probe's interval.

### A check the workload pings

When a workload already pings the check's URL, use a `ping` `Check`
with the same slug. The ping URL stays the same, so the workload keeps
working. To have the operator write the URL into a ConfigMap that
already exists, name it in `ping.configMap`. The operator takes it
over: it writes its key, adds the label
`app.kubernetes.io/managed-by: healthchecks-operator`, and adds an
owner reference to the `Check`, so the ConfigMap is deleted when the
`Check` is. A ConfigMap that another `Check` already holds is not taken
over, and `Ready` is `False` with the reason `ConfigMapConflict`. A
ConfigMap that some other object controls, such as a Helm release or
another operator, cannot be taken over either: `Ready` is `False` with
the reason `ConfigMapFailed` and the API server's error. Name a
ConfigMap of its own for the operator.

## Tell Terraform to forget it

If Terraform made the check with the `healthchecksio` provider, remove
the resource with a `removed` block, which needs Terraform 1.7 or
later, before or after you apply the `Check`, so Terraform forgets the check without deleting it:

```hcl
removed {
  from = healthchecksio_check.website
  lifecycle {
    destroy = false
  }
}
```

Run `terraform plan` and confirm it says the check will be removed from
state and not destroyed. Do the same for a ConfigMap that Terraform
wrote with the URL. Deleting the resource from the Terraform code
without a `removed` block destroys the check, and the operator then
creates a new one with a new ping URL.

## Confirm the takeover

```sh
kubectl -n example get check website -o jsonpath='{.status.slug} {.status.uuid} {.status.pingURL}{"\n"}'
```

`status.uuid` must be the UUID the check had before, the one in its
address in the web UI. `status.pingURL` is the check's UUID-based ping
URL, `<ping endpoint>/<uuid>`, not the slug-based URL; both keep
working, so a workload that pings either one needs no change. A `ping`
`Check` writes the UUID-based URL into its ConfigMap. In the web UI, the
check keeps its earlier log entries, and a new one appears when the
first probe runs.

## Do not

- Do not change `spec.slug` after the takeover. The operator deletes
  the check at the old slug, with its history, and makes a new check.
- Do not delete and recreate the `Check` to fix a mistake. Deleting a
  `Check` deletes its check in Healthchecks. Edit it in place.
