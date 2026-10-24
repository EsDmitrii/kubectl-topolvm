package topolvm

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Device class types.
const (
	TypeThick   = "thick"
	TypeThin    = "thin"
	TypeUnknown = "unknown"
)

// defaultSpareGB is lvmd's spare-gb when the config leaves it out.
const defaultSpareGB = 10

// DeviceClass is one lvmd device class.
type DeviceClass struct {
	Name, VolumeGroup, ThinPool string
	Type                        string
	Default                     bool
	// SpareBytes is what lvmd keeps out of the free figure; nil for thin classes, which do not
	// subtract it.
	SpareBytes         *int64
	OverprovisionRatio float64
}

type lvmdConfig struct {
	DeviceClasses []struct {
		Name        string  `json:"name"`
		VolumeGroup string  `json:"volume-group"`
		Default     bool    `json:"default"`
		SpareGB     *uint64 `json:"spare-gb"`
		Type        string  `json:"type"`
		ThinPool    *struct {
			Name               string  `json:"name"`
			OverprovisionRatio float64 `json:"overprovision-ratio"`
		} `json:"thin-pool"`
	} `json:"device-classes"`
}

// ParseLVMDConfig parses lvmd.yaml.
func ParseLVMDConfig(data string) ([]DeviceClass, error) {
	var cfg lvmdConfig
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		return nil, err
	}
	out := make([]DeviceClass, 0, len(cfg.DeviceClasses))
	for _, c := range cfg.DeviceClasses {
		dc := DeviceClass{Name: c.Name, VolumeGroup: c.VolumeGroup, Default: c.Default, Type: TypeThick}
		switch c.Type {
		case "", TypeThick:
			spare := uint64(defaultSpareGB)
			if c.SpareGB != nil {
				spare = *c.SpareGB
			}
			b := int64(spare << 30) //nolint:gosec // spare-gb is a small config value
			dc.SpareBytes = &b
		case TypeThin:
			dc.Type = TypeThin
			if c.ThinPool != nil {
				dc.ThinPool = c.ThinPool.Name
				dc.OverprovisionRatio = c.ThinPool.OverprovisionRatio
			}
		default:
			dc.Type = TypeUnknown
		}
		out = append(out, dc)
	}
	return out, nil
}

// ClassesFromConfigMaps merges the device classes of every ConfigMap that carries lvmd.yaml into one
// cluster-wide view. Node groups may give a class different volume groups and spare sizes, which
// leaves those fields empty here (NodeClasses has them per node); a class with different types gets
// type unknown. It returns nil when no ConfigMap has an lvmd config.
func ClassesFromConfigMaps(cms []corev1.ConfigMap) (classes map[string]DeviceClass, warnings []string) {
	sorted := append([]corev1.ConfigMap(nil), cms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, cm := range sorted {
		data, ok := cm.Data["lvmd.yaml"]
		if !ok {
			continue
		}
		parsed, err := ParseLVMDConfig(data)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("configmap %s: cannot parse lvmd.yaml: %v", cm.Name, err))
			continue
		}
		if classes == nil {
			classes = map[string]DeviceClass{}
		}
		for _, dc := range parsed {
			prev, seen := classes[dc.Name]
			if !seen {
				classes[dc.Name] = dc
				continue
			}
			if prev.Type != dc.Type && prev.Type != TypeUnknown {
				warnings = append(warnings, fmt.Sprintf(
					"device class %q has different types in several lvmd ConfigMaps; its type is unknown", dc.Name))
				prev.Type = TypeUnknown
			}
			if prev.VolumeGroup != dc.VolumeGroup {
				prev.VolumeGroup = ""
			}
			if prev.SpareBytes == nil || dc.SpareBytes == nil || *prev.SpareBytes != *dc.SpareBytes {
				prev.SpareBytes = nil
			}
			prev.Default = prev.Default && dc.Default
			classes[dc.Name] = prev
		}
	}
	return classes, warnings
}

// NodeClasses maps each node to the device classes of the lvmd ConfigMap its lvmd pod mounts (the
// topolvm-node pod when lvmd is embedded). Nodes whose pod mounts no lvmd ConfigMap are left out.
func NodeClasses(pods []corev1.Pod, cms []corev1.ConfigMap) map[string]map[string]DeviceClass {
	byConfigMap := map[string]map[string]DeviceClass{}
	for _, cm := range cms {
		data, ok := cm.Data["lvmd.yaml"]
		if !ok {
			continue
		}
		parsed, err := ParseLVMDConfig(data)
		if err != nil {
			continue
		}
		classes := map[string]DeviceClass{}
		for _, dc := range parsed {
			classes[dc.Name] = dc
		}
		byConfigMap[cm.Name] = classes
	}
	out := map[string]map[string]DeviceClass{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.ConfigMap == nil {
				continue
			}
			if classes, ok := byConfigMap[v.ConfigMap.Name]; ok {
				out[p.Spec.NodeName] = classes
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ClassesFromLVMVolumeGroups reads device classes from Red Hat LVMS, which writes lvmd.yaml only to
// the host and keeps one LVMVolumeGroup object per class. It returns nil for no objects.
func ClassesFromLVMVolumeGroups(items []unstructured.Unstructured) map[string]DeviceClass {
	if len(items) == 0 {
		return nil
	}
	classes := make(map[string]DeviceClass, len(items))
	for i := range items {
		u := &items[i]
		dc := DeviceClass{Name: u.GetName(), VolumeGroup: u.GetName(), Type: TypeThick}
		dc.Default, _, _ = unstructured.NestedBool(u.Object, "spec", "default")
		if pool, found, _ := unstructured.NestedMap(u.Object, "spec", "thinPoolConfig"); found {
			dc.Type = TypeThin
			dc.ThinPool, _, _ = unstructured.NestedString(pool, "name")
			switch r := pool["overprovisionRatio"].(type) {
			case int64:
				dc.OverprovisionRatio = float64(r)
			case float64:
				dc.OverprovisionRatio = r
			}
		} else {
			dc.SpareBytes = new(int64(0))
		}
		classes[dc.Name] = dc
	}
	if len(classes) == 1 {
		for name, dc := range classes {
			dc.Default = true
			classes[name] = dc
		}
	}
	return classes
}
