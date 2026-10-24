// Package output renders reports as kubectl-style tables and as versioned JSON or YAML.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// Section selects which part of a report a command prints.
type Section string

// Sections of the structured output.
const (
	SectionPools   Section = "pools"
	SectionVolumes Section = "volumes"
	SectionCheck   Section = "check"
	SectionFull    Section = "full"
)

type header struct {
	SchemaVersion string    `json:"schemaVersion"`
	Driver        string    `json:"driver"`
	GeneratedAt   time.Time `json:"generatedAt"`
}

type poolsView struct {
	header
	Pools    []report.Pool   `json:"pools"`
	Totals   []report.Total  `json:"totals"`
	Breaches []report.Breach `json:"breaches"`
	Warnings []string        `json:"warnings"`
}

type volumesView struct {
	header
	Volumes  []report.Volume `json:"volumes"`
	Warnings []string        `json:"warnings"`
}

type checkView struct {
	header
	Findings []report.Finding `json:"findings"`
	Warnings []string         `json:"warnings"`
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// View returns the JSON-ready document for one section. Arrays are never null.
func View(r report.Report, s Section) (any, error) {
	h := header{SchemaVersion: r.SchemaVersion, Driver: r.Driver, GeneratedAt: r.GeneratedAt}
	warnings := nonNil(r.Warnings)
	switch s {
	case SectionPools:
		return poolsView{h, nonNil(r.Pools), nonNil(r.Totals), nonNil(r.Breaches), warnings}, nil
	case SectionVolumes:
		return volumesView{h, nonNil(r.Volumes), warnings}, nil
	case SectionCheck:
		return checkView{h, nonNil(r.Findings), warnings}, nil
	case SectionFull:
		r.Pools, r.Totals, r.Volumes, r.Findings, r.Breaches, r.Warnings = nonNil(r.Pools), nonNil(r.Totals), nonNil(r.Volumes), nonNil(r.Findings), nonNil(r.Breaches), warnings
		return r, nil
	default:
		return nil, fmt.Errorf("unknown section %q", s)
	}
}

// Wrap puts a command-specific body under name, next to the common header and warnings.
func Wrap(r report.Report, name string, body any) map[string]any {
	return map[string]any{
		"schemaVersion": r.SchemaVersion,
		"driver":        r.Driver,
		"generatedAt":   r.GeneratedAt,
		name:            body,
		"warnings":      nonNil(r.Warnings),
	}
}

// Structured writes one section as indented JSON or as YAML.
func Structured(w io.Writer, r report.Report, s Section, format string) error {
	v, err := View(r, s)
	if err != nil {
		return err
	}
	return Encode(w, v, format)
}

// Encode writes any value as indented JSON or as YAML.
func Encode(w io.Writer, v any, format string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	case "yaml":
		out, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(out)
		return err
	default:
		return fmt.Errorf("unknown structured format %q", format)
	}
}
