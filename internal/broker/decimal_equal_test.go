package broker

import (
	"strings"
	"testing"
)

func TestEqualPositiveDecimal(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"90", "90.0000", true},
		{"00090.0", "90", true},
		{"0.000000000000000001", "0.0000000000000000010", true},
		{"90", "90.000000000000001", false},
		{"110", "110.000000000000001", false},
		{"90", "0x1.68p+6", false},
		{"90", "180/2", false},
		{"90", "9e1", false},
		{"0", "0.0", false},
		{"-90", "-90", false},
		{"NaN", "NaN", false},
		{"90", " 90", false},
		{"90", "90\n", false},
		{"", "", false},
		{"90", "90." + strings.Repeat("0", 1024), false},
	} {
		for _, pair := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			if got := equalPositiveDecimal(pair[0], pair[1]); got != tc.want {
				t.Errorf("equalPositiveDecimal(%q, %q) = %v, want %v", pair[0], pair[1], got, tc.want)
			}
		}
	}
}
