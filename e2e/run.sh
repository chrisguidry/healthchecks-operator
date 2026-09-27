#!/usr/bin/env bash
# The operator's end-to-end test: a k3s cluster and a Healthchecks
# instance, both in Docker, the operator built from this checkout and
# loaded into the cluster with no registry involved, and a ClusterProject
# plus a Check for every probe kind. e2e/checks.yaml says what it
# declares, and e2e/lib/assertions.sh says what it proves.
#
# Every container and network name carries RUN_ID, so a second run on
# the same machine does not collide with this one. cleanup runs on any
# exit, including a failed one, and a failed assertion prints the
# operator's log and the state kubectl and Healthchecks agree on.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
E2E_DIR="$REPO_ROOT/e2e"
CHECK_NS=example

# Each of these resolves under this file's own directory, never
# somewhere a reader or a linter would need to guess at.
# shellcheck source=lib/common.sh disable=SC1091
source "$E2E_DIR/lib/common.sh"
# shellcheck source=lib/cluster.sh disable=SC1091
source "$E2E_DIR/lib/cluster.sh"
# shellcheck source=lib/healthchecks.sh disable=SC1091
source "$E2E_DIR/lib/healthchecks.sh"
# shellcheck source=lib/logs.sh disable=SC1091
source "$E2E_DIR/lib/logs.sh"
# shellcheck source=lib/assertions.sh disable=SC1091
source "$E2E_DIR/lib/assertions.sh"

RUN_ID=$(random_id)
NETWORK="hc-e2e-net-$RUN_ID"
K3S_CONTAINER="hc-e2e-k3s-$RUN_ID"
HC_CONTAINER="hc-e2e-hc-$RUN_ID"
OPERATOR_IMAGE="ghcr.io/chrisguidry/healthchecks-operator:e2e"

# .e2e/ is under the repository, not the system temp directory: a
# machine can run /tmp as a tmpfs, backed by memory rather than disk,
# and this run's kubeconfig should not compete with everything else on
# the machine for RAM.
mkdir -p "$REPO_ROOT/.e2e"
WORKDIR=$(mktemp -d "$REPO_ROOT/.e2e/$RUN_ID.XXXXXX")
KUBECONFIG="$WORKDIR/kubeconfig"
export KUBECONFIG

cleanup() {
  docker rm -f "$K3S_CONTAINER" "$HC_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  docker rmi "$OPERATOR_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}

on_exit() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    print_diagnostics
  fi
  cleanup
  exit "$status"
}
trap on_exit EXIT

log "run $RUN_ID: network $NETWORK, k3s $K3S_CONTAINER, healthchecks $HC_CONTAINER, namespace $CHECK_NS"
docker network create "$NETWORK" >/dev/null

start_healthchecks
start_k3s
apply_crds
build_and_load
deploy_manifests

assert_clusterproject_ready
assert_checks_ready
assert_passing_states
assert_healthchecks_api_state
assert_triggered_jobs
assert_workload_pings
assert_heartbeat
assert_restart_catchup
assert_slug_change_and_delete
assert_no_hot_loop

log "all assertions passed"
