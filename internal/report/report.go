package report

import (
	"math"
	"time"
)

// SchemaVersion is the version of the JSON and YAML output. Fields are only ever added to it.
const SchemaVersion = "v1"

// DefaultPlaceholder names the bucket for volumes without deviceClass on a node whose default
// device class cannot be told.
const DefaultPlaceholder = "<default>"

// Report is the computed view of a cluster.
type Report struct {
	SchemaVersion string    `json:"schemaVersion"`
	Driver        string    `json:"driver"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Pools         []Pool    `json:"pools"`
	Totals        []Total   `json:"totals"`
	Volumes       []Volume  `json:"volumes"`
	Findings      []Finding `json:"findings"`
	Breaches      []Breach  `json:"breaches"`
	Warnings      []string  `json:"warnings"`
}

// Pool is one device class on one node.
type Pool struct {
	Node        string   `json:"node"`
	DeviceClass string   `json:"deviceClass"`
	Default     bool     `json:"default"`
	Type        string   `json:"type"`
	VolumeGroup *string  `json:"volumeGroup"`
	SpareBytes  *int64   `json:"spareBytes"`
	SizeBytes   *int64   `json:"sizeBytes"`
	UsedBytes   int64    `json:"usedBytes"`
	FreeBytes   *int64   `json:"freeBytes"`
	UsedPercent *float64 `json:"usedPercent"`
	Volumes     int      `json:"volumes"`

	// Filled only with --metrics.
	VGSizeBytes         *int64   `json:"volumeGroupSizeBytes"`
	OtherBytes          *int64   `json:"otherBytes"`
	ThinDataPercent     *float64 `json:"thinDataPercent"`
	ThinMetadataPercent *float64 `json:"thinMetadataPercent"`
}

// Total sums one device class over the nodes that have it.
type Total struct {
	DeviceClass  string   `json:"deviceClass"`
	Default      bool     `json:"default"`
	Type         string   `json:"type"`
	Nodes        int      `json:"nodes"`
	SizeBytes    *int64   `json:"sizeBytes"`
	UsedBytes    int64    `json:"usedBytes"`
	FreeBytes    *int64   `json:"freeBytes"`
	MaxFreeBytes *int64   `json:"maxFreeBytes"`
	MaxFreeNode  string   `json:"maxFreeNode"`
	UsedPercent  *float64 `json:"usedPercent"`
	Volumes      int      `json:"volumes"`
}

// Build computes the report.
func Build(in Input, o Options) Report {
	r := Report{
		SchemaVersion: SchemaVersion,
		Driver:        in.Driver.Name,
		GeneratedAt:   in.Now,
		Warnings:      append([]string{}, in.Warnings...),
	}
	pools, warnings := buildPools(in, o)
	r.Pools = pools
	r.Totals = buildTotals(pools)
	r.Volumes = buildVolumes(in, o)
	r.Findings = []Finding{}
	r.Warnings = append(r.Warnings, warnings...)
	if o.Checks {
		findings, checkWarnings := buildFindings(in, o)
		r.Findings = findings
		r.Warnings = append(r.Warnings, checkWarnings...)
	}
	return r
}

func percent(used, size int64) float64 {
	return math.Round(float64(used)/float64(size)*1000) / 10
}
