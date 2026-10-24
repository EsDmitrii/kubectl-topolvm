# kubectl-topolvm

A read-only kubectl plugin for [TopoLVM](https://github.com/topolvm/topolvm), the node-local LVM
CSI driver. It shows how much of each node's pool is left, which claim sits on which
LogicalVolume, and finds leaked, released, stuck or failed volumes.

Unofficial: written and maintained outside the TopoLVM project.

```console
$ kubectl topolvm
NODE     DEVICE-CLASS    TYPE    SIZE      USED    FREE      USE%   LVS
node-1   ssd (default)   thick   315.5Gi   202Gi   113.5Gi   64%    14
node-2   ssd (default)   thick   315.5Gi   137Gi   178.5Gi   43%    14
node-3   ssd (default)   thick   315.5Gi   220Gi   95.5Gi    70%    9
*        ssd (default)   thick   946.6Gi   559Gi   387.6Gi   59%    37

$ kubectl topolvm check
CHECK                OBJECT                    NODE        SIZE     AGE      DETAIL
orphan-lv            logicalvolume/pvc-a1      node-a      10Gi     4d12h    no PersistentVolume with volumeHandle 00000000-0000-0000-0000-000000000001
released-pv          persistentvolume/pvc-a5   node-b      20Gi     <none>   PV is Released (reclaimPolicy Retain), the LV still holds the space
lv-on-missing-node   logicalvolume/pvc-a6      node-gone   5Gi      23d      node node-gone no longer exists
stuck-deletion       logicalvolume/pvc-a7      node-a      2Gi      120m     deletion requested 120m ago, finalizers: topolvm.io/logicalvolume
lv-error             logicalvolume/pvc-a10     node-b      20Gi     23d      expand failed: Internal: not enough space
resize-pending       logicalvolume/pvc-a10     node-b      30Gi     179m     spec.size 30Gi, current 20Gi
$ echo $?
2

$ kubectl topolvm fit 50Gi
NODE     DEVICE-CLASS   FREE    FREE-AFTER   FITS   COUNT   NOTES
node-a   ssd            100Gi   50Gi         yes    2       <none>
node-c   ssd            60Gi    10Gi         yes    1       taint dedicated=db:NoSchedule
node-b   ssd            30Gi    -            no     0       cordoned
fits on 2 of 3 nodes (want 1); the scheduler checks each claim on its own, treat this as an estimate
```

## Why

TopoLVM keeps what you need spread over several objects. Free space per device class sits in node
annotations (`capacity.topolvm.io/<class>`, plus a `00default` alias that repeats the default
class), allocated space in LogicalVolume objects that `kubectl get` prints with nothing but a name
and an age, and the claim behind each volume in the PersistentVolume that points at it.
`kubectl get csistoragecapacities` repeats the free figure once per StorageClass and hides the node
in a label selector. This plugin joins them and adds what a one-liner cannot: anomalies, placement
and the cost of losing a node.

## Install

With [krew](https://krew.sigs.k8s.io/):

```bash
kubectl krew install topolvm
```

Until the plugin is in the krew index, install the manifest attached to a release:

```bash
kubectl krew install --manifest-url=https://github.com/EsDmitrii/kubectl-topolvm/releases/latest/download/topolvm.yaml
```

By hand: download the archive for your platform from the
[releases](https://github.com/EsDmitrii/kubectl-topolvm/releases), put `kubectl-topolvm` (or
`kubectl-topolvm.exe`) somewhere on your `PATH` and run `kubectl topolvm`.

Linux, macOS and Windows on amd64 and arm64.

## Commands

| Command | What it does |
|---|---|
| `kubectl topolvm` / `pools` | Free and allocated space per node and device class, with a `*` total per class. `--fail-over 85` and `--min-free 50Gi` exit 2 on breach. `--metrics` adds the volume group size, space outside TopoLVM and thin pool fill. |
| `volumes` | Every LogicalVolume with its node, claim, StorageClass and status (Bound, Released, Orphan, Resizing, Error, Snapshot). `-o wide` adds the volume ID, which is the LV name `lvs` prints on the node. `--group-by namespace\|storageclass\|node\|device-class`. `--usage` adds filesystem usage from kubelet. |
| `check` | Read-only checks, exit 2 on any finding. Run `kubectl topolvm check --help` for the list. |
| `fit SIZE` | Which nodes can host a new volume. `--storage-class`, `--device-class`, `--replicas N` (exit 2 if fewer than N nodes fit). |
| `pending` | Why TopoLVM claims are Pending: Immediate binding, no consumer, no node with room, or a node without room. |
| `headroom [NS/]PVC [SIZE]` | How far a claim can grow on its node; with SIZE exit 2 if it cannot. Shows pvc-autoresizer annotations and the last resize failure. |
| `node NODE` | The node's pools and volumes, which workloads keep their only copy there, which claims TopoLVM deletes with the Node object, and whether the volumes would fit elsewhere. |
| `tree` | Node, device class, claim and pod as a tree. |
| `rbac` | Prints the ClusterRole and Role the plugin needs; `--check` tests them for the current user. |
| `dump --dir DIR --anonymize` | Writes what the plugin reads, cut down and renamed, for a bug report. |
| `lvs-diff --node NODE -f lvs.json` | Compares LogicalVolumes with `lvs` output you captured on the node: leaked LVs on disk, objects without an LV, and LVs the API places on another node (a capture from the wrong node). `lvs-diff --help` has a `kubectl debug` command that captures it. |

All commands take the usual kubectl flags (`--context`, `--kubeconfig`, `--as`, ...),
`-o table|wide|json|yaml`, `--no-headers`, `--bytes` and `--color auto|always|never` (auto honours
`NO_COLOR`). `node` and `headroom` print sections of key and value, which `--no-headers` and
`-o wide` leave as they are; `rbac` without `--check` always prints YAML manifests. `--node` and
`--device-class` narrow `pools`, `volumes`, `tree`, `check`, `pending` and `fit`, and a value that
matches no TopoLVM node or class is an error rather than an empty table. `-n` narrows `volumes`,
`tree`, `check` and `pending`; without it they cover every namespace, whatever namespace the
kubeconfig context sets.

JSON and YAML output carry `schemaVersion: v1`. Sizes are integers in bytes, a size or flag that is
unknown is `null`, and a name that does not apply (a volume without an owner, an event that did not
happen) is an empty string. Fields are only ever added.

Exit codes: `0` success, `1` error (flags, API, RBAC, no TopoLVM nodes found), `2` findings of
`check` or `lvs-diff`, a breached threshold, a volume or a claim's new size that does not fit, or a
permission `rbac --check` found missing.

## How the numbers are computed

- **FREE** is the capacity annotation topolvm-node writes for the class. For thick classes lvmd
  already keeps `spare-gb` (10 GiB by default) out of it.
- **USED** is the sum of the LogicalVolumes of that class on the node (`status.currentSize`, else
  `spec.size`). Volumes without `spec.deviceClass` belong to the default class. Only LVs on disk
  count: not one whose creation failed (no `status.volumeID`), nor one being deleted whose TopoLVM
  finalizer is already gone.
- **SIZE** is FREE plus USED: what TopoLVM can hand out, not the size of the volume group. With
  `--metrics` the wide output adds `VG-SIZE` and `OTHER = VG - spare - FREE - USED`, the space taken by
  LVs TopoLVM does not manage (a root LV, thin pool metadata, RAID overhead), less any thin pool in
  the same volume group. lvmd reports FREE 0 once the VG has less free space than spare-gb; OTHER is
  then unknown unless a thin class in that VG gives the raw free space.
- `00default` is an alias of the default class and is never counted as a class of its own.
- **Thin classes** (from the lvmd ConfigMap, LVMS LVMVolumeGroups, `--thin`, or a class with
  snapshots) report virtual, overprovisioned sizes; USE% is not shown for them, and `--metrics`
  adds the real data and metadata fill of the pool.
- When the type of a class cannot be read, TYPE shows `?` and USE% assumes thick.

## RBAC

The base role covers every command. Apply it with `kubectl topolvm rbac | kubectl apply -f -`
and bind it to the users or groups who should run the plugin:

```yaml
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubectl-topolvm-view
rules:
  - apiGroups: [""]
    resources: [nodes]
    verbs: [get, list]
  - apiGroups: [""]
    resources: [persistentvolumes, persistentvolumeclaims, pods, events]
    verbs: [list]
  - apiGroups: [storage.k8s.io]
    resources: [csidrivers]
    verbs: [get]
  - apiGroups: [storage.k8s.io]
    resources: [storageclasses, csinodes, csistoragecapacities]
    verbs: [list]
  - apiGroups: [topolvm.io]
    resources: [logicalvolumes]
    verbs: [list]
  - apiGroups: [topolvm.cybozu.com]
    resources: [logicalvolumes]
    verbs: [list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kubectl-topolvm-view
  namespace: topolvm-system
rules:
  - apiGroups: [""]
    resources: [configmaps, pods]
    verbs: [list]
```

Optional modes need more (`kubectl topolvm rbac --with metrics,usage,lvms`):

- `--metrics`: `get pods/proxy` in the TopoLVM namespace.
- `volumes --usage`: `get nodes/proxy`. This reaches the kubelet API, which includes exec into any
  pod on the node. Grant it only to people who may already exec on nodes.
- Red Hat LVMS: `list lvmvolumegroups.lvm.topolvm.io`, and pass `--topolvm-namespace openshift-storage`.

Without a right the plugin says which one is missing; optional data turns into a warning and the
output goes on without it. `kubectl topolvm rbac --check` lists what you have.

## Compatibility

- Driver `topolvm.io` (TopoLVM 0.16 and later) and the legacy `topolvm.cybozu.com`, detected from
  the CSIDriver object; `--driver` overrides it.
- Checked against live clusters on TopoLVM 0.41 and 0.42 with Kubernetes 1.31 and 1.35.
- The end-to-end suite in `hack/e2e` (kind, TopoLVM 0.42 from chart 17.3.0, Kubernetes 1.37) covers
  thin pools with snapshots and clones, several device classes and volume groups per node, node
  deletion and the legacy driver name. LVMS is covered by test fixtures only.

## Development

`make test` runs the unit tests and the golden files in `testdata/*/want`; `make golden` rewrites
them. `hack/e2e` builds a kind cluster with TopoLVM on loop devices and checks every command against
it. It needs a Linux host with docker, lvm2, thin-provisioning-tools, kind, helm, jq and sudo:

```bash
./hack/e2e/setup.sh
./hack/e2e/scenarios.sh
KT=./kubectl-topolvm ./hack/e2e/verify.sh
./hack/e2e/teardown.sh
```

The scripts only ever use their own kubeconfig in `/var/tmp/topolvm-e2e` and stop unless its
context is `kind-topolvm-e2e`. `LEGACY=1` on `setup.sh` and `scenarios.sh` installs TopoLVM under
`topolvm.cybozu.com`, and `API_ADDRESS=0.0.0.0` makes the API server reachable from outside the host
(it listens on 127.0.0.1 otherwise). CI runs both drivers.

## Verify a release

```bash
sha256sum -c --ignore-missing checksums.txt
gh attestation verify kubectl-topolvm_1.0.0_linux_amd64.tar.gz -R EsDmitrii/kubectl-topolvm
```

## k9s

Pick shortcuts that are free in your k9s setup.

```yaml
plugins:
  topolvm-node:
    shortCut: Shift-T
    description: TopoLVM node
    scopes: [nodes]
    command: bash
    background: false
    args: ["-c", "kubectl topolvm node \"$NAME\" --context \"$CONTEXT\" --color=always | less -RK"]
  topolvm-pvc:
    shortCut: Shift-T
    description: TopoLVM headroom
    scopes: [pvc]
    command: bash
    background: false
    args: ["-c", "kubectl topolvm headroom \"$NAMESPACE/$NAME\" --context \"$CONTEXT\" --color=always | less -RK"]
```

## Prior art

- [topolvm/kubectl-topolvm](https://github.com/topolvm/kubectl-topolvm) showed capacity per node.
  It is archived since 2022, targets the old `topolvm.cybozu.com` group, was never in krew and
  counts `00default` as a separate class.
- [kubectl-df-pv](https://github.com/yashbhutwala/kubectl-df-pv) shows filesystem usage of claims,
  not the LVM pools behind them.
- [kube-capacity](https://github.com/robscott/kube-capacity) and
  [kubectl-view-allocations](https://github.com/davidB/kubectl-view-allocations) cover CPU and
  memory.

## Bug reports

Attach the output of `kubectl topolvm dump --dir topolvm-dump --anonymize`. It keeps only what the
plugin reads, renames nodes, namespaces, claims, pods and owners, and loads straight into the
plugin's tests. Read it before you share it.

## License

[Apache-2.0](LICENSE)
