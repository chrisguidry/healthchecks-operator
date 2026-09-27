#!/usr/bin/env bash
# Starts a k3s server in Docker, waits for its node to be Ready, and
# writes a kubeconfig that reaches it from the host. Also builds the
# operator's image with the same Dockerfile the release does, and
# loads it straight into the cluster's containerd, so the cluster runs
# exactly what this run just compiled without a registry in between.

start_k3s() {
  log "starting k3s ($K3S_CONTAINER)"
  docker run -d --name "$K3S_CONTAINER" --network "$NETWORK" --privileged \
    -p 127.0.0.1::6443 \
    rancher/k3s:v1.37.0-k3s1 server --disable=traefik,metrics-server --tls-san 127.0.0.1 \
    >/dev/null

  retry "k3s's kubeconfig appearing" 60 k3s_kubeconfig_exists
  write_kubeconfig

  retry "kubectl reaching the new cluster" 60 kubectl get --raw=/healthz
  retry "the node registering itself" 60 node_exists
  kubectl wait --for=condition=Ready node --all --timeout=90s >/dev/null
}

node_exists() {
  [ "$(kubectl get nodes --no-headers 2>/dev/null | wc -l)" -gt 0 ]
}

k3s_kubeconfig_exists() {
  docker exec "$K3S_CONTAINER" test -s /etc/rancher/k3s/k3s.yaml
}

# write_kubeconfig copies k3s's own kubeconfig out and rewrites its
# server address: k3s writes 127.0.0.1:6443, the port inside its own
# container, but run.sh published 6443 to a random free port on the
# host, since a second run must not collide with one already using
# 6443.
write_kubeconfig() {
  local port
  port=$(docker port "$K3S_CONTAINER" 6443/tcp | head -1 | cut -d: -f2)
  docker exec "$K3S_CONTAINER" cat /etc/rancher/k3s/k3s.yaml \
    | sed "s#server: https://127.0.0.1:6443#server: https://127.0.0.1:$port#" \
    > "$KUBECONFIG"
}

# build_and_load builds the operator's image from this checkout and
# imports it into k3s's containerd under the same name and tag the
# e2e overlay's images transformer requests, so the Deployment it
# creates finds the image already there.
build_and_load() {
  log "building the operator image ($OPERATOR_IMAGE)"
  docker build -t "$OPERATOR_IMAGE" --build-arg VERSION=e2e "$REPO_ROOT" >/dev/null

  log "loading the operator image into k3s"
  docker save "$OPERATOR_IMAGE" | docker exec -i "$K3S_CONTAINER" ctr images import - >/dev/null
}

# apply_crds installs just the two CRDs, ahead of everything else that
# names their kinds. Creating a CustomResourceDefinition and a custom
# resource of its kind in the same batch races the API server's own
# discovery cache, so ClusterProject and Check wait for Established
# before the rest of the manifests name them.
apply_crds() {
  log "applying the CRDs"
  kubectl apply -f "$REPO_ROOT/deploy/clusterprojects-crd.yaml" -f "$REPO_ROOT/deploy/checks-crd.yaml" >/dev/null
  kubectl wait --for=condition=Established \
    crd/clusterprojects.healthchecks.guid.foo crd/checks.healthchecks.guid.foo --timeout=30s >/dev/null
}

# deploy_manifests applies the operator, RBAC, and the e2e overlay's
# ClusterProject, then the Checks and CronJobs that exercise it. The
# CRDs are already Established, so this is one apply.
deploy_manifests() {
  log "applying the operator and its e2e fixtures"
  kubectl kustomize "$E2E_DIR" \
    | sed "s/203.0.113.1/$HC_IP/" \
    | kubectl apply -f - >/dev/null

  kubectl apply -f "$E2E_DIR/checks.yaml" >/dev/null

  kubectl -n healthchecks-operator rollout status deployment/healthchecks-operator --timeout=90s >/dev/null
}
