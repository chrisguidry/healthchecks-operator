#!/usr/bin/env bash
# The operator's own log, fetched on demand. Nothing here runs in the
# background: a follower is a process this script would have to stop
# reliably before it can exit, and a quiet operator gives a follower
# nothing to read and no reason to end on its own, so the wait for it
# never returns. --tail bounds a single fetch instead, so a hot loop
# cannot fill this machine's memory or disk either.

OPERATOR_LOG_TAIL_LINES=2000

# operator_log prints what the current operator pod has logged, oldest
# kept line first. A Deployment's pod is gone once
# assert_restart_catchup replaces it, so this sees only the current
# pod's own history, never what an earlier pod logged before it.
operator_log() {
  kubectl -n healthchecks-operator logs deploy/healthchecks-operator \
    --tail="$OPERATOR_LOG_TAIL_LINES" 2>&1 || true
}

# print_diagnostics is what a failing assertion leaves behind: the
# operator's own account of what it did, and what the cluster and
# Healthchecks agree the state is now.
print_diagnostics() {
  echo "===== operator log =====" >&2
  operator_log >&2
  echo "===== kubectl get =====" >&2
  kubectl get clusterprojects -o wide >&2 2>&1 || true
  kubectl -n "$CHECK_NS" get checks -o wide >&2 2>&1 || true
  kubectl -n "$CHECK_NS" get jobs,cronjobs >&2 2>&1 || true
  kubectl -n healthchecks-operator get deployments,pods >&2 2>&1 || true
}
