package books

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupBookAnnouncementStateDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "announcement.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = previous; _ = conn.Close() })
	if err := db.AutoMigrate(&model.BookRequestAnnouncementDelivery{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestBookAnnouncementPublishedUsesDeliveryReceipt(t *testing.T) {
	db := setupBookAnnouncementStateDB(t)
	// No historical business log lookup is needed when there is no receipt.
	if published, err := bookRequestGroupAnnouncementAlreadyPublished(1); err != nil || published {
		t.Fatalf("missing receipt: published=%t error=%v", published, err)
	}
	delivery := model.BookRequestAnnouncementDelivery{RequestID: 1, Status: bookAnnouncementDeliveryUncertain}
	if err := db.Create(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if published, err := bookRequestGroupAnnouncementAlreadyPublished(1); err != nil || published {
		t.Fatalf("uncertain receipt: published=%t error=%v", published, err)
	}
	if err := db.Model(&delivery).Update("status", bookAnnouncementDeliverySent).Error; err != nil {
		t.Fatal(err)
	}
	if published, err := bookRequestGroupAnnouncementAlreadyPublished(1); err != nil || !published {
		t.Fatalf("sent receipt: published=%t error=%v", published, err)
	}
}

func TestExpiredBookAnnouncementClaimPersistsUncertainState(t *testing.T) {
	db := setupBookAnnouncementStateDB(t)
	now := time.Now()
	expired := now.Add(-time.Minute)
	delivery := model.BookRequestAnnouncementDelivery{RequestID: 1, ItemID: "book", TargetChatID: -100, Status: bookAnnouncementDeliverySending, LeaseUntil: &expired, AttemptCount: 2}
	if err := db.Create(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := claimBookRequestAnnouncementDelivery(1, "book", -100, 101, now); !errors.Is(err, errBookAnnouncementUncertain) {
		t.Fatalf("expired send was not refused as uncertain: %v", err)
	}
	var persisted model.BookRequestAnnouncementDelivery
	if err := db.First(&persisted, delivery.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Status != bookAnnouncementDeliveryUncertain || persisted.LeaseUntil != nil || persisted.AttemptCount != 2 || persisted.LastErrorCode != "sending_lease_expired" {
		t.Fatalf("expired claim recovery was rolled back: %+v", persisted)
	}
}
