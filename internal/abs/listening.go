package abs

import (
	"encoding/json"
	"fmt"
	"math"
)

// ParseListeningDay requires the scoped response to distinguish zero playback
// from missing data. It ignores any values outside the requested day.
func ParseListeningDay(body []byte, day string) (float64, error) {
	var response struct {
		Days map[string]json.RawMessage `json:"days"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, fmt.Errorf("ABS_DAILY_INVALID_RESPONSE")
	}
	value, present := response.Days[day]
	if !present {
		return 0, fmt.Errorf("ABS_DAILY_MISSING_DAY: %s", day)
	}
	var seconds *float64
	if err := json.Unmarshal(value, &seconds); err != nil || seconds == nil || *seconds < 0 || math.IsNaN(*seconds) || math.IsInf(*seconds, 0) {
		return 0, fmt.Errorf("ABS_DAILY_INVALID_VALUE: %s", day)
	}
	return *seconds, nil
}
