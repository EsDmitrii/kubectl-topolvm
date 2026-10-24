# Changelog

## v1.0.0

First release.

- `pools` (default): free and allocated space per node and device class, without counting the
  `00default` alias twice; thin classes marked; `--fail-over` and `--min-free` for cron and CI;
  `--metrics` adds the volume group size, space outside TopoLVM and thin pool fill.
- `volumes`: LogicalVolumes joined to PersistentVolumes by volumeHandle, with status; `--group-by`;
  `--usage` adds filesystem usage from kubelet.
- `check`: leaked, released, stuck and failed volumes and capacity tracking problems, exit 2 on
  findings.
- `fit`, `headroom`, `pending`, `node`, `tree`.
- `rbac` prints the roles the plugin needs and checks them; `dump` writes an anonymized bug report;
  `lvs-diff` compares LogicalVolumes with `lvs` output from a node.
- Both driver names, `topolvm.io` and the legacy `topolvm.cybozu.com`; device classes from the lvmd
  ConfigMap or, on Red Hat LVMS, from LVMVolumeGroups.
