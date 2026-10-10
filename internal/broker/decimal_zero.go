package broker

import "regexp"

// Check decimal evidence without floating-point conversion: ParseFloat can
// silently underflow a nonzero price or filled quantity (1e-400) to zero.
var decimalZeroPattern = regexp.MustCompile(`^[+-]?(?:0+(?:\.0*)?|\.0+)(?:[eE][+-]?[0-9]+)?$`)

func explicitDecimalZero(value string) bool {
	return decimalZeroPattern.MatchString(value)
}
