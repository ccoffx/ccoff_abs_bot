package game

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const (
	absCrossDayBackfillPagesPerBatch = 5
	absCrossDayBackfillTickInterval  = 30 * time.Second
	absCrossDayBackfillPageDelay     = 100 * time.Millisecond
	absCrossDayBackfillRetryDelay    = 5 * time.Minute
	absCrossDayBackfillSeedInterval  = 10 * time.Minute
)

var absCrossDayBackfillMu sync.Mutex

func StartABSCrossDaySessionBackfill() {
	go func() {
		if err := seedABSCrossDaySessionScanCursors(); err != nil {
			log.Printf("ABS cross-day history cursor seed failed: %s", textfmt.FormatPlainError(err))
		}
		runABSCrossDaySessionBackfillBatchSafely()
		batchTicker := time.NewTicker(absCrossDayBackfillTickInterval)
		seedTicker := time.NewTicker(absCrossDayBackfillSeedInterval)
		defer batchTicker.Stop()
		defer seedTicker.Stop()
		for {
			select {
			case <-batchTicker.C:
				runABSCrossDaySessionBackfillBatchSafely()
			case <-seedTicker.C:
				if err := seedABSCrossDaySessionScanCursors(); err != nil {
					log.Printf("ABS cross-day history periodic cursor seed failed: %s", textfmt.FormatPlainError(err))
				}
			}
		}
	}()
	log.Println("✅ ABS 跨日会话历史渐进扫描器已启动：每批最多 5 页，持久化游标续扫至历史末尾。")
}

func runABSCrossDaySessionBackfillBatchSafely() {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("ABS cross-day history backfill panic recovered: %s", textfmt.FormatPlainValue(recovered))
		}
	}()
	if !absCrossDayBackfillMu.TryLock() {
		return
	}
	defer absCrossDayBackfillMu.Unlock()
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		log.Printf("ABS cross-day history backfill batch failed: %s", textfmt.FormatPlainError(err))
	}
}

func seedABSCrossDaySessionScanCursors() error {
	if storage.DB == nil {
		return fmt.Errorf("ABS_CROSS_DAY_CURSOR_DB_EMPTY")
	}
	var users []model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("abs_user_id <> ?", "").Find(&users).Error; err != nil {
		return err
	}
	rows := make([]model.ABSCrossDaySessionScanCursor, 0, len(users))
	for _, user := range users {
		absUserID := strings.TrimSpace(user.AbsUserID)
		if user.TelegramID == 0 || absUserID == "" {
			continue
		}
		rows = append(rows, model.ABSCrossDaySessionScanCursor{UserID: user.TelegramID, AbsUserID: absUserID})
	}
	if len(rows) == 0 {
		return nil
	}
	return storage.DB.Clauses(absCrossDaySessionCursorOnConflict(time.Now())).CreateInBatches(&rows, 100).Error
}

// ensureABSCrossDaySessionScanCursor immediately enrolls a newly registered or
// rebound ABS identity. If ownership changed, the historical cursor is reset so
// persisted observations are reassigned to the current Telegram user by the
// normal idempotent observation upsert.
func EnsureABSCrossDaySessionScanCursor(userID int64, absUserID string) error {
	if storage.DB == nil {
		return fmt.Errorf("ABS_CROSS_DAY_CURSOR_DB_EMPTY")
	}
	absUserID = strings.TrimSpace(absUserID)
	if userID == 0 || absUserID == "" {
		return fmt.Errorf("ABS_CROSS_DAY_CURSOR_ARGUMENT_INVALID")
	}
	row := model.ABSCrossDaySessionScanCursor{UserID: userID, AbsUserID: absUserID}
	return storage.DB.Clauses(absCrossDaySessionCursorOnConflict(time.Now())).Create(&row).Error
}

func absCrossDaySessionCursorOnConflict(now time.Time) clause.OnConflict {
	if now.IsZero() {
		now = time.Now()
	}
	changed := "(abs_cross_day_session_scan_cursors.user_id <> excluded.user_id OR abs_cross_day_session_scan_cursors.last_error_code = 'binding_no_longer_current')"
	return clause.OnConflict{
		Columns: []clause.Column{{Name: "abs_user_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"user_id":           gorm.Expr("excluded.user_id"),
			"next_page":         gorm.Expr("CASE WHEN " + changed + " THEN 0 ELSE abs_cross_day_session_scan_cursors.next_page END"),
			"pages_scanned":     gorm.Expr("CASE WHEN " + changed + " THEN 0 ELSE abs_cross_day_session_scan_cursors.pages_scanned END"),
			"sessions_observed": gorm.Expr("CASE WHEN " + changed + " THEN 0 ELSE abs_cross_day_session_scan_cursors.sessions_observed END"),
			"completed":         gorm.Expr("CASE WHEN " + changed + " THEN false ELSE abs_cross_day_session_scan_cursors.completed END"),
			"last_scanned_at":   gorm.Expr("CASE WHEN " + changed + " THEN NULL ELSE abs_cross_day_session_scan_cursors.last_scanned_at END"),
			"completed_at":      gorm.Expr("CASE WHEN " + changed + " THEN NULL ELSE abs_cross_day_session_scan_cursors.completed_at END"),
			"next_retry_at":     gorm.Expr("CASE WHEN " + changed + " THEN NULL ELSE abs_cross_day_session_scan_cursors.next_retry_at END"),
			"last_error_code":   gorm.Expr("CASE WHEN " + changed + " THEN '' ELSE abs_cross_day_session_scan_cursors.last_error_code END"),
			"updated_at":        now,
		}),
	}
}

func advanceABSCrossDaySessionBackfill(now time.Time) error {
	if storage.DB == nil || abs.DefaultClient == nil {
		return fmt.Errorf("ABS_CROSS_DAY_BACKFILL_NOT_READY")
	}
	if now.IsZero() {
		now = time.Now()
	}
	var cursor model.ABSCrossDaySessionScanCursor
	err := storage.DB.Where("completed = ? AND (next_retry_at IS NULL OR next_retry_at <= ?)", false, now).
		Order("COALESCE(last_scanned_at, created_at) ASC, id ASC").First(&cursor).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}

	var currentBindingCount int64
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ? AND abs_user_id = ?", cursor.UserID, cursor.AbsUserID).Count(&currentBindingCount).Error; err != nil {
		return err
	}
	if currentBindingCount != 1 {
		res := storage.DB.Model(&model.ABSCrossDaySessionScanCursor{}).Where("id = ? AND completed = ?", cursor.ID, false).Updates(map[string]interface{}{
			"completed":       true,
			"completed_at":    now,
			"next_retry_at":   nil,
			"last_error_code": "binding_no_longer_current",
		})
		if res.Error != nil {
			return res.Error
		}
		return nil
	}

	for batchPage := 0; batchPage < absCrossDayBackfillPagesPerBatch; batchPage++ {
		page := cursor.NextPage
		sessions, rawCount, fetchErr := fetchABSCrossDayListeningSessionPage(cursor.AbsUserID, page)
		if fetchErr != nil {
			retryAt := now.Add(absCrossDayBackfillRetryDelay)
			updateErr := storage.DB.Model(&model.ABSCrossDaySessionScanCursor{}).Where("id = ?", cursor.ID).Updates(map[string]interface{}{
				"next_retry_at":   retryAt,
				"last_error_code": textfmt.TruncateRunes(textfmt.FormatPlainError(fetchErr), 120),
			}).Error
			if updateErr != nil {
				return fmt.Errorf("fetch=%v cursor_update=%w", fetchErr, updateErr)
			}
			return fetchErr
		}

		observedAt := time.Now()
		observations, diagnostics := buildABSCrossDaySessionObservations(cursor.UserID, cursor.AbsUserID, sessions, observedAt)
		complete := rawCount < dailyListeningSessionPageSize
		err = storage.DB.Transaction(func(tx *gorm.DB) error {
			if err := upsertABSCrossDaySessionObservations(tx, observations); err != nil {
				return err
			}
			if err := recordSectRetreatListeningSessionsInTx(tx, cursor.UserID, cursor.AbsUserID, sessions); err != nil {
				return err
			}
			updates := map[string]interface{}{
				"next_page":         page + 1,
				"pages_scanned":     gorm.Expr("pages_scanned + 1"),
				"sessions_observed": gorm.Expr("sessions_observed + ?", len(observations)),
				"completed":         complete,
				"last_scanned_at":   observedAt,
				"next_retry_at":     nil,
				"last_error_code":   "",
			}
			if complete {
				updates["completed_at"] = observedAt
			}
			res := tx.Model(&model.ABSCrossDaySessionScanCursor{}).
				Where(
					"id = ? AND user_id = ? AND abs_user_id = ? AND next_page = ? AND completed = ?",
					cursor.ID, cursor.UserID, cursor.AbsUserID, page, false,
				).
				Updates(updates)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("ABS_CROSS_DAY_CURSOR_ADVANCE_CONFLICT")
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(diagnostics) > 0 {
			log.Printf("ABS cross-day history page persisted: user=%d abs=%s page=%d raw=%d tracked=%d", cursor.UserID, textfmt.FormatPlainValue(cursor.AbsUserID), page, rawCount, len(observations))
		}
		cursor.NextPage = page + 1
		if complete {
			log.Printf("ABS cross-day history scan completed: user=%d abs=%s pages=%d", cursor.UserID, textfmt.FormatPlainValue(cursor.AbsUserID), cursor.NextPage)
			return nil
		}
		if batchPage+1 < absCrossDayBackfillPagesPerBatch {
			time.Sleep(absCrossDayBackfillPageDelay)
		}
	}
	return nil
}

func fetchABSCrossDayListeningSessionPage(absUserID string, page int) ([]absListeningSession, int, error) {
	if abs.DefaultClient == nil || strings.TrimSpace(absUserID) == "" || page < 0 {
		return nil, 0, fmt.Errorf("ABS_CROSS_DAY_PAGE_ARGUMENT_INVALID")
	}
	body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningSessionsPath(absUserID, page, dailyListeningSessionPageSize), nil)
	if err != nil {
		return nil, 0, err
	}
	if code != 200 {
		return nil, 0, &abs.AbsAPIError{Operation: "读取 ABS 用户历史会话失败", StatusCode: code, Message: "响应: " + abs.AbsResponseSnippet(body)}
	}
	rawSessions, err := parseABSSessionsPayload(body)
	if err != nil {
		return nil, 0, err
	}
	sessions := make([]absListeningSession, 0, len(rawSessions))
	seen := make(map[string]struct{}, len(rawSessions))
	for _, raw := range rawSessions {
		if !absSessionBelongsToUser(raw, absUserID) {
			continue
		}
		session, ok := parseABSListeningSession(raw, absUserID)
		if !ok {
			continue
		}
		key := strings.TrimSpace(session.SessionKey)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		sessions = append(sessions, session)
	}
	return sessions, len(rawSessions), nil
}
