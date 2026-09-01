#!/usr/bin/env bash
#
# Runs presage on a real Kubernetes cluster with a real kubelet.
#
# envtest gives an API server and etcd but no kubelet, so nothing there has
# ever scheduled a pod. That leaves a class of failure untested: image and RBAC
# problems that only appear in a running pod, the scale subresource against a
# Deployment whose controller is actually reconciling, and the elementary
# question of whether replicas presage asks for genuinely arrive.
#
#   ./hack/e2e-kind.sh            # create, test, delete
#   KEEP=1 ./hack/e2e-kind.sh     # leave the cluster up for poking at
set -euo pipefail

CLUSTER=presage-e2e
NS=presage-e2e
IMG=presage:e2e
FAKE_IMG=presage-fakemetrics:e2e
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEEP="${KEEP:-0}"
scratch="$(mktemp -d)"

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  local code=$?
  rm -rf "$scratch"
  if [ "$code" -ne 0 ]; then
    log "failed; dumping state"
    kubectl -n "$NS" get predictivescalers -o yaml 2>/dev/null | head -80 || true
    kubectl -n presage-system logs -l app.kubernetes.io/name=presage --tail=60 2>/dev/null || true
    kubectl -n "$NS" get pods 2>/dev/null || true
  fi
  if [ "$KEEP" = "1" ]; then
    log "KEEP=1, leaving cluster '$CLUSTER' up"
  else
    log "deleting cluster"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  exit $code
}
trap cleanup EXIT

# Wait until `kubectl get -o jsonpath` returns the expected value.
await() {
  local desc="$1" want="$2" timeout="$3"; shift 3
  local deadline=$(( SECONDS + timeout )) got=""
  while [ $SECONDS -lt $deadline ]; do
    got="$("$@" 2>/dev/null || true)"
    [ "$got" = "$want" ] && { echo "    $desc: $got"; return 0; }
    sleep 2
  done
  fail "$desc: got '${got:-<none>}', want '$want' after ${timeout}s"
}

set_demand() {
  kubectl -n "$NS" create configmap fake-metrics-demand \
    --from-literal=demand="$1" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  # The kubelet propagates ConfigMap changes to the mounted volume on its own
  # sync period, which is up to a minute. Restarting the pod is immediate and
  # this test is about presage, not about kubelet mount semantics.
  kubectl -n "$NS" rollout restart deployment/fake-metrics >/dev/null
  kubectl -n "$NS" rollout status deployment/fake-metrics --timeout=300s >/dev/null
}

replicas() { kubectl -n "$NS" get deployment sample -o jsonpath='{.spec.replicas}'; }
ready()    { kubectl -n "$NS" get deployment sample -o jsonpath='{.status.readyReplicas}'; }

log "creating kind cluster"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --config "$ROOT/test/kind/kind-config.yaml" --wait 120s

log "building and loading the controller image"
docker build -t "$IMG" "$ROOT"
kind load docker-image "$IMG" --name "$CLUSTER"

# Built locally rather than pulled. kind side-loads with --all-platforms, and a
# multi-arch image pulled from a registry only has this host's blobs present,
# so loading it fails with "content digest ...: not found". A locally built
# image is single-platform and loads cleanly -- which is also why the controller
# image above has always worked.
log "building and loading the fake metrics image"
docker build -q -t "$FAKE_IMG" -f "$ROOT/test/kind/fakemetrics/Dockerfile" "$ROOT" >/dev/null
kind load docker-image "$FAKE_IMG" --name "$CLUSTER" >/dev/null

log "installing CRDs and the controller"
kubectl apply -f "$ROOT/config/crd" >/dev/null
kubectl apply -k "$ROOT/config" >/dev/null
kubectl -n presage-system set image deployment/presage-controller manager="$IMG"
kubectl -n presage-system patch deployment presage-controller --type=json \
  -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
# One replica: leader election across two would only add startup latency here.
kubectl -n presage-system scale deployment/presage-controller --replicas=1
kubectl -n presage-system rollout status deployment/presage-controller --timeout=180s

log "deploying the fake metrics endpoint and the workload"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f "$ROOT/test/kind/fake-metrics.yaml" >/dev/null
kubectl -n "$NS" rollout status deployment/fake-metrics --timeout=300s
kubectl apply -f "$ROOT/test/kind/workload.yaml" >/dev/null
kubectl -n "$NS" rollout status deployment/sample --timeout=300s

# ---------------------------------------------------------------------------
log "demand 100 at 50/replica -> 2 replicas"
await "spec.replicas" 2 120 replicas
await "readyReplicas"  2 120 ready

log "demand 500 -> scales up to 10, and the pods actually arrive"
set_demand 500
await "spec.replicas" 10 120 replicas
# The assertion envtest cannot make: replicas presage asked for genuinely
# became running pods on a node.
await "readyReplicas" 10 180 ready

log "demand 100 -> scales back down"
set_demand 100
await "spec.replicas" 2 180 replicas

log "status reports why"
kubectl -n "$NS" get predictivescaler sample \
  -o jsonpath='{.status.recommendedReplicas}{"\t"}{.status.breakdown.constraint}{"\t"}{.status.lastForecast.backend}{"\n"}'

ready_cond="$(kubectl -n "$NS" get predictivescaler sample \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
[ "$ready_cond" = "True" ] || fail "Ready condition is '$ready_cond'"

log "controller reports no reconcile errors"
errs="$(kubectl -n presage-system exec deploy/presage-controller -- \
  wget -qO- localhost:8080/metrics 2>/dev/null |
  awk '/^presage_reconcile_total.*result="error"/ {s+=$2} END {printf "%d", s+0}')" || errs=0
[ "${errs:-0}" = "0" ] || fail "controller recorded $errs failed reconciles"

log "PASS: presage scaled a real workload on a real cluster"
