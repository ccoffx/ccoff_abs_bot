package game

import (
	"ccoff-abs-bot/internal/textfmt"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func saveListeningReportStats(userID int64, absID string, total float64, fetchedAt time.Time) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCurrentListeningBindingInTx(tx, userID, absID); err != nil {
			return err
		}
		return saveListeningReportStatsInTx(tx, userID, absID, total, fetchedAt)
	})
}

// The caller holds the current-binding write guard. Sharing its transaction
// with daily rows prevents a failed metadata write leaving a partial snapshot.
func saveListeningReportStatsInTx(tx *gorm.DB, userID int64, absID string, total float64, fetchedAt time.Time) error {
	var previous model.ListeningReportSnapshot
	err := tx.First(&previous, "user_id = ?", userID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	// Compare instants in Go; SQLite strings can contain different UTC offsets.
	if err == nil && previous.AbsUserID == absID && previous.StatsFetchedAt.After(fetchedAt) && !previous.StatsFetchedAt.After(time.Now()) {
		return nil
	}
	if previous.AbsUserID != absID {
		previous = model.ListeningReportSnapshot{UserID: userID}
	}
	previous.AbsUserID, previous.RawTotalSeconds, previous.StatsFetchedAt = absID, total, fetchedAt
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, UpdateAll: true}).Create(&previous).Error
}

// Book progress is only needed by reports, so fetch it in the background, once
// per three minutes, without adding requests to manual daily-listening refreshes.
func refreshListeningReportProgress(userID int64, absID string, now time.Time) {
	coalesceListeningFetch(fmt.Sprintf("progress:%d:%q", userID, absID), func() listeningFetchResult {
		refreshListeningReportProgressOnce(userID, absID, now)
		return listeningFetchResult{}
	})
}

func refreshListeningReportProgressOnce(userID int64, absID string, now time.Time) {
	var previous model.ListeningReportSnapshot
	if err := storage.DB.Where("user_id = ? AND abs_user_id = ?", userID, absID).First(&previous).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return
	}
	if !previous.ProgressFetchedAt.IsZero() && listeningTimestampFresh(previous.ProgressFetchedAt, now, ListeningSyncInterval) {
		return
	}
	if abs.DefaultClient == nil || !acquireListeningSyncSlot() {
		return
	}
	defer func() { <-listeningSyncSlots }()
	body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserPath(absID), nil)
	if err != nil || code != 200 {
		return
	}
	var data struct {
		MediaProgress *[]struct {
			IsFinished bool `json:"isFinished"`
		} `json:"mediaProgress"`
	}
	if json.Unmarshal(body, &data) != nil || data.MediaProgress == nil {
		return
	}
	finished, listening := 0, 0
	for _, progress := range *data.MediaProgress {
		if progress.IsFinished {
			finished++
		} else {
			listening++
		}
	}
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCurrentListeningBindingInTx(tx, userID, absID); err != nil {
			return err
		}
		var current model.ListeningReportSnapshot
		err := tx.Where("user_id = ?", userID).First(&current).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) || current.AbsUserID != absID {
			current = model.ListeningReportSnapshot{UserID: userID, AbsUserID: absID}
			current.FinishedBooks, current.ListeningBooks, current.ProgressFetchedAt = finished, listening, now
			return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, UpdateAll: true}).Create(&current).Error
		}
		if current.ProgressFetchedAt.After(now) && !current.ProgressFetchedAt.After(time.Now()) {
			return nil
		}
		return tx.Model(&current).Updates(map[string]any{"finished_books": finished, "listening_books": listening, "progress_fetched_at": now}).Error
	})
	if err != nil {
		log.Printf("听书书籍进度快照写入失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
	}
}
