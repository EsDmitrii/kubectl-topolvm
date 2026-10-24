package fixture

import "testing"

func TestFixtureLoadOptionalFiles(t *testing.T) {
	in, err := Load(Path("no-configmap"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Classes != nil {
		t.Errorf("Classes = %v, want nil without configmaps.json", in.Classes)
	}
	in, err = Load(Path("single-thick"))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.PVs) != 2 || len(in.LVs) != 2 || len(in.Nodes) != 2 || in.Classes["ssd"].VolumeGroup != "vg0" {
		t.Errorf("single-thick = %d PVs, %d LVs, %d nodes, classes %v", len(in.PVs), len(in.LVs), len(in.Nodes), in.Classes)
	}
	if in.Nodes[0].Name != "node-a" || in.Nodes[0].Capacity["ssd"] != 121915834368 {
		t.Errorf("node-a = %+v", in.Nodes[0])
	}
}

func TestFixtureLoadMissingDir(t *testing.T) {
	if _, err := Load(Path("does-not-exist")); err == nil {
		t.Error("want an error for a missing fixture")
	}
}

func TestFixtureLoadExtended(t *testing.T) {
	in, err := Load(Path("single-thick"))
	if err != nil {
		t.Fatal(err)
	}
	if in.PVCs != nil || in.Pods != nil || in.Events != nil || in.StorageClasses != nil || in.CSIDriver != nil || in.NodeStates != nil {
		t.Errorf("absent files must stay nil: %+v", in)
	}
	in, err = Load(Path("extended"))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.PVCs) != 1 || len(in.Pods) != 1 || len(in.Events) != 1 || len(in.StorageClasses) != 1 || len(in.Capacities) != 1 ||
		len(in.CSINodes) != 1 || in.CSIDriver == nil || !in.NodeStates["node-a"].Ready || len(in.TopoLVMPods) != 1 ||
		in.Classes["vg1"].Type != "thin" {
		t.Errorf("extended = %+v", in)
	}
}
