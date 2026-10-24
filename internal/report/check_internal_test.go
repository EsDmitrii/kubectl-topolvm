package report

import (
	"testing"
	"time"
)

func TestParseResizeRequestedAt(t *testing.T) {
	got, err := parseGoTime("2026-10-24 09:00:00.123456789 +0000 UTC m=+12.345")
	if err != nil {
		t.Fatal(err)
	}
	if got.UTC().Format(time.RFC3339Nano) != "2026-10-24T09:00:00.123456789Z" {
		t.Errorf("parsed = %v", got)
	}
	if _, err := parseGoTime("garbage"); err == nil {
		t.Error("want an error for garbage")
	}
}

func TestCodeName(t *testing.T) {
	for code, want := range map[int64]string{0: "OK", 8: "ResourceExhausted", 13: "Internal", 16: "Unauthenticated", 42: "code 42"} {
		if got := codeName(code); got != want {
			t.Errorf("codeName(%d) = %q, want %q", code, got, want)
		}
	}
}
