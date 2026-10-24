// Package units formats byte counts the way the plugin prints them everywhere.
package units

import (
	"math"
	"strconv"
	"strings"
)

var iecUnits = []string{"Ki", "Mi", "Gi", "Ti", "Pi", "Ei"}

// FormatIEC prints bytes as a Kubernetes quantity with one decimal: 315.5Gi, 10Gi, 512. A value
// that rounds up to 1024 of a unit moves to the next unit, so 1Ti - 1 prints as 1Ti.
func FormatIEC(b int64) string {
	if b < 1024 && b > -1024 {
		return strconv.FormatInt(b, 10)
	}
	v := math.Abs(float64(b))
	i := -1
	for v >= 1024 && i < len(iecUnits)-1 {
		v /= 1024
		i++
	}
	r := math.Round(v*10) / 10
	if r >= 1024 && i < len(iecUnits)-1 {
		r = math.Round(v/1024*10) / 10
		i++
	}
	s := strings.TrimSuffix(strconv.FormatFloat(r, 'f', 1, 64), ".0")
	if b < 0 {
		s = "-" + s
	}
	return s + iecUnits[i]
}
