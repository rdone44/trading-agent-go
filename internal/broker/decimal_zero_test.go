package broker

import "testing"

func TestExplicitDecimalZero(t *testing.T) {
	for _, value := range []string{"0", "0.00000", "-0", "+0.0", ".0", "0.", "00.00", "0e-400", "-0E+999999"} {
		if !explicitDecimalZero(value) {
			t.Errorf("explicit zero %q rejected", value)
		}
	}
	for _, value := range []string{"", " ", " 0", "0\n", "1e-400", "-1e-400", "0.00000000000000000001", "NaN", "Inf", "null", "0x0p0", "0_0", ".", "+", "0e", "0e+", "0e1.0"} {
		if explicitDecimalZero(value) {
			t.Errorf("nonzero or malformed evidence %q accepted", value)
		}
	}
}
