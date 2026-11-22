package model

import "time"

// Embedded in activity participants. New entries use day totals from this day
// onward, so unrelated historical corrections cannot change activity progress.
type ActivityListeningSnapshot struct {
	BaselineDay     string
	BaselineSeconds float64
	// Already published progress retained when an old cumulative baseline is
	// replaced with a daily baseline. It is never read from ABS totalTime.
	CarriedSeconds      float64
	DailyInputJSON      string
	Seconds             float64
	FetchedAt           *time.Time
	Finalized           bool `gorm:"not null;default:false"`
	FinalAttempts       int
	FinalAttemptedAt    *time.Time
	FinalNextRetryAt    *time.Time
	FinalLastError      string
	FinalFallbackReason string
}
