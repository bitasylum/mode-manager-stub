#!/usr/bin/env bash
# Runs the end-to-end tests on a new two-node kind cluster: the tests install
# the stub and mode-operator with their charts, and check that the stub
# follows the mode manager's contract through the client library.
#
# The cluster is reached only through a kubeconfig of its own, so your
# current kubectl context is left alone. The script refuses to run if a
# cluster named E2E_CLUSTER already exists, and deletes only the cluster it
# created, unless KEEP_CLUSTER=1.
#
# Uses STUB_IMG and OPERATOR_IMG (both already built), MODE_OPERATOR_DIR (a
# mode-operator checkout, for its chart), HELM, KIND and KUBECTL.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
CLUSTER=${E2E_CLUSTER:-mode-manager-stub-test-e2e}
STUB_IMG=${STUB_IMG:?set STUB_IMG to the stub image}
OPERATOR_IMG=${OPERATOR_IMG:?set OPERATOR_IMG to the mode-operator image}
MODE_OPERATOR_DIR=${MODE_OPERATOR_DIR:?set MODE_OPERATOR_DIR to a mode-operator checkout}
HELM=${HELM:-helm}
KIND=${KIND:-kind}
KUBECTL=${KUBECTL:-kubectl}

KUBECONFIG=$(mktemp)
export KUBECONFIG
# Helm's own settings would take precedence over the kubeconfig, and a
# kuberc could change kubectl's defaults, so neither may point elsewhere.
unset HELM_KUBECONTEXT HELM_KUBEAPISERVER HELM_KUBETOKEN HELM_KUBEASUSER HELM_KUBEASGROUPS \
  HELM_KUBECAFILE HELM_KUBETLS_SERVER_NAME HELM_KUBEINSECURE_SKIP_TLS_VERIFY
export KUBECTL_KUBERC=false
created=0

fail() {
  echo "FAILED: $*" >&2
  exit 1
}

# diagnose prints what is needed to debug a failed run, before the cluster
# goes.
diagnose() {
  echo "== diagnostics"
  "$KUBECTL" get pods -A -o wide || true
  "$KUBECTL" -n mode-manager logs -l app.kubernetes.io/name=mode-manager-stub --tail=-1 || true
  "$KUBECTL" -n mode-operator-system logs -l app.kubernetes.io/component=controller --prefix --tail=200 || true
  "$KUBECTL" get clustermode -o yaml || true
  "$KUBECTL" -n mode-manager get configmap -o yaml || true
  "$KUBECTL" get events -A --sort-by=.metadata.creationTimestamp || true
}

cleanup() {
  local status=$?
  if ((created)); then
    if ((status != 0)); then diagnose; fi
    if [[ ${KEEP_CLUSTER:-} == 1 ]]; then
      echo "Kept cluster $CLUSTER; its kubeconfig is $KUBECONFIG"
    else
      "$KIND" delete cluster --name "$CLUSTER"
    fi
  fi
  if [[ ${KEEP_CLUSTER:-} != 1 ]]; then rm -f "$KUBECONFIG"; fi
  exit "$status"
}
trap cleanup EXIT

[[ -d $MODE_OPERATOR_DIR/dist/chart ]] || fail "no mode-operator chart in $MODE_OPERATOR_DIR/dist/chart"
# Two runs at once would share the cluster name, and one's cleanup would
# delete the other's cluster.
if command -v flock >/dev/null; then
  exec 9>"${TMPDIR:-/tmp}/$CLUSTER.lock"
  flock -n 9 || fail "another run is using $CLUSTER"
fi
clusters=$("$KIND" get clusters)
if grep -Fqx -- "$CLUSTER" <<<"$clusters"; then
  fail "a kind cluster named $CLUSTER already exists; delete it or set E2E_CLUSTER"
fi
# Every kind node takes inotify instances from the same per-user limit, and a
# node that runs out can't start kube-proxy.
instances=$(cat /proc/sys/fs/inotify/max_user_instances 2>/dev/null || echo 512)
if ((instances < 512)); then
  echo "WARNING: fs.inotify.max_user_instances is $instances; with other kind clusters running, nodes" \
    "may fail to start. Raise it with: sudo sysctl fs.inotify.max_user_instances=512" >&2
fi

echo "== creating kind cluster $CLUSTER"
"$KIND" create cluster --name "$CLUSTER" --config test/e2e/kind-config.yaml --wait 2m
created=1
# The tests run only on a cluster marked with this run's ID.
E2E_RUN_ID=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
"$KUBECTL" annotate namespace default "e2e.mode-manager-stub.bitasylum.org/run=$E2E_RUN_ID"
"$KUBECTL" -n kube-system rollout status daemonset/kube-proxy --timeout 2m ||
  fail "kube-proxy didn't start on every node"
"$KIND" load docker-image "$STUB_IMG" "$OPERATOR_IMG" --name "$CLUSTER"

echo "== running the tests"
# The tests run in test/e2e, so paths they get must be absolute.
E2E_CLUSTER=$CLUSTER E2E_RUN_ID=$E2E_RUN_ID STUB_IMG=$STUB_IMG OPERATOR_IMG=$OPERATOR_IMG \
  MODE_OPERATOR_CHART="$(cd "$MODE_OPERATOR_DIR" && pwd)/dist/chart" HELM=$HELM KUBECTL=$KUBECTL \
  go test -tags e2e ./test/e2e -count=1 -timeout 25m -v -ginkgo.v
