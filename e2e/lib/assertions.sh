#!/usr/bin/env bash
# The claims this test proves. Each assert_* function is one
# numbered claim from the e2e plan; the check_* functions under it are
# what retry (e2e/lib/common.sh) polls until it holds or times out.

READY_CHECKS=(healthchecks-ui broken-page kubernetes-api-cert backup flaky nightly-export weekly-report)

assert_clusterproject_ready() {
  retry "ClusterProject e2e Ready=True" 60 clusterproject_ready
  log "ok: ClusterProject e2e is Ready"
}

clusterproject_ready() {
  local status
  status=$(kubectl get clusterproject e2e -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  [ "$status" = "True" ] || { kubectl get clusterproject e2e -o yaml; return 1; }
}

assert_checks_ready() {
  local name
  for name in "${READY_CHECKS[@]}"; do
    retry "Check $CHECK_NS/$name Ready=True" 90 check_ready "$name"
  done
  log "ok: every Check is Ready"
}

check_ready() {
  local name=$1 status
  status=$(kubectl -n "$CHECK_NS" get check "$name" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  [ "$status" = "True" ] || { kubectl -n "$CHECK_NS" get check "$name" -o yaml; return 1; }
}

assert_passing_states() {
  retry "Check healthchecks-ui Passing=True" 90 check_passing healthchecks-ui True
  retry "Check broken-page Passing=False, missing the expected text" 90 \
    check_passing broken-page False "this text is not on the page"
  retry "Check kubernetes-api-cert Passing=False, certificate does not verify" 60 \
    check_passing kubernetes-api-cert False
  log "ok: healthchecks-ui passes, broken-page and kubernetes-api-cert fail as expected"
}

# check_passing reads a Check's Passing condition, and when CONTAINS is
# given, requires the failure message to mention it: the text the http
# probe expected and did not find.
check_passing() {
  local name=$1 want=$2 contains=${3:-} status message
  status=$(kubectl -n "$CHECK_NS" get check "$name" -o jsonpath='{.status.conditions[?(@.type=="Passing")].status}')
  message=$(kubectl -n "$CHECK_NS" get check "$name" -o jsonpath='{.status.conditions[?(@.type=="Passing")].message}')
  if [ "$status" != "$want" ]; then
    echo "Check $name Passing=$status, want $want; message: $message"
    return 1
  fi
  if [ -n "$contains" ] && [[ "$message" != *"$contains"* ]]; then
    echo "Check $name Passing message does not mention '$contains': $message"
    return 1
  fi
}

# EXPECT_* name what each Check should have written into Healthchecks
# itself, past the operator's own status: the slug that identifies it,
# either the timeout an http or tls Check's interval sets or the
# schedule a cronJob Check copies (translated, for flaky's @hourly),
# and the tags a Check declares.
declare -A EXPECT_SLUG=(
  [healthchecks-ui]=example-healthchecks-ui
  [broken-page]=example-broken-page
  [kubernetes-api-cert]=example-kubernetes-api-cert
  [backup]=example-backup
  [flaky]=example-flaky
  [nightly-export]=example-nightly-export
  [weekly-report]=example-weekly-report
)
declare -A EXPECT_TIMEOUT=([healthchecks-ui]=60 [broken-page]=60 [kubernetes-api-cert]=60 [nightly-export]=3600)
declare -A EXPECT_SCHEDULE=([backup]="* * * * *" [flaky]="0 * * * *" [weekly-report]="0 4 * * 1")
declare -A EXPECT_TAGS=([healthchecks-ui]="e2e http")

assert_healthchecks_api_state() {
  local pushover_id
  pushover_id=$(hc_api /api/v3/channels/ | jq -r '.channels[] | select(.name=="Pushover") | .id')
  [ -n "$pushover_id" ] || die "no Pushover channel found through the Healthchecks API"

  local name
  for name in "${!EXPECT_SLUG[@]}"; do
    retry "Healthchecks' record of $name matches its Check" 30 \
      check_matches_healthchecks "$name" "${EXPECT_SLUG[$name]}" "$pushover_id"
  done
  log "ok: every check in Healthchecks matches its Check's slug, timeout or schedule, tags, and channel"
}

check_matches_healthchecks() {
  local name=$1 slug=$2 pushover_id=$3 found count channels timeout schedule tags
  found=$(hc_api "/api/v3/checks/?slug=$slug") || { echo "no check at slug $slug"; return 1; }
  count=$(jq '.checks | length' <<<"$found")
  [ "$count" = "1" ] || { echo "no check at slug $slug: $found"; return 1; }

  channels=$(jq -r '.checks[0].channels' <<<"$found")
  [[ ",$channels," == *",$pushover_id,"* ]] || {
    echo "check $slug has channels [$channels], want the Pushover channel ($pushover_id)"; return 1
  }

  if [ -n "${EXPECT_TIMEOUT[$name]:-}" ]; then
    timeout=$(jq -r '.checks[0].timeout' <<<"$found")
    [ "$timeout" = "${EXPECT_TIMEOUT[$name]}" ] || {
      echo "check $slug timeout=$timeout, want ${EXPECT_TIMEOUT[$name]}"; return 1
    }
  fi
  if [ -n "${EXPECT_SCHEDULE[$name]:-}" ]; then
    schedule=$(jq -r '.checks[0].schedule' <<<"$found")
    [ "$schedule" = "${EXPECT_SCHEDULE[$name]}" ] || {
      echo "check $slug schedule=[$schedule], want [${EXPECT_SCHEDULE[$name]}]"; return 1
    }
  fi
  if [ -n "${EXPECT_TAGS[$name]:-}" ]; then
    tags=$(jq -r '.checks[0].tags' <<<"$found")
    [ "$tags" = "${EXPECT_TAGS[$name]}" ] || {
      echo "check $slug tags=[$tags], want [${EXPECT_TAGS[$name]}]"; return 1
    }
  fi
}

assert_triggered_jobs() {
  assert_triggered_success
  assert_triggered_failure
}

assert_triggered_success() {
  local job_name="backup-ok-$RUN_ID"
  log "triggering cronjob/backup: $job_name"
  kubectl -n "$CHECK_NS" create job "$job_name" --from=cronjob/backup >/dev/null
  kubectl -n "$CHECK_NS" wait --for=condition=Complete "job/$job_name" --timeout=60s >/dev/null

  retry "example-backup reports a start ping then a success ping for $job_name" 60 \
    pings_show_start_then example-backup success
  log "ok: a triggered successful Job produces a start ping then a success ping"
}

assert_triggered_failure() {
  local job_name="flaky-boom-$RUN_ID" uuid n body
  log "triggering cronjob/flaky: $job_name"
  kubectl -n "$CHECK_NS" create job "$job_name" --from=cronjob/flaky >/dev/null
  kubectl -n "$CHECK_NS" wait --for=condition=Failed "job/$job_name" --timeout=60s >/dev/null

  retry "example-flaky reports a start ping then a fail ping for $job_name" 60 \
    pings_show_start_then example-flaky fail

  uuid=$(check_uuid example-flaky)
  n=$(hc_api "/api/v3/checks/$uuid/pings/" | jq -r '.pings[0].n')
  body=$(hc_api "/api/v3/checks/$uuid/pings/$n/body")
  [[ "$body" == *"$job_name"* ]] || die "fail ping body for example-flaky does not name $job_name: $body"
  log "ok: a triggered failing Job produces a fail ping whose body names the Job"
}

# pings_show_start_then reads a check's two most recent pings and
# requires the latest to be WANT, and the one before it "start": every
# Job the operator reports gets a start ping first, whether it goes on
# to succeed or fail.
pings_show_start_then() {
  local slug=$1 want=$2 uuid pings latest prior
  uuid=$(check_uuid "$slug")
  [ -n "$uuid" ] || { echo "no check at slug $slug"; return 1; }
  pings=$(hc_api "/api/v3/checks/$uuid/pings/")
  latest=$(jq -r '.pings[0].type // empty' <<<"$pings")
  prior=$(jq -r '.pings[1].type // empty' <<<"$pings")
  if [ "$latest" != "$want" ] || [ "$prior" != "start" ]; then
    echo "pings for $slug, most recent first: $pings"
    return 1
  fi
}

# assert_workload_pings proves the ping kind end to end: the operator
# takes over a ConfigMap it did not create, creates one that does not
# exist, and writes the check's ping URL into each, and a Pod that reads
# the URL from a ConfigMap reaches the check.
assert_workload_pings() {
  local job_name="nightly-export-$RUN_ID"
  retry "ConfigMap nightly-export-healthcheck holds the ping URL" 60 configmap_holds_ping_url nightly-export nightly-export-healthcheck
  retry "ConfigMap weekly-report-healthcheck exists and holds the ping URL" 60 configmap_holds_ping_url weekly-report weekly-report-healthcheck

  log "starting a Job that pings the URL from the ConfigMap: $job_name"
  kubectl -n "$CHECK_NS" apply -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata: {name: $job_name, namespace: $CHECK_NS}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      hostAliases: [{ip: "$HC_IP", hostnames: [hc-e2e-healthchecks]}]
      containers:
        - name: ping
          image: busybox:1.36
          command: [sh, -c, 'wget -q -O- "\$HEALTHCHECK_URL"']
          envFrom: [{configMapRef: {name: nightly-export-healthcheck}}]
YAML
  kubectl -n "$CHECK_NS" wait --for=condition=Complete "job/$job_name" --timeout=60s >/dev/null

  retry "example-nightly-export has a success ping" 30 latest_ping_is example-nightly-export success
  log "ok: a Pod reads the ping URL from the ConfigMap the operator wrote, and its ping reaches the check"
}

configmap_holds_ping_url() {
  local check=$1 name=$2 want got managed owner
  want=$(kubectl -n "$CHECK_NS" get check "$check" -o jsonpath='{.status.pingURL}')
  got=$(kubectl -n "$CHECK_NS" get configmap "$name" -o jsonpath='{.data.HEALTHCHECK_URL}')
  managed=$(kubectl -n "$CHECK_NS" get configmap "$name" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')
  owner=$(kubectl -n "$CHECK_NS" get configmap "$name" -o jsonpath='{.metadata.ownerReferences[?(@.controller==true)].name}')
  if [ -z "$want" ] || [ "$got" != "$want" ] || [ "$managed" != "healthchecks-operator" ] || [ "$owner" != "$check" ]; then
    echo "ConfigMap $name has URL [$got], label [$managed], and owner [$owner]; want [$want], healthchecks-operator, and $check"
    return 1
  fi
}

latest_ping_is() {
  local slug=$1 want=$2 uuid latest
  uuid=$(check_uuid "$slug")
  [ -n "$uuid" ] || { echo "no check at slug $slug"; return 1; }
  latest=$(hc_api "/api/v3/checks/$uuid/pings/" | jq -r '.pings[0].type // empty')
  [ "$latest" = "$want" ] || { echo "the latest ping for $slug is [$latest], want $want"; return 1; }
}

assert_heartbeat() {
  retry "operator-heartbeat exists and has pings" 100 heartbeat_has_pings
  log "ok: the heartbeat check exists and gets pings"
}

heartbeat_has_pings() {
  local n
  n=$(hc_api "/api/v3/checks/?slug=operator-heartbeat" | jq -r '.checks[0].n_pings // 0')
  [ "$n" -gt 0 ] || { echo "operator-heartbeat n_pings=$n"; return 1; }
}

# assert_restart_catchup proves the operator finds a run it missed: it
# stops, a Job runs and finishes while it is down, and it starts again
# to a check that still reports that run.
assert_restart_catchup() {
  local job_name="backup-restart-$RUN_ID" uuid before after

  uuid=$(check_uuid example-backup)
  before=$(hc_api "/api/v3/checks/$uuid/pings/" | jq -r '.pings[0].n // 0')

  log "scaling the operator to 0"
  kubectl -n healthchecks-operator scale deployment/healthchecks-operator --replicas=0 >/dev/null
  kubectl -n healthchecks-operator wait --for=delete pod -l app=healthchecks-operator --timeout=60s >/dev/null

  log "triggering cronjob/backup while the operator is down: $job_name"
  kubectl -n "$CHECK_NS" create job "$job_name" --from=cronjob/backup >/dev/null
  kubectl -n "$CHECK_NS" wait --for=condition=Complete "job/$job_name" --timeout=60s >/dev/null

  log "scaling the operator back to 1"
  kubectl -n healthchecks-operator scale deployment/healthchecks-operator --replicas=1 >/dev/null
  kubectl -n healthchecks-operator rollout status deployment/healthchecks-operator --timeout=90s >/dev/null

  retry "example-backup reports $job_name after the restart" 90 pings_advanced_past example-backup "$before"
  log "ok: a Job that finished while the operator was down is still reported once it restarts"
}

pings_advanced_past() {
  local slug=$1 before=$2 uuid after
  uuid=$(check_uuid "$slug")
  after=$(hc_api "/api/v3/checks/$uuid/pings/" | jq -r '.pings[0].n // 0')
  [ "$after" -gt "$before" ] || { echo "$slug pings[0].n=$after, want more than $before"; return 1; }
}

assert_slug_change_and_delete() {
  local old_slug new_slug="example-healthchecks-ui-renamed" deleted_slug

  old_slug=$(kubectl -n "$CHECK_NS" get check healthchecks-ui -o jsonpath='{.status.slug}')
  log "renaming healthchecks-ui's slug: $old_slug -> $new_slug"
  kubectl -n "$CHECK_NS" patch check healthchecks-ui --type merge \
    -p "{\"spec\":{\"slug\":\"$new_slug\"}}" >/dev/null

  retry "the check at the old slug $old_slug is gone" 60 slug_is_gone "$old_slug"
  retry "the check at the new slug $new_slug exists" 60 slug_exists "$new_slug"
  retry "Check healthchecks-ui's status.slug is $new_slug" 60 check_status_slug_is healthchecks-ui "$new_slug"
  log "ok: a slug change deletes the old check and creates the new one"

  deleted_slug=$(kubectl -n "$CHECK_NS" get check kubernetes-api-cert -o jsonpath='{.status.slug}')
  log "deleting Check kubernetes-api-cert"
  kubectl -n "$CHECK_NS" delete check kubernetes-api-cert --timeout=60s >/dev/null
  retry "the check at slug $deleted_slug is gone" 30 slug_is_gone "$deleted_slug"
  log "ok: deleting a Check deletes its check and releases the finalizer"
}

slug_is_gone() {
  local count
  count=$(hc_api "/api/v3/checks/?slug=$1" | jq '.checks | length')
  [ "$count" = "0" ] || { echo "slug $1 still exists in Healthchecks"; return 1; }
}

slug_exists() {
  local count
  count=$(hc_api "/api/v3/checks/?slug=$1" | jq '.checks | length')
  [ "$count" = "1" ] || { echo "slug $1 not found in Healthchecks"; return 1; }
}

check_status_slug_is() {
  local name=$1 want=$2 got
  got=$(kubectl -n "$CHECK_NS" get check "$name" -o jsonpath='{.status.slug}')
  [ "$got" = "$want" ] || { echo "Check $name status.slug=$got, want $want"; return 1; }
}

# assert_no_hot_loop guards against the hot-loop bug fixed earlier: a
# failure that fed itself into an unbroken stream of identical log
# lines. reconcile_log.go writes a fault once and stays quiet while it
# lasts, so a healthy run repeats no line more than a couple of times,
# from the handful of passes a watch or the backstop ticker starts.
assert_no_hot_loop() {
  local worst count
  worst=$(operator_log | sort | uniq -c | sort -rn | head -1)
  count=$(awk '{print $1}' <<<"$worst")
  if [ -n "$count" ] && [ "$count" -gt 3 ]; then
    die "a line repeats $count times in the operator log: $worst"
  fi
  log "ok: no line repeats more than 3 times in the operator log"
}
