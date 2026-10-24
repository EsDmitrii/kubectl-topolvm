#!/usr/bin/env bash
# Remove what setup.sh and scenarios.sh created: the kind cluster, the volume groups and their loop
# devices and backing files. Touches only the e2e-* volume groups and files under WORK.
set -euo pipefail

CLUSTER="${CLUSTER:-topolvm-e2e}"
WORK="${WORK:-/var/tmp/topolvm-e2e}"
VGS=(e2e-w1 e2e-w2 e2e-w2-hdd e2e-w3)

kind delete cluster --name "$CLUSTER" 2>/dev/null || true
# ours VG: every PV of the volume group is a loop device backed by WORK/VG.img.
ours() {
  local pv back
  for pv in $(sudo pvs --noheadings -o pv_name -S vg_name="$1"); do
    back="$(sudo losetup -l -n -O BACK-FILE "$pv" 2>/dev/null | sed 's/ (deleted)$//')"
    [ "$back" = "$WORK/$1.img" ] || return 1
  done
}

for vg in "${VGS[@]}"; do
  if sudo vgs "$vg" >/dev/null 2>&1; then
    if ! ours "$vg"; then
      echo "volume group $vg is not backed by $WORK/$vg.img; leaving it alone" >&2
      continue
    fi
    sudo vgremove -qq -f "$vg" >/dev/null
  fi
  # lvm inside the kind nodes runs without udev and leaves its own device nodes in the shared /dev.
  for m in $(sudo dmsetup ls 2>/dev/null | awk -v p="${vg//-/--}-" 'index($1, p) == 1 && substr($1, length(p) + 1, 1) != "-" {print $1}'); do
    sudo dmsetup remove "$m"
  done
  if [ -d "/dev/$vg" ] && [ -z "$(find "/dev/$vg" -mindepth 1 ! -type l)" ]; then
    sudo rm -rf "/dev/$vg"
  fi
  # A loop device keeps its backing file even after the file is deleted, so match by name.
  for dev in $(sudo losetup -l -n -O NAME,BACK-FILE | awk -v f="$WORK/$vg.img" '$2 == f {print $1}'); do
    sudo pvremove -qq -ff -y "$dev" >/dev/null 2>&1 || true
    sudo losetup -d "$dev"
  done
  sudo rm -f "$WORK/$vg.img"
done
rm -f "$WORK/kubeconfig" "$WORK/values.yaml" "$WORK/kind.yaml"
echo "torn down"
