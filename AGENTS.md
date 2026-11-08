# Working on healthchecks-operator

This repository holds the `ClusterProject` and `Check` resources and the
operator that turns them into checks on a Healthchecks instance.
`plans/00-design.md` is the design and the source of each fact it depends
on. Later plans are numbered after it.

`make test` runs every check CI runs.

## Layout

The operator is one flat `package main`, with files grouped by prefix:

- `kube_*.go`: the Kubernetes API client, written against the HTTP API
  with no client-go.
- `api_*.go`: the Go types that mirror the CRDs in `deploy/*-crd.yaml`.
  The YAML is written by hand, and the `api_*_test.go` files check it.
- `reconcile*.go`, `project*.go`, `check*.go`, `heartbeat.go`: the
  reconcile loop.
- `probe*.go`: the probes. A probe returns a pass or a failure with a
  reason, and it never calls Healthchecks.

`healthchecks/` is the Healthchecks API client and the ping sender, and
`healthchecks/healthcheckstest/` is a fake Healthchecks server for tests.

`skills/` holds the Agent Skills, written by hand from the README and the
code. `.claude-plugin/` makes the repository a plugin marketplace for Claude
Code and Codex, and `plugin.json` is the portable manifest Codex prefers
(https://developers.openai.com/codex/plugins/build). A change that alters a
field, a reason, a default, or a command the README states changes the skill
that states it in the same commit. Run `claude plugin validate .` after a
change to the manifests.

## Tests

Tests run against fakes of the real servers: `kube_server_test.go` for the
Kubernetes API, and `healthcheckstest` for Healthchecks. Use them, and
`httptest` servers for probe targets. Do not add mocks.

## Privacy

This repository is public. Use example.com and example.net in examples and
tests, and keep the names of real hosts and services out of it.

## Releases

Versions are calendar versions, `yyyy.mm.dd-nnn`, where `nnn` is a
three-digit serial within the day, starting at `001`. Run
`git tag -l "$(date +%Y.%m.%d)-*"` and take the next number. A tag is
the bare version, lightweight, not annotated.

A pushed tag is a release. `release.yaml` waits for the `ci.yaml` run
of the same commit to pass, then runs `docker buildx bake --push`, which
builds and pushes `ghcr.io/chrisguidry/healthchecks-operator` under the version and
under `:latest`, with provenance and an SBOM. `docker-bake.hcl` holds the
platforms, the tags, and the OCI labels and annotations of the image, so
`docker buildx bake --load` on a workstation builds the same image. A
change to a label or an annotation goes in that file, not in the
Dockerfile.

A push to `main` is a development build, versioned from the most recent
release tag plus a suffix: `2026.09.27-001-dev-003-abcdef01` is three
commits past `2026.09.27-001`, at commit `abcdef01`. A development build
never moves `:latest`. To run one, pin the manifests to the full commit
sha and the image to the build's version; the run's step summary prints
both lines.
