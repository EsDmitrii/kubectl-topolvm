#!/usr/bin/env bash
# Put workloads and every anomaly the plugin checks for on the cluster setup.sh built. Idempotent
# enough to rerun after a failure; run teardown.sh for a clean start.
set -euo pipefail

CLUSTER="${CLUSTER:-topolvm-e2e}"
WORK="${WORK:-/var/tmp/topolvm-e2e}"
# Always our own kubeconfig: the scripts create, patch and delete objects.
export KUBECONFIG="$WORK/kubeconfig"
W1="$CLUSTER-worker" W2="$CLUSTER-worker2" W3="$CLUSTER-worker3"
GROUP=topolvm.io
[ "${LEGACY:-0}" = 1 ] && GROUP=topolvm.cybozu.com
if [ "$(kubectl config current-context 2>/dev/null)" != "kind-$CLUSTER" ]; then
  echo "$KUBECONFIG does not point at kind-$CLUSTER; run setup.sh first" >&2
  exit 1
fi

claim() { # claim NS NAME SIZE CLASS
  cat <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $2, namespace: $1}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $4
  resources: {requests: {storage: $3}}
YAML
}

pod() { # pod NS NAME CLAIM [NODE]
  local selector=""
  [ -n "${4:-}" ] && selector="nodeSelector: {kubernetes.io/hostname: $4}"
  cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $2, namespace: $1}
spec:
  $selector
  terminationGracePeriodSeconds: 1
  containers:
    - {name: app, image: busybox:1.36, command: [sh, -c, "dd if=/dev/zero of=/data/fill bs=1M count=64; sleep 1d"], volumeMounts: [{name: data, mountPath: /data}]}
  volumes:
    - {name: data, persistentVolumeClaim: {claimName: $3}}
YAML
}

sts() { # sts NS NAME REPLICAS SIZE CLASS NODES: replicas spread over NODES, one per node
  cat <<YAML
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: $2, namespace: $1}
spec:
  replicas: $3
  serviceName: $2
  selector: {matchLabels: {app: $2}}
  template:
    metadata: {labels: {app: $2}}
    spec:
      terminationGracePeriodSeconds: 1
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions: [{key: kubernetes.io/hostname, operator: In, values: [$6]}]
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - {labelSelector: {matchLabels: {app: $2}}, topologyKey: kubernetes.io/hostname}
      containers:
        - {name: app, image: busybox:1.36, command: [sh, -c, "sleep 1d"], volumeMounts: [{name: data, mountPath: /data}]}
  volumeClaimTemplates:
    - metadata: {name: data}
      spec: {accessModes: [ReadWriteOnce], storageClassName: $5, resources: {requests: {storage: $4}}}
YAML
}

lv_of() { # lv_of NS CLAIM: the LogicalVolume name of a bound claim
  kubectl -n "$1" get pvc "$2" -o jsonpath='{.spec.volumeName}'
}

wait_bound() { # wait_bound NS CLAIM...: StatefulSets create claims one replica at a time
  local ns="$1"; shift
  for c in "$@"; do
    for _ in $(seq 1 90); do
      kubectl -n "$ns" get pvc "$c" >/dev/null 2>&1 && break
      sleep 2
    done
    kubectl -n "$ns" wait --for=jsonpath='{.status.phase}'=Bound "pvc/$c" --timeout=180s >/dev/null
  done
}

kubectl create namespace e2e-a --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace e2e-b --dry-run=client -o yaml | kubectl apply -f -

# Workloads: a single-copy database, an HA pair on Retain, a thin volume with a snapshot and a clone,
# and a volume on the second device class of w2.
{
  sts e2e-a db 1 2Gi topolvm-ssd "$W2"; echo ---
  sts e2e-a ha 2 1Gi topolvm-retain "$W1, $W2"; echo ---
  claim e2e-a thin-src 2Gi topolvm-thin; echo ---
  pod e2e-a thin-app thin-src "$W1"; echo ---
  claim e2e-a hdd-data 1Gi topolvm-hdd; echo ---
  pod e2e-a hdd-app hdd-data "$W2"
} | kubectl apply -f -
wait_bound e2e-a data-db-0 data-ha-0 data-ha-1 thin-src hdd-data

kubectl apply -f - <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: thin-snap, namespace: e2e-a}
spec:
  volumeSnapshotClassName: topolvm-thin
  source: {persistentVolumeClaimName: thin-src}
YAML
kubectl -n e2e-a wait --for=jsonpath='{.status.readyToUse}'=true volumesnapshot/thin-snap --timeout=180s
kubectl apply -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: thin-clone, namespace: e2e-a}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: topolvm-thin
  resources: {requests: {storage: 2Gi}}
  dataSource: {name: thin-snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}
---
$(pod e2e-a clone-app thin-clone "$W1")
YAML
wait_bound e2e-a thin-clone

# Claims that stay Pending: too big for any node, no consumer, Immediate binding.
{
  claim e2e-b too-big 50Gi topolvm-ssd; echo ---
  pod e2e-b too-big-app too-big; echo ---
  claim e2e-b no-consumer 1Gi topolvm-ssd; echo ---
  claim e2e-b immediate 1Gi topolvm-immediate
} | kubectl apply -f -

# An expansion beyond what the node has.
{ claim e2e-b grow 2Gi topolvm-ssd; echo ---; pod e2e-b grow-app grow "$W3"; } | kubectl apply -f -
wait_bound e2e-b grow
kubectl -n e2e-b patch pvc grow --type=merge -p '{"spec":{"resources":{"requests":{"storage":"30Gi"}}}}'

# A released Retain volume, and an orphan: a released volume whose PV object is then deleted.
for c in released orphan; do
  { claim e2e-b "$c" 1Gi topolvm-retain; echo ---; pod e2e-b "$c-app" "$c" "$W2"; } | kubectl apply -f -
done
wait_bound e2e-b released orphan
orphan_pv="$(lv_of e2e-b orphan)"
kubectl -n e2e-b delete pod released-app orphan-app --wait=true
kubectl -n e2e-b delete pvc released orphan --wait=true
kubectl wait --for=jsonpath='{.status.phase}'=Released "pv/$orphan_pv" --timeout=120s
kubectl delete pv "$orphan_pv" --wait=true

# A deletion held by a foreign finalizer.
{ claim e2e-b stuck 1Gi topolvm-ssd; echo ---; pod e2e-b stuck-app stuck "$W3"; } | kubectl apply -f -
wait_bound e2e-b stuck
stuck_lv="$(lv_of e2e-b stuck)"
kubectl patch "logicalvolumes.$GROUP" "$stuck_lv" --type=json -p '[{"op":"add","path":"/metadata/finalizers/-","value":"e2e.topolvm/hold"}]'
kubectl -n e2e-b delete pod stuck-app --wait=true
kubectl -n e2e-b delete pvc stuck --wait=false
# TopoLVM removes the LV from disk, then its own finalizer; only the foreign one may stay.
for _ in $(seq 1 90); do
  [ "$(kubectl get "logicalvolumes.$GROUP" "$stuck_lv" -o jsonpath='{.metadata.finalizers}')" = '["e2e.topolvm/hold"]' ] && break
  sleep 2
done

# A LogicalVolume on a node that does not exist, with a volume ID that is not its uid.
kubectl apply -f - <<YAML
apiVersion: $GROUP/v1
kind: LogicalVolume
metadata: {name: ghost-volume}
spec: {name: ghost-volume, nodeName: e2e-ghost, size: 1Gi, deviceClass: ssd}
YAML
kubectl patch "logicalvolumes.$GROUP" ghost-volume --subresource=status --type=merge \
  -p '{"status":{"volumeID":"00000000-0000-0000-0000-00000000dead","currentSize":"1Gi"}}'

# On disk only: an LV named like a volume ID that no LogicalVolume knows about.
sudo lvcreate -q -L 256M -n 11111111-2222-3333-4444-555555555555 e2e-w1 >/dev/null 2>&1 || true

echo "scenarios in place"
