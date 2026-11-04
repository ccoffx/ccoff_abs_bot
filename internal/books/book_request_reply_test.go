package books

import (
	"path/filepath"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestBookReplyRejectsStaleRequestSnapshot(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "reply.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := db.AutoMigrate(&model.BookRequest{}, &model.BookRequestLog{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	request := model.BookRequest{UserID: 1, AssigneeID: 2, Status: BookRequestStatusNeedInfo, UserNote: "原说明", LastActionAt: now.Add(-time.Minute)}
	if err := db.Create(&request).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.BookRequest{}).Where("id = ?", request.ID).Update("last_action_at", now).Error; err != nil {
		t.Fatal(err)
	}
	updated, err := MarkBookRequestUserReplied(db, request, "用户", "过期回复", now)
	if err != nil || updated {
		t.Fatalf("stale reply updated a refreshed request: %v %v", updated, err)
	}
	if err := db.First(&request, request.ID).Error; err != nil {
		t.Fatal(err)
	}
	if request.Status != BookRequestStatusNeedInfo || request.UserNote != "原说明" {
		t.Fatalf("stale reply changed the request: %+v", request)
	}
	updated, err = MarkBookRequestUserReplied(db, request, "用户", "有效回复", now.Add(time.Second))
	if err != nil || !updated {
		t.Fatalf("fresh reply was rejected: %v %v", updated, err)
	}
	var logs int64
	if err := db.Model(&model.BookRequestLog{}).Count(&logs).Error; err != nil || logs != 1 {
		t.Fatalf("want only the accepted reply's log, got %d: %v", logs, err)
	}
}
