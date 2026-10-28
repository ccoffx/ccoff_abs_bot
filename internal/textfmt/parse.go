package textfmt

import (
	"fmt"
	"math"
	"strconv"
)

func ParseNonNegativeInt(raw string, fieldName string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s必须是非负整数。", fieldName)
	}
	return value, nil
}

func ParseFloatRange(raw string, fieldName string, min float64, max float64) (float64, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < min || value > max {
		return 0, fmt.Errorf("%s必须在 %.0f 到 %.0f 之间，且必须是有限数字。", fieldName, min, max)
	}
	return value, nil
}
