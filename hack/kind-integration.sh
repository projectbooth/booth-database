#!/usr/bin/env bash
# Layer 3 (contracts/testing-strategy.md): deploy the real chart — module + bundled PostgreSQL +
# backup CronJob — into a throwaway kind cluster, then run test/integration/in_cluster_check.py as a
# Job inside it, and trigger one backup. Used by .github/workflows/integration.yml and runnable locally.
#
# What stands in for booth-core (and why): the BoothModule CRD is booth-core's real one (vendored in
# test/integration/fixtures, so the manifest is validated by the real schema), and the
# booth-credential-broker-provider-credentials Secret is created here the way core's provisioner
# would. Core's broker itself is played by FakeCore inside the check Job — running real core means
# its OIDC provider, controller and a real identity, which is booth-e2e's tier (ADR 0055), not this one.
#
# Two nodes (control plane + worker), matching ADR 0082's everyday node_count=2 shape, so node
# pinning (ADR 0090) is exercised for real: the pin is checked, the server is force-rescheduled, and
# it must come back on the same node with its data. The NetworkPolicy is checked both ways — from a
# namespace labelled booth.projectbooth.io/database-client=true (where the client checks run) and
# from an unlabelled one, which must not reach port 5432. kindnet enforces NetworkPolicy.
#
# KEEP_CLUSTER=1 leaves the cluster up for poking at.
set -euo pipefail

CLUSTER=${CLUSTER:-booth-database-it}
NS=booth-database
CLIENT_NS=it-clients
STS=db-booth-database-postgres
PG=$STS.$NS.svc.cluster.local
CRED=bcbp.database.$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
cd "$(dirname "$0")/.."

cleanup() { [ "${KEEP_CLUSTER:-}" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 120s --config - <<'KIND'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
KIND
fi

docker build -t booth-database:it .
docker build -t booth-database-check:it -f test/integration/Dockerfile .
kind load docker-image booth-database:it booth-database-check:it --name "$CLUSTER"

kubectl apply -f test/integration/fixtures/boothmodule-crd.yaml
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" create secret generic booth-credential-broker-provider-credentials \
  --from-literal=credential="$CRED" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install db charts/booth-database -n "$NS" \
  --set image.repository=booth-database --set image.tag=it --set image.pullPolicy=Never \
  --set leases.reapInterval=2s --set leases.minTTL=1s --set leases.maxTTL=1h --wait --timeout 5m

echo "--- BoothModule accepted by booth-core's real CRD schema:"
kubectl -n "$NS" get boothmodule database -o jsonpath='{.spec.id} providesCredentials={.spec.providesCredentials.kinds}{"\n"}'

echo "--- ADR 0090: StatefulSet pinned to the node its pod landed on:"
node=""
for _ in $(seq 60); do
  node=$(kubectl -n "$NS" get pod "$STS-0" -o jsonpath='{.spec.nodeName}')
  pin=$(kubectl -n "$NS" get statefulset "$STS" -o jsonpath='{.spec.template.spec.nodeSelector.kubernetes\.io/hostname}')
  [ -n "$node" ] && [ "$pin" = "$node" ] && break
  sleep 2
done
[ -n "$node" ] && [ "$pin" = "$node" ] || { echo "not pinned: pod on '$node', nodeSelector '$pin'"; exit 1; }
echo "pinned to $node"
# The pin changes the pod template, so the StatefulSet rolls once; let that settle.
kubectl -n "$NS" rollout status statefulset/"$STS" --timeout=180s

# Client checks run from a separate namespace carrying the label booth-core applies to modules
# declaring `database` — the path notebook/pipeline workloads take.
kubectl create namespace "$CLIENT_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "$CLIENT_NS" booth.projectbooth.io/database-client=true --overwrite
kubectl create namespace it-outsider --dry-run=client -o yaml | kubectl apply -f -

probe() { # probe <namespace> <name>: exit 0 iff a TCP connection to the server's port succeeds
  kubectl -n "$1" delete pod "$2" --ignore-not-found --wait >/dev/null
  kubectl -n "$1" run "$2" --image=booth-database-check:it --image-pull-policy=Never --restart=Never     --command -- python -c "import socket; socket.create_connection(('$PG', 5432), timeout=8)" >/dev/null
  kubectl -n "$1" wait --for=jsonpath='{.status.phase}'=Succeeded pod/"$2" --timeout=40s >/dev/null 2>&1
}
echo "--- NetworkPolicy:"
probe "$CLIENT_NS" probe-in && echo "labelled namespace reaches 5432: yes" || { echo "labelled namespace blocked"; exit 1; }
if probe it-outsider probe-out; then echo "UNLABELLED namespace reached 5432"; exit 1; fi
echo "unlabelled namespace reaches 5432: no"

kubectl -n "$CLIENT_NS" delete job in-cluster-check --ignore-not-found
kubectl -n "$CLIENT_NS" create -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: in-cluster-check
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: check
          image: booth-database-check:it
          imagePullPolicy: Never
          env:
            - name: PROVIDER_URL
              value: http://db-booth-database.$NS.svc.cluster.local:8080
            - name: PROVIDER_CREDENTIAL
              value: "$CRED"
EOF
if ! kubectl -n "$CLIENT_NS" wait --for=condition=complete job/in-cluster-check --timeout=180s; then
  kubectl -n "$CLIENT_NS" logs job/in-cluster-check || true
  kubectl -n "$NS" logs deploy/db-booth-database --tail=50 || true
  exit 1
fi
kubectl -n "$CLIENT_NS" logs job/in-cluster-check

echo "--- forced reschedule: the server must come back on $node with its data"
kubectl -n "$NS" delete pod "$STS-0" --wait
kubectl -n "$NS" rollout status statefulset/"$STS" --timeout=180s
after=$(kubectl -n "$NS" get pod "$STS-0" -o jsonpath='{.spec.nodeName}')
[ "$after" = "$node" ] || { echo "rescheduled to $after, not $node"; exit 1; }
rows=$(kubectl -n "$NS" exec "$STS-0" -- sh -c 'for d in $(psql -U booth_admin -d postgres -AtX -c "select datname from pg_database where datname like '"'"'bdb\_ws\_%'"'"'"); do psql -U booth_admin -d $d -AtX -c "select count(*) from it_notes" 2>/dev/null; done' | tr -d '[:space:]')
[ -n "$rows" ] && [ "$rows" != 0 ] || { echo "workspace data missing after reschedule"; exit 1; }
echo "back on $after, workspace rows intact ($rows)"

echo "--- backup CronJob, run once:"
kubectl -n "$NS" delete job backup-now --ignore-not-found
kubectl -n "$NS" create job backup-now --from=cronjob/db-booth-database-backup
kubectl -n "$NS" wait --for=condition=complete job/backup-now --timeout=120s
kubectl -n "$NS" logs job/backup-now

echo "--- provider audit lines (must name leases, never carry a password):"
kubectl -n "$NS" logs deploy/db-booth-database | grep 'audit:' | head -3
echo "kind integration: OK"
