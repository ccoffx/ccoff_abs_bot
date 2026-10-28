package model

import (
	"time"

	"gorm.io/gorm"
)

type OpenRegistrationCampaign struct {
	gorm.Model
	CampaignID string `gorm:"uniqueIndex;not null"`
	Mode       string `gorm:"index;not null"`
	Quota      int
	EndsAt     *time.Time `gorm:"index"`
	Status     string     `gorm:"index;not null"`
	CreatedBy  int64      `gorm:"index;not null"`
	Reason     string
	ClosedAt   *time.Time
}

func (OpenRegistrationCampaign) TableName() string { return "open_registration_campaigns" }

type OpenRegistrationReservation struct {
	gorm.Model
	CampaignID  string `gorm:"uniqueIndex:idx_open_registration_campaign_user;index;not null"`
	UserID      int64  `gorm:"uniqueIndex:idx_open_registration_campaign_user;index;not null"`
	Status      string `gorm:"index;not null"`
	ReservedAt  time.Time
	CompletedAt *time.Time
	ReleasedAt  *time.Time
}

func (OpenRegistrationReservation) TableName() string {
	return "open_registration_reservations"
}

type CompensationCampaign struct {
	gorm.Model
	CampaignID     string `gorm:"uniqueIndex;not null"`
	CreatedBy      int64  `gorm:"index;not null"`
	Points         int
	Days           int
	Announcement   string
	Status         string `gorm:"index;not null"`
	RecipientCount int
	AppliedCount   int
	SuccessCount   int
	FailedCount    int
	CreatedAtRun   time.Time `gorm:"index;not null"`
	CompletedAt    *time.Time
	LastError      string
}

func (CompensationCampaign) TableName() string { return "compensation_campaigns" }

type CompensationGrant struct {
	gorm.Model
	CampaignID           string `gorm:"uniqueIndex:idx_compensation_campaign_user;index;not null"`
	UserID               int64  `gorm:"uniqueIndex:idx_compensation_campaign_user;index;not null"`
	UserName             string
	Points               int
	Days                 int
	Status               string `gorm:"index;not null"`
	BalanceBefore        int
	BalanceAfter         int
	ExpireBefore         *time.Time
	ExpireAfter          *time.Time
	DaysApplied          int
	AppliedAt            *time.Time
	SentAt               *time.Time
	DeliveryError        string
	ReactivationRequired bool
	ReactivationOK       bool
	ReactivationError    string
	AttemptCount         int
	LastAttemptAt        *time.Time
	LastError            string
}

func (CompensationGrant) TableName() string { return "compensation_grants" }
