package output

import (
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
	"github.com/EsDmitrii/kubectl-topolvm/internal/units"
)

const (
	none    = "<none>"
	unknown = "<unknown>"
)

// ANSI colors used for cells that need attention.
const (
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
	reset  = "\x1b[0m"
)

// USE% thresholds for coloring.
const (
	warnPercent = 70
	critPercent = 85
)

// TableOptions tune every table writer.
type TableOptions struct {
	Wide      bool
	NoHeaders bool
	// Bytes prints exact byte counts instead of IEC quantities.
	Bytes bool
	// Color paints cells that need attention.
	Color bool
	// ASCII draws trees with plain characters.
	ASCII bool
	// Usage adds kubelet filesystem columns to volume tables.
	Usage bool
	// Metrics adds topolvm-node metric columns to pool tables.
	Metrics bool
}

func (o TableOptions) size(b int64) string {
	if o.Bytes {
		return strconv.FormatInt(b, 10)
	}
	return units.FormatIEC(b)
}

func (o TableOptions) sizeOr(p *int64, missing string) string {
	if p == nil {
		return missing
	}
	return o.size(*p)
}

// table lays out columns the way text/tabwriter.NewWriter(w, 0, 4, 3, ' ', 0) does for uniform
// rows: left aligned, three spaces of padding, no trailing spaces after the last cell. Widths are
// measured without color, which is applied after padding so colored tables stay aligned.
type table struct {
	rows   [][]string
	colors map[[2]int]string
}

func newTable() *table { return &table{colors: map[[2]int]string{}} }

func (t *table) row(cells ...string) { t.rows = append(t.rows, cells) }

// paint colors the cell at column col of the last row added.
func (t *table) paint(col int, color string) {
	if color != "" {
		t.colors[[2]int{len(t.rows) - 1, col}] = color
	}
}

func (t *table) render(w io.Writer) error {
	var widths []int
	for _, r := range t.rows {
		for i, c := range r {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	var b strings.Builder
	for ri, r := range t.rows {
		for i, c := range r {
			if color, ok := t.colors[[2]int{ri, i}]; ok {
				b.WriteString(color + c + reset)
			} else {
				b.WriteString(c)
			}
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)+3))
			}
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// keyValues prints aligned "KEY:  value" lines like kubectl describe.
func keyValues(w io.Writer, pairs [][2]string) error {
	width := 0
	for _, p := range pairs {
		width = max(width, utf8.RuneCountInString(p[0]))
	}
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(p[0] + ":" + strings.Repeat(" ", width-utf8.RuneCountInString(p[0])+2) + p[1] + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func classLabel(class string, isDefault bool) string {
	switch {
	case class == "":
		return none
	case isDefault:
		return class + " (default)"
	default:
		return class
	}
}

func typeLabel(t string) string {
	if t == topolvm.TypeUnknown || t == "" {
		return "?"
	}
	return t
}

// usePercent recomputes USE% from bytes rather than from the rounded report value, and says which
// color the cell deserves.
func usePercent(typ string, used int64, size *int64, o TableOptions) (text, color string) {
	switch {
	case typ == topolvm.TypeThin:
		return "-", ""
	case size == nil:
		return unknown, ""
	case *size == 0:
		return "-", ""
	}
	pct := math.Round(float64(used) / float64(*size) * 100)
	if o.Color {
		switch {
		case pct >= critPercent:
			color = red
		case pct >= warnPercent:
			color = yellow
		}
	}
	return strconv.FormatFloat(pct, 'f', 0, 64) + "%", color
}

func orNone(p *string) string {
	if p == nil {
		return none
	}
	return *p
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinOr(items []string) string {
	if len(items) == 0 {
		return none
	}
	return strings.Join(items, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func age(now, t time.Time) string {
	return duration.HumanDuration(max(now.Sub(t), 0))
}
