// Package fixture loads a directory of kubectl-style JSON dumps into a report.Input. The same
// layout is what the dump command writes, so a bug report is a ready-made test case.
package fixture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// Meta is meta.json: what a dump cannot carry inside the Kubernetes objects.
type Meta struct {
	Driver      string    `json:"driver"`
	Now         time.Time `json:"now"`
	AbsentNodes []string  `json:"absentNodes"`
	Thin        []string  `json:"thin"`
	// SchedulerExtender is omitted from dumps of clusters without topolvm-scheduler.
	SchedulerExtender bool `json:"schedulerExtender,omitempty"`
}

// Path is <repo root>/testdata/<name>.
func Path(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", name)
}

// readJSON decodes dir/name into v. It reports false when the file does not exist, which means
// the data was not collected.
func readJSON(dir, name string, v any) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return true, nil
}

// Load reads a fixture directory.
func Load(dir string) (report.Input, error) {
	var meta Meta
	found, err := readJSON(dir, "meta.json", &meta)
	if err != nil {
		return report.Input{}, err
	}
	if !found {
		return report.Input{}, fmt.Errorf("%s: meta.json not found", dir)
	}
	driver, err := topolvm.ParseDriver(meta.Driver)
	if err != nil {
		return report.Input{}, err
	}
	in := report.Input{Driver: driver, Now: meta.Now.UTC(), Thin: meta.Thin, AbsentNodes: map[string]bool{}, SchedulerExtender: meta.SchedulerExtender}
	for _, n := range meta.AbsentNodes {
		in.AbsentNodes[n] = true
	}

	var nodes corev1.NodeList
	if _, err := readJSON(dir, "nodes.json", &nodes); err != nil {
		return report.Input{}, err
	}
	for i := range nodes.Items {
		in.Nodes = append(in.Nodes, topolvm.NodeFrom(nodes.Items[i].Name, nodes.Items[i].Annotations, driver))
	}
	sort.Slice(in.Nodes, func(i, j int) bool { return in.Nodes[i].Name < in.Nodes[j].Name })

	var lvs struct {
		Items []map[string]any `json:"items"`
	}
	if _, err := readJSON(dir, "logicalvolumes.json", &lvs); err != nil {
		return report.Input{}, err
	}
	for _, obj := range lvs.Items {
		lv, err := topolvm.LogicalVolumeFrom(&unstructured.Unstructured{Object: obj})
		if err != nil {
			in.Warnings = append(in.Warnings, err.Error()+"; skipped")
			in.SkippedLVs = append(in.SkippedLVs, lv.Key())
			continue
		}
		in.LVs = append(in.LVs, lv)
	}

	var pvs corev1.PersistentVolumeList
	if found, err := readJSON(dir, "pvs.json", &pvs); err != nil {
		return report.Input{}, err
	} else if found {
		in.PVs = append([]corev1.PersistentVolume{}, pvs.Items...)
	}

	var cms corev1.ConfigMapList
	if found, err := readJSON(dir, "configmaps.json", &cms); err != nil {
		return report.Input{}, err
	} else if found {
		classes, warnings := topolvm.ClassesFromConfigMaps(cms.Items)
		in.Classes = classes
		in.Warnings = append(in.Warnings, warnings...)
		in.LVMDConfigMaps = cms.Items
	}
	var configPods corev1.PodList
	if found, err := readJSON(dir, "configpods.json", &configPods); err != nil {
		return report.Input{}, err
	} else if found {
		in.ConfigPods = configPods.Items
		in.NodeClasses = topolvm.NodeClasses(in.ConfigPods, cms.Items)
	}
	if in.Classes == nil {
		var vgs struct {
			Items []map[string]any `json:"items"`
		}
		if found, err := readJSON(dir, "lvmvolumegroups.json", &vgs); err != nil {
			return report.Input{}, err
		} else if found {
			items := make([]unstructured.Unstructured, 0, len(vgs.Items))
			for _, obj := range vgs.Items {
				items = append(items, unstructured.Unstructured{Object: obj})
			}
			in.Classes = topolvm.ClassesFromLVMVolumeGroups(items)
		}
	}
	if err := loadExtended(dir, &in); err != nil {
		return report.Input{}, err
	}
	return in, nil
}

// loadExtended reads the optional objects; a missing file leaves its field nil.
func loadExtended(dir string, in *report.Input) error {
	var (
		pvcs  corev1.PersistentVolumeClaimList
		pods  corev1.PodList
		evs   corev1.EventList
		scs   storagev1.StorageClassList
		cscs  storagev1.CSIStorageCapacityList
		nodes storagev1.CSINodeList
		tpods corev1.PodList
		drv   storagev1.CSIDriver
	)
	steps := []struct {
		file string
		into any
		set  func()
	}{
		{"pvcs.json", &pvcs, func() { in.PVCs = append([]corev1.PersistentVolumeClaim{}, pvcs.Items...) }},
		{"pods.json", &pods, func() { in.Pods = append([]corev1.Pod{}, pods.Items...) }},
		{"events.json", &evs, func() { in.Events = append([]corev1.Event{}, evs.Items...) }},
		{"storageclasses.json", &scs, func() { in.StorageClasses = append([]storagev1.StorageClass{}, scs.Items...) }},
		{"csistoragecapacities.json", &cscs, func() { in.Capacities = append([]storagev1.CSIStorageCapacity{}, cscs.Items...) }},
		{"csinodes.json", &nodes, func() { in.CSINodes = append([]storagev1.CSINode{}, nodes.Items...) }},
		{"topolvmpods.json", &tpods, func() { in.TopoLVMPods = append([]corev1.Pod{}, tpods.Items...) }},
		{"csidriver.json", &drv, func() { in.CSIDriver = &drv }},
		{"nodestates.json", &in.NodeStates, func() {}},
		{"metrics.json", &in.Metrics, func() {}},
		{"usage.json", &in.Usage, func() {}},
	}
	for _, st := range steps {
		found, err := readJSON(dir, st.file, st.into)
		if err != nil {
			return err
		}
		if found {
			st.set()
		}
	}
	return nil
}
