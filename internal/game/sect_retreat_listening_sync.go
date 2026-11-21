package game

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

var sectRetreatListeningSyncMu sync.Mutex

// RunSectRetreatListeningSyncIfNeeded collects proof independently of ordinary
// daily totals, abuse scans and the one-time historical backfill. Each retreat
// gets at most five pages per five minutes, with progress committed per page.
func RunSectRetreatListeningSyncIfNeeded(now time.Time) error {
	if !sectRetreatListeningSyncMu.TryLock() {
		return nil
	}
	defer sectRetreatListeningSyncMu.Unlock()
	if storage.DB == nil || abs.DefaultClient == nil {
		return fmt.Errorf("SECT_RETREAT_LISTENING_NOT_READY")
	}
	var due []struct {
		model.SectCaveRetreat
		CurrentAbsUserID string
	}
	// Repeat full passes during the retreat and for 24 hours afterward to pick
	// up delayed uploads. Older retreats with no completed pass are also repaired.
	if err := storage.DB.Model(&model.SectCaveRetreat{}).
		Select("sect_cave_retreats.*, users.abs_user_id AS current_abs_user_id").
		Joins("JOIN users ON users.telegram_id = sect_cave_retreats.user_id AND users.deleted_at IS NULL").
		Where("TRIM(users.abs_user_id) <> '' AND start_at <= ?", now).
		Where("listening_scan_attempt_at IS NULL OR listening_scan_attempt_at <= ? OR listening_scan_attempt_at > ?", now.Add(-ListeningSyncInterval), now).
		Where("listening_scan_completed_at IS NULL OR listening_scan_started_at IS NULL OR julianday(listening_scan_started_at) < julianday(end_at) + 1 OR listening_scan_abs_user_id <> users.abs_user_id").
		Order("COALESCE(listening_scan_attempt_at, sect_cave_retreats.created_at), sect_cave_retreats.id").
		Limit(20).Find(&due).Error; err != nil {
		return err
	}
	var failures []error
	for _, entry := range due {
		if err := syncSectRetreatListening(entry.SectCaveRetreat, entry.CurrentAbsUserID, now); err != nil {
			failures = append(failures, fmt.Errorf("retreat=%d user=%d: %w", entry.ID, entry.UserID, err))
		}
	}
	return errors.Join(failures...)
}

func syncSectRetreatListening(retreat model.SectCaveRetreat, absID string, now time.Time) error {
	// Evidence changes settled listening totals too, so it must respect the
	// full-refresh barrier through title publication, just like daily inputs.
	listeningRefreshMu.RLock()
	defer listeningRefreshMu.RUnlock()
	// Reserve the attempt separately from proof and settlement. A rolled-back
	// page must still back off and yield its batch slot to other retreats.
	attempt := storage.DB.Model(&model.SectCaveRetreat{}).
		Where("id = ? AND user_id = ?", retreat.ID, retreat.UserID).
		UpdateColumn("listening_scan_attempt_at", now)
	if attempt.Error != nil {
		return attempt.Error
	}
	if attempt.RowsAffected != 1 {
		return fmt.Errorf("SECT_RETREAT_LISTENING_WRITE_MISSED")
	}
	page := retreat.ListeningScanPage
	startedAt := retreat.ListeningScanStartedAt
	if retreat.ListeningScanAbsUserID != absID || retreat.ListeningScanCompletedAt != nil || startedAt == nil {
		page = 0
		startedAt = &now
	}
	for fetched := 0; fetched < dailyListeningSessionMaxPages; fetched++ {
		sessions, count, fetchErr := fetchABSCrossDayListeningSessionPage(absID, page)
		if fetchErr != nil {
			return fetchErr
		}
		complete := count < dailyListeningSessionPageSize
		err := storage.DB.Transaction(func(tx *gorm.DB) error {
			// Follow the source/settlement lock order. Proof, settlement and page
			// advancement must all roll back if the binding or a write changes.
			if err := spiritfire.Lock(tx); err != nil {
				return err
			}
			if err := lockCurrentListeningBindingInTx(tx, retreat.UserID, absID); err != nil {
				return err
			}
			if err := recordSectRetreatListeningSessionsInTx(tx, retreat.UserID, absID, sessions); err != nil {
				return err
			}
			updates := map[string]any{
				"listening_scan_abs_user_id":  absID,
				"listening_scan_page":         page + 1,
				"listening_scan_started_at":   *startedAt,
				"listening_scan_completed_at": nil,
			}
			if complete {
				updates["listening_scan_completed_at"] = now
			}
			res := tx.Model(&model.SectCaveRetreat{}).Where("id = ? AND user_id = ?", retreat.ID, retreat.UserID).UpdateColumns(updates)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("SECT_RETREAT_LISTENING_WRITE_MISSED")
			}
			_, err := settleCultivationListeningInTx(tx, retreat.UserID, now)
			return err
		})
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		page++
	}
	return nil
}
