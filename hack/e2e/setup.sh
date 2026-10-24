#!/usr/bin/env bash
# Build a kind cluster with TopoLVM on loop-backed volume groups. Runs on a Linux host with docker,
# lvm2, thin-provisioning-tools, kind, kubectl, helm and sudo. LEGACY=1 installs TopoLVM under its
# old driver name, topolvm.cybozu.com.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER="${CLUSTER:-topolvm-e2e}"
WORK="${WORK:-/var/tmp/topolvm-e2e}"
CHART_VERSION="${CHART_VERSION:-17.3.0}"
SNAPSHOTTER_VERSION="${SNAPSHOTTER_VERSION:-v8.2.1}"
# Always our own kubeconfig: the scripts create, patch and delete objects.
export KUBECONFIG="$WORK/kubeconfig"
DRIVER=topolvm.io
[ "${LEGACY:-0}" = 1 ] && DRIVER=topolvm.cybozu.com

mkdir -p "$WORK"

# vg NAME SIZE: a volume group on a loop device backed by a sparse file.
vg() {
  local name="$1" size="$2" file="$WORK/$1.img" dev
  if sudo vgs "$name" >/dev/null 2>&1; then
    if ! sudo losetup -l -n -O BACK-FILE | grep -qx "$file"; then
      echo "volume group $name exists and is not backed by $file; not touching it" >&2
      exit 1
    fi
    return
  fi
  sudo truncate -s "$size" "$file"
  dev="$(sudo losetup --find --show "$file")"
  sudo vgcreate -q "$name" "$dev" >/dev/null
}

vg e2e-w1 20G
vg e2e-w2 20G
vg e2e-w2-hdd 10G
vg e2e-w3 20G
sudo lvs e2e-w1/pool0 >/dev/null 2>&1 || sudo lvcreate -q -T -L 6G -n pool0 e2e-w1 >/dev/null

if ! kind get clusters | grep -qx "$CLUSTER"; then
  sed "s|apiServerAddress: .*|apiServerAddress: \"${API_ADDRESS:-127.0.0.1}\"|" "$HERE/kind.yaml" >"$WORK/kind.yaml"
  kind create cluster --config "$WORK/kind.yaml" --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 180s
fi
if [ "$(kubectl config current-context 2>/dev/null)" != "kind-$CLUSTER" ]; then
  echo "$KUBECONFIG does not point at kind-$CLUSTER; run setup.sh first" >&2
  exit 1
fi

# lvmd runs lvm in the node's namespaces (hostPID), so the kind nodes need lvm2. They have no udev:
# LVM must create device nodes itself and not wait for udev events.
for n in $(kind get nodes --name "$CLUSTER" | grep worker); do
  docker exec "$n" bash -euc '
    command -v lvm >/dev/null || {
      apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq lvm2 thin-provisioning-tools >/dev/null
    }
    cat > /etc/lvm/lvmlocal.conf <<CONF
activation {
	udev_sync = 0
	udev_rules = 0
}
devices {
	obtain_device_list_from_udev = 0
}
CONF
  '
done

kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=${SNAPSHOTTER_VERSION}"
kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=${SNAPSHOTTER_VERSION}"

sed "s|topolvm.io/|$DRIVER/|; s|^useLegacy: false|useLegacy: $([ "$DRIVER" = topolvm.io ] && echo false || echo true)|" \
  "$HERE/values.yaml" >"$WORK/values.yaml"
helm upgrade --install topolvm topolvm --repo https://topolvm.github.io/topolvm --version "$CHART_VERSION" \
  --namespace topolvm-system --create-namespace -f "$WORK/values.yaml" --wait --timeout 10m

kubectl apply -f - <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: topolvm-thin
driver: $DRIVER
deletionPolicy: Delete
YAML

echo "waiting for CSIStorageCapacity objects"
for _ in $(seq 1 60); do
  [ "$(kubectl get csistoragecapacities -A --no-headers 2>/dev/null | wc -l)" -ge 9 ] && break
  sleep 5
done
kubectl get csistoragecapacities -A
echo "cluster ready; KUBECONFIG=$KUBECONFIG"
