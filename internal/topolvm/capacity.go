package topolvm

import (
	"sort"
	"strconv"
	"strings"
)

// Node is a TopoLVM node as seen through its capacity annotations.
type Node struct {
	Name string
	// Capacity maps a device class name, or DefaultAlias, to free bytes.
	Capacity map[string]int64
	// BadAnnotations lists capacity annotation keys whose value is not an integer.
	BadAnnotations []string
}

// NodeFrom reads the capacity annotations of one node.
func NodeFrom(name string, annotations map[string]string, d Driver) Node {
	n := Node{Name: name, Capacity: map[string]int64{}}
	prefix := d.CapacityPrefix()
	for k, v := range annotations {
		class, ok := strings.CutPrefix(k, prefix)
		if !ok || class == "" {
			continue
		}
		free, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			n.BadAnnotations = append(n.BadAnnotations, k)
			continue
		}
		n.Capacity[class] = free
	}
	sort.Strings(n.BadAnnotations)
	return n
}

// NamedClasses lists the device classes with a capacity annotation, without DefaultAlias.
func (n Node) NamedClasses() []string {
	out := make([]string, 0, len(n.Capacity))
	for c := range n.Capacity {
		if c != DefaultAlias {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}
