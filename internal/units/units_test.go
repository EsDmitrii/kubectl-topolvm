package units

import "testing"

func TestFormatIEC(t *testing.T) {
	cases := map[int64]string{
		0: "0", 512: "512", 1024: "1Ki", 1536: "1.5Ki", 10737418240: "10Gi", 121915834368: "113.5Gi",
		338811682816: "315.5Gi", 1099511627775: "1Ti", 1099511627776: "1Ti", 1288490188800: "1.2Ti",
	}
	for in, want := range cases {
		if got := FormatIEC(in); got != want {
			t.Errorf("FormatIEC(%d) = %q, want %q", in, got, want)
		}
	}
}
