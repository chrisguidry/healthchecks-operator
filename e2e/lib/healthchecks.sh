#!/usr/bin/env bash
# Starts the Healthchecks instance the operator talks to, and seeds it
# with the user, project, and Pushover channel that fixtures.yaml and
# checks.yaml expect to find. The operator only ever calls the
# management API against it, the same as it would against a real
# Healthchecks instance.

# HC_API_KEY is a read-write key, and exactly 32 characters: a shorter
# one reads back from the API as valid but answers every request with
# "missing api key", which is a confusing way to learn the length
# matters.
HC_API_KEY='e2e0api0key00000000000000000000x'

start_healthchecks() {
  log "starting Healthchecks ($HC_CONTAINER)"
  docker run -d --name "$HC_CONTAINER" --network "$NETWORK" \
    -e DB=sqlite -e DB_NAME=/tmp/hc.sqlite \
    -e SECRET_KEY="$(random_id)$(random_id)" \
    -e SITE_ROOT="http://hc-e2e-healthchecks:8000" \
    -e PING_ENDPOINT="http://hc-e2e-healthchecks:8000/ping/" \
    -e ALLOWED_HOSTS='*' -e DEBUG=False \
    -p 127.0.0.1::8000 \
    healthchecks/healthchecks:v4.4 \
    >/dev/null

  HC_PORT=$(docker port "$HC_CONTAINER" 8000/tcp | head -1 | cut -d: -f2)

  retry "Healthchecks answering" 60 curl -sSf --max-time 5 -o /dev/null "http://127.0.0.1:$HC_PORT/api/v3/status/"
  seed_healthchecks

  # hostAliases needs the container's address on the network run.sh
  # made for it, the address the alias in checks.yaml and
  # fixtures.yaml resolves to from inside a pod.
  HC_IP=$(docker inspect "$HC_CONTAINER" --format "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}")
  [ -n "$HC_IP" ] || die "could not find the address of $HC_CONTAINER on $NETWORK"
}

# seed_healthchecks makes a user and a project the same way the
# Healthchecks signup flow does (hc.accounts.views._make_user), skips
# the default project it would also make, and sets the project's own
# api_key and a Pushover webhook channel by hand.
seed_healthchecks() {
  log "seeding Healthchecks: a user, a project, and a Pushover channel"
  docker exec "$HC_CONTAINER" ./manage.py shell -c "
from hc.accounts.views import _make_user
from hc.accounts.models import Project
from hc.api.models import Channel
import json

user = _make_user('e2e@example.com', with_project=False)
project = Project(owner=user)
project.badge_key = user.username
project.api_key = '$HC_API_KEY'
project.save()

channel = Channel(project=project, kind='webhook', name='Pushover')
channel.value = json.dumps({
    'method_down': 'POST', 'url_down': 'http://example.com/down',
    'body_down': '', 'headers_down': {},
    'method_up': 'POST', 'url_up': 'http://example.com/up',
    'body_up': '', 'headers_up': {},
})
channel.save()
" >/dev/null
}

# hc_api calls the management API directly, the way the assertions
# read back what the operator did, separately from what the Check
# objects in Kubernetes say about themselves.
hc_api() {
  curl -sSf --max-time 10 -H "X-Api-Key: $HC_API_KEY" "http://127.0.0.1:$HC_PORT$1"
}

# check_uuid looks up a check's UUID by its Healthchecks slug, printing
# nothing (an empty string) when no check has that slug.
check_uuid() {
  hc_api "/api/v3/checks/?slug=$1" | jq -r '.checks[0].uuid // empty'
}
