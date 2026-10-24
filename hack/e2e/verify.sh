#!/usr/bin/env bash
# Check every command of the plugin against the cluster setup.sh and scenarios.sh built. Needs jq and
# docker; KT is the plugin binary. The last step deletes the third worker; DELETE_NODE=0 skips it.
# jq filters below use jq variables, not shell ones:
# shellcheck disable=SC2016
set -uo pipefail

CLUSTER="${CLUSTER:-topolvm-e2e}"
WORK="${WORK:-/var/tmp/topolvm-e2e}"
# Always our own kubeconfig: the scripts create, patch and delete objects.
export KUBECONFIG="$WORK/kubeconfig"
KT="${KT:-kubectl-topolvm}"
DELETE_NODE="${DELETE_NODE:-1}"
W1="$CLUSTER-worker" W2="$CLUSTER-worker2" W3="$CLUSTER-worker3"
GI=1073741824
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT
if [ "$(kubectl config current-context 2>/dev/null)" != "kind-$CLUSTER" ]; then
  echo "$KUBECONFIG does not point at kind-$CLUSTER; run setup.sh first" >&2
  exit 1
fi

fails=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }

# run NAME CMD...: stdout to $OUT/NAME.json, stderr to $OUT/NAME.err, exit code to $OUT/NAME.rc.
run() {
  local name="$1"; shift
  "$@" >"$OUT/$name.json" 2>"$OUT/$name.err"
  echo $? >"$OUT/$name.rc"
}

# expect NAME RC DESCRIPTION FILTER: the exit code of run NAME is RC and FILTER yields true on its output.
# every is all on a non-empty array: an empty selection must fail, not pass.
expect() {
  local name="$1" rc="$2" what="$3" filter="$4" got
  got="$(cat "$OUT/$name.rc")"
  if [ "$got" != "$rc" ]; then
    fail "$what: exit $got, want $rc: $(head -c 300 "$OUT/$name.err")"
  elif jq -e --arg w1 "$W1" --arg w2 "$W2" --arg w3 "$W3" --argjson gi "$GI" "def every: length > 0 and all; $filter" \
    "$OUT/$name.json" >/dev/null; then
    pass "$what"
  else
    fail "$what"
    jq -c . "$OUT/$name.json" | head -c 2000
    echo
  fi
}

# stderr NAME DESCRIPTION REGEX COUNT: REGEX matches COUNT lines of the stderr of run NAME.
stderr() {
  local got
  got="$(grep -cE "$3" "$OUT/$1.err")"
  if [ "$got" = "$4" ]; then pass "$2"; else fail "$2: $got lines match, want $4: $(cat "$OUT/$1.err")"; fi
}

lvs_of() { # lvs_of NODE: lvs on a kind node, as lvs-diff wants it
  docker exec "$1" lvs --reportformat json -o lv_name,vg_name,lv_size --units b --nosuffix
}

run pools "$KT" pools -o json
expect pools 0 "thick pools: SIZE is USED plus FREE" \
  '[.pools[] | select(.type == "thick") | .sizeBytes == .usedBytes + .freeBytes] | every'
expect pools 0 "per-node classes: thin only on w1, hdd only on w2" \
  '([.pools[] | select(.type == "thin") | .node] == [$w1]) and ([.pools[] | select(.deviceClass == "hdd") | .node] == [$w2])'
expect pools 0 "USED skips an LV whose disk LV is already removed" \
  '[.pools[] | select(.node == $w3 and .deviceClass == "ssd") | .usedBytes == 3 * $gi and .volumes == 2] | every'
expect pools 0 "thin pool: three volumes, one of them a snapshot" \
  '[.pools[] | select(.type == "thin") | .volumes == 3 and .usedBytes == 6 * $gi and .usedPercent == null] | every'

run metrics "$KT" pools -o json --metrics
expect metrics 0 "--metrics: VG size and OTHER on every thick pool, never negative" \
  '[.pools[] | select(.type == "thick") | .volumeGroupSizeBytes > 0 and .otherBytes >= 0] | every'
expect metrics 0 "--metrics: thin data and metadata use" \
  '[.pools[] | select(.type == "thin") | .thinDataPercent > 0 and .thinMetadataPercent > 0] | every'

run volumes "$KT" volumes -o json
expect volumes 0 "volumes: snapshot and clone kinds" \
  '([.volumes[] | select(.kind == "snapshot")] | length) == 1 and ([.volumes[] | select(.pvc == "thin-clone")][0].kind == "clone")'
expect volumes 0 "volumes: the held deletion shows as Deleting" \
  '[.volumes[] | select(.pvc == "stuck")][0].status == "Deleting"'

# kubelet refreshes volume stats once a minute.
usage='[.volumes[] | select(.pvc == "hdd-data")][0] | .fsUsedBytes > 0 and .idleBytes > 0'
for _ in $(seq 1 12); do
  run usage "$KT" volumes -o json --usage -n e2e-a
  jq -e "$usage" "$OUT/usage.json" >/dev/null && break
  sleep 10
done
expect usage 0 "--usage: filesystem use of a mounted volume" "$usage"

run check "$KT" check -o json --min-age 0
expect check 2 "check: every planted anomaly is found" '
  [.findings[] | .check + " " + .object] as $f
  | ["lv-on-missing-node logicalvolume/ghost-volume", "uid-mismatch logicalvolume/ghost-volume",
     "orphan-lv logicalvolume/ghost-volume", "immediate-binding storageclass/topolvm-immediate",
     "pvc-pending persistentvolumeclaim/e2e-b/too-big", "pvc-resize-stuck persistentvolumeclaim/e2e-b/grow"]
  | all(. as $want | $f | index($want))'
expect check 2 "check: a Retain and a Delete PV stay Released" \
  '[.findings[] | select(.check == "released-pv")] | length == 2'
expect check 2 "check: the Delete PV explains why it stays" \
  '[.findings[] | select(.check == "released-pv" and (.detail | contains("reclaimPolicy Delete) and was not deleted, the LV is already removed from disk")))] | length == 1 and .[0].sizeBytes == null'
expect check 2 "check: the held deletion names the foreign finalizer" \
  '[.findings[] | select(.check == "stuck-deletion")] | length == 1 and (.[0].detail | contains("e2e.topolvm/hold")) and .[0].sizeBytes == null'
expect check 2 "check: the orphaned LV of a deleted PV" \
  '[.findings[] | select(.check == "orphan-lv")] | length == 2'

run pending "$KT" pending -o json
expect pending 0 "pending: no consumer and too big" '
  (.pending | map({key: .pvc, value: .reason}) | from_entries) as $r
  | $r["no-consumer"] == "NoConsumer" and $r["too-big"] == "Unschedulable"
  and ([.pending[] | select(.pvc == "too-big")][0].detail | startswith("no node has 50Gi free in class ssd"))'

run fit "$KT" fit 10Gi -o json
expect fit 0 "fit 10Gi: every worker" '.fit.nodesThatFit == 3 and .fit.ok'
run fit2 "$KT" fit 15800Mi --replicas 2 -o json
expect fit2 2 "fit 15800Mi x2: only w3, exit 2" '.fit.nodesThatFit == 1 and .fit.rows[0].node == $w3 and (.fit.ok | not)'
run fithdd "$KT" fit 3Gi --storage-class topolvm-hdd -o json
expect fithdd 0 "fit by StorageClass: the hdd class of w2 only" '[.fit.rows[] | .node + "/" + .deviceClass] == [$w2 + "/hdd"]'

run headroom "$KT" headroom -n e2e-b grow -o json
expect headroom 0 "headroom: the failed expansion and its node" \
  '.headroom.node == $w3 and (.headroom.lastResizeFailure | contains("not enough space"))'
run headroomthin "$KT" headroom -n e2e-a thin-src -o json
expect headroomthin 0 "headroom: no maximum on a thin pool" '.headroom.type == "thin" and .headroom.maxBytes == null'

run node "$KT" node "$W3" -o json
expect node 0 "node: TopoLVM deletes only the claim with selected-node" '.node.deletedOnNodeDelete == ["e2e-b/grow"]'
expect node 0 "node: every LogicalVolume goes, two PVs stay without one" \
  '.node.logicalVolumesDeletedOnNodeDelete == 3 and .node.claimsLeftWithoutLV == ["e2e-b/immediate", "e2e-b/stuck"]'
expect node 0 "node: a volume being deleted is not evacuated" '[.node.evacuation[].volume] | index("e2e-b/stuck") | not'

run tree "$KT" tree -o json
expect tree 0 "tree: all three workers" '[.tree.pools[].node] | unique | length == 3'

lvs_of "$W1" >"$OUT/lvs-w1.json"
run diff1 "$KT" lvs-diff --node "$W1" -f "$OUT/lvs-w1.json" -o json
expect diff1 2 "lvs-diff: the LV only on disk" \
  '[.lvs[] | select(.state == "disk-only") | .lv] == ["11111111-2222-3333-4444-555555555555"]'
lvs_of "$W3" >"$OUT/lvs-w3.json"
run diff3 "$KT" lvs-diff --node "$W3" -f "$OUT/lvs-w3.json" -o json
expect diff3 2 "lvs-diff: the LogicalVolume whose LV is gone" '[.lvs[] | select(.state == "cr-only") | .pvc] == ["e2e-b/stuck"]'
lvs_of "$W2" >"$OUT/lvs-w2.json"
run diff2 "$KT" lvs-diff --node "$W2" -f "$OUT/lvs-w2.json" -o json
expect diff2 0 "lvs-diff: w2 matches" '[.lvs[] | .state == "ok"] | every'

if "$KT" dump --anonymize --dir "$OUT/dump" >/dev/null 2>&1 && ! grep -rqE 'e2e-a|e2e-b|topolvm-e2e|thin-src|too-big' "$OUT/dump"; then
  pass "dump --anonymize: no namespace, node or claim names"
else
  fail "dump --anonymize: $(grep -rhoE 'e2e-a|e2e-b|topolvm-e2e|thin-src|too-big' "$OUT/dump" | sort | uniq -c | head -5)"
fi

AS=(--as system:serviceaccount:default:topolvm-viewer)
kubectl create serviceaccount topolvm-viewer -n default --dry-run=client -o yaml | kubectl apply -f - >/dev/null
"$KT" rbac | kubectl apply -f - >/dev/null
kubectl create clusterrolebinding topolvm-viewer --clusterrole=kubectl-topolvm-view --serviceaccount=default:topolvm-viewer \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n topolvm-system create rolebinding topolvm-viewer --role=kubectl-topolvm-view --serviceaccount=default:topolvm-viewer \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
run rbac "$KT" rbac --check -o json "${AS[@]}"
expect rbac 0 "rbac: the printed roles cover the base commands" '[.permissions[] | .allowed] | every'
run rbacmetrics "$KT" rbac --check --with metrics -o json "${AS[@]}"
expect rbacmetrics 2 "rbac: --with metrics is missing, exit 2" '[.permissions[] | select(.allowed | not) | .resource] == ["pods/proxy"]'
run asviewer "$KT" pools --metrics -o json "${AS[@]}"
expect asviewer 0 "pools --metrics under the viewer role: one warning naming the rule" \
  '(.pools | length == 5) and ([.warnings[] | select(contains("cannot get pods/proxy in topolvm-system: forbidden"))] | length == 1)'
run asviewertable "$KT" volumes --usage "${AS[@]}"
stderr asviewertable "volumes --usage under the viewer role: one warning line" 'cannot get nodes/proxy: forbidden' 1

run wrongns "$KT" pools --topolvm-namespace nope -o json
expect wrongns 0 "a wrong --topolvm-namespace is named" '[.warnings[] | select(startswith("no pods in namespace nope"))] | length == 1'

if [ "$DELETE_NODE" = 1 ]; then
  docker stop "$W3" >/dev/null
  kubectl delete node "$W3" --wait=true --timeout=180s >/dev/null
  for _ in $(seq 1 90); do
    kubectl -n e2e-b get pvc grow >/dev/null 2>&1 || break
    sleep 2
  done
  if kubectl -n e2e-b get pvc grow >/dev/null 2>&1; then fail "node delete: TopoLVM deleted e2e-b/grow"; else pass "node delete: TopoLVM deleted e2e-b/grow"; fi
  if kubectl -n e2e-b get pvc immediate >/dev/null 2>&1; then pass "node delete: the claim without selected-node stays"; else fail "node delete: the claim without selected-node stays"; fi
  run afterdelete "$KT" pools -o json
  expect afterdelete 0 "node delete: w3 is gone from pools" '[.pools[] | select(.node == $w3)] | length == 0'
  run afterdeletecheck "$KT" check -o json --min-age 0
  expect afterdeletecheck 2 "node delete: the PV of e2e-b/immediate is left without an LV" \
    '[.findings[] | select(.check == "pv-without-lv" and .namespace == "e2e-b")] | length == 1'
fi

echo "$fails failed"
[ "$fails" = 0 ]
