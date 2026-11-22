package game

import (
	"net/http"
	"net/http/httptest"

	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func bossEvidenceFixture(t *testing.T) (model.WorldBossEvent, model.WorldBossParticipant) {
	t.Helper()
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.AutoMigrate(&model.ActivityEntryRequest{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(-time.Second).Truncate(time.Second)
	event := model.WorldBossEvent{BossID: "test-boss", Status: "active", StartAt: end.Add(-time.Hour), EndAt: end}
	p := model.WorldBossParticipant{Model: gorm.Model{CreatedAt: event.StartAt}, BossID: event.BossID, UserID: 1, AbsUserID: "abs1", ListeningSnapshot: newActivityListeningSnapshot(nil, event.StartAt)}
	if err := storage.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return event, p
}

func bossSession(id string, start, end time.Time, seconds float64) map[string]interface{} {
	return map[string]interface{}{"id": id, "userId": "abs1", "startedAt": start.Format(time.RFC3339Nano), "updatedAt": end.Format(time.RFC3339Nano), "timeListening": seconds}
}

func bossEvidenceServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	old := abs.DefaultClient
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
	t.Cleanup(func() { abs.DefaultClient = old; server.Close() })
}
