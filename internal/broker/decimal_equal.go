package broker

import (
	"math/big"
	"regexp"
)

// Exchange trigger prices are decimal strings, not fractions or hexadecimal
// floats. Compare their exact values: float64 can erase a conflicting last
// digit and incorrectly authorize an exit. Bound input size before allocating
// arbitrary-precision integers; unsupported evidence fails closed.
var positiveDecimalPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

func equalPositiveDecimal(a, b string) bool {
	if len(a) > 1024 || len(b) > 1024 || !positiveDecimalPattern.MatchString(a) || !positiveDecimalPattern.MatchString(b) {
		return false
	}
	x, ok := new(big.Rat).SetString(a)
	if !ok || x.Sign() <= 0 {
		return false
	}
	y, ok := new(big.Rat).SetString(b)
	return ok && y.Sign() > 0 && x.Cmp(y) == 0
}
