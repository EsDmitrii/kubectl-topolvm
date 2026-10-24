package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// proxyConcurrency bounds the per-pod and per-node GETs through the API server proxy.
const proxyConcurrency = 8

// proxyTimeout bounds one proxied read: the API server never times out a proxy request, and a
// stuck kubelet or topolvm-node would hang the command.
var proxyTimeout = 15 * time.Second

// rawWithin is c.Raw bounded by proxyTimeout, with a readable error when it runs out.
func (c *Clients) rawWithin(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, proxyTimeout)
	defer cancel()
	body, err := c.Raw(ctx, path)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("no answer in %s", proxyTimeout)
	}
	return body, err
}

// defaultMetricsPort is the topolvm-node metrics port when the pod does not name one.
const defaultMetricsPort = 8080

var metricFields = map[string]func(*report.ClassMetrics, float64){
	"topolvm_volumegroup_size_bytes":      func(m *report.ClassMetrics, v float64) { m.VGSizeBytes = new(int64(v)) },
	"topolvm_volumegroup_available_bytes": func(m *report.ClassMetrics, v float64) { m.VGAvailableBytes = new(int64(v)) },
	"topolvm_thinpool_size_bytes":         func(m *report.ClassMetrics, v float64) { m.ThinPoolSizeBytes = new(int64(v)) },
	"topolvm_thinpool_data_percent":       func(m *report.ClassMetrics, v float64) { m.ThinDataPercent = new(v) },
	"topolvm_thinpool_metadata_percent":   func(m *report.ClassMetrics, v float64) { m.ThinMetadataPercent = new(v) },
}

// parseLabels reads `a="x",b="y"` with Prometheus escaping.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 || eq+1 >= len(s) || s[eq+1] != '"' {
			return out
		}
		key := strings.TrimSpace(s[:eq])
		var b strings.Builder
		i := eq + 2
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		out[key] = b.String()
		s = strings.TrimLeft(s[min(i+1, len(s)):], ", ")
	}
	return out
}

// ParseMetrics reads the TopoLVM gauges from a Prometheus text exposition, keyed by node and
// device class.
func ParseMetrics(r io.Reader) (map[string]map[string]report.ClassMetrics, error) {
	out := map[string]map[string]report.ClassMetrics{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, rest, hasLabels := strings.Cut(line, "{")
		set, ok := metricFields[name]
		if !ok || !hasLabels {
			continue
		}
		labels, value, ok := strings.Cut(rest, "}")
		if !ok {
			return nil, fmt.Errorf("bad metric line %q", line)
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return nil, fmt.Errorf("bad metric line %q", line)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("bad metric value in %q: %w", line, err)
		}
		l := parseLabels(labels)
		node, class := l["node"], l["device_class"]
		if out[node] == nil {
			out[node] = map[string]report.ClassMetrics{}
		}
		m := out[node][class]
		set(&m, v)
		out[node][class] = m
	}
	return out, sc.Err()
}

type kubeletSummary struct {
	Pods []struct {
		Volume []struct {
			PVCRef *struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"pvcRef"`
			UsedBytes     *int64 `json:"usedBytes"`
			CapacityBytes *int64 `json:"capacityBytes"`
			InodesUsed    *int64 `json:"inodesUsed"`
			Inodes        *int64 `json:"inodes"`
		} `json:"volume"`
	} `json:"pods"`
}

// ParseSummary reads claim volumes from a kubelet stats/summary document, keyed by namespace/claim.
func ParseSummary(r io.Reader) (map[string]report.VolumeUsage, error) {
	var s kubeletSummary
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return nil, err
	}
	out := map[string]report.VolumeUsage{}
	for _, p := range s.Pods {
		for _, v := range p.Volume {
			if v.PVCRef == nil {
				continue
			}
			out[v.PVCRef.Namespace+"/"+v.PVCRef.Name] = report.VolumeUsage{
				UsedBytes: v.UsedBytes, CapacityBytes: v.CapacityBytes, InodesUsed: v.InodesUsed, Inodes: v.Inodes,
			}
		}
	}
	return out, nil
}

func metricsPort(p *corev1.Pod) int32 {
	for i := range p.Spec.Containers {
		c := &p.Spec.Containers[i]
		if c.Name != "topolvm-node" {
			continue
		}
		for _, port := range c.Ports {
			if port.Name == "metrics" {
				return port.ContainerPort
			}
		}
	}
	return defaultMetricsPort
}

// fanOut runs fn for every key with bounded concurrency and collects warnings in key order.
func fanOut(keys []string, fn func(key string) string) []string {
	warnings := make([]string, len(keys))
	sem := make(chan struct{}, proxyConcurrency)
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			warnings[i] = fn(k)
		})
	}
	wg.Wait()
	out := []string{}
	for _, w := range warnings {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

// refused sums up proxy reads RBAC refused: it refuses them all alike, so one warning naming the
// rule to grant says more than one per node.
func refused(resource string, n, total int, what, unit, mode string) []string {
	if n == 0 {
		return nil
	}
	return []string{fmt.Sprintf("cannot get %s: forbidden; no %s from %d of %d %s (kubectl topolvm rbac --with %s prints the rule)",
		resource, what, n, total, unit, mode)}
}

// collectMetrics reads topolvm-node metrics through the pod proxy.
func (c *Clients) collectMetrics(ctx context.Context, in *report.Input) {
	pods := map[string]*corev1.Pod{}
	keys := make([]string, 0, len(in.TopoLVMPods))
	for i := range in.TopoLVMPods {
		p := &in.TopoLVMPods[i]
		pods[p.Name] = p
		keys = append(keys, p.Name)
	}
	sort.Strings(keys)
	var (
		mu        sync.Mutex
		forbidden atomic.Int32
	)
	in.Metrics = map[string]map[string]report.ClassMetrics{}
	warnings := fanOut(keys, func(name string) string {
		p := pods[name]
		path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/metrics", p.Namespace, p.Name, metricsPort(p))
		body, err := c.rawWithin(ctx, path)
		if apierrors.IsForbidden(err) {
			forbidden.Add(1)
			return ""
		}
		if err != nil {
			return fmt.Sprintf("metrics from %s unavailable: %v", name, err)
		}
		m, err := ParseMetrics(strings.NewReader(string(body)))
		if err != nil {
			return fmt.Sprintf("metrics from %s unavailable: %v", name, err)
		}
		mu.Lock()
		defer mu.Unlock()
		for node, classes := range m {
			if in.Metrics[node] == nil {
				in.Metrics[node] = map[string]report.ClassMetrics{}
			}
			maps.Copy(in.Metrics[node], classes)
		}
		return ""
	})
	if len(keys) > 0 {
		in.Warnings = append(in.Warnings, refused("pods/proxy in "+pods[keys[0]].Namespace, int(forbidden.Load()), len(keys),
			"metrics", "topolvm-node pods", "metrics")...)
	}
	in.Warnings = append(in.Warnings, warnings...)
}

// collectUsage reads kubelet volume stats of every node that holds a volume.
func (c *Clients) collectUsage(ctx context.Context, in *report.Input) {
	seen := map[string]bool{}
	var keys []string
	for i := range in.LVs {
		n := in.LVs[i].NodeName
		if n != "" && !seen[n] && !in.AbsentNodes[n] {
			seen[n] = true
			keys = append(keys, n)
		}
	}
	sort.Strings(keys)
	var (
		mu        sync.Mutex
		forbidden atomic.Int32
	)
	in.Usage = map[string]report.VolumeUsage{}
	warnings := fanOut(keys, func(node string) string {
		body, err := c.rawWithin(ctx, "/api/v1/nodes/"+node+"/proxy/stats/summary")
		if apierrors.IsForbidden(err) {
			forbidden.Add(1)
			return ""
		}
		if err != nil {
			return fmt.Sprintf("usage from %s unavailable: %v", node, err)
		}
		u, err := ParseSummary(strings.NewReader(string(body)))
		if err != nil {
			return fmt.Sprintf("usage from %s unavailable: %v", node, err)
		}
		mu.Lock()
		defer mu.Unlock()
		maps.Copy(in.Usage, u)
		return ""
	})
	in.Warnings = append(in.Warnings, refused("nodes/proxy", int(forbidden.Load()), len(keys), "usage", "nodes", "usage")...)
	in.Warnings = append(in.Warnings, warnings...)
}
