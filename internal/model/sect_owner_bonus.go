package model

// SectOwnerBonusState marks the one-time rollout; restarting must not move its date.
type SectOwnerBonusState struct {
	ID        uint `gorm:"primaryKey"`
	LaunchDay string
}

// SectOwnerBonusRevision records the next day's entitlement independently of
// current membership, so transfers and late listening corrections preserve history.
type SectOwnerBonusRevision struct {
	ID      uint   `gorm:"primaryKey"`
	UserID  int64  `gorm:"uniqueIndex:idx_owner_bonus_user_day;not null"`
	DayKey  string `gorm:"uniqueIndex:idx_owner_bonus_user_day;not null"`
	Enabled bool
}
