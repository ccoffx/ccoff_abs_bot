package books

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const (
	bookAnnouncementDeliveryPending   = "pending"
	bookAnnouncementDeliverySending   = "sending"
	bookAnnouncementDeliverySent      = "sent"
	bookAnnouncementDeliveryFailed    = "failed"
	bookAnnouncementDeliveryUncertain = "uncertain"
)

func claimBookRequestAnnouncementDelivery(reqID uint, itemID string, targetChatID int64, requestedBy int64, now time.Time) (model.BookRequestAnnouncementDelivery, error) {
	var delivery model.BookRequestAnnouncementDelivery
	if storage.DB == nil {
		return delivery, fmt.Errorf("BOOK_ANNOUNCEMENT_DB_EMPTY")
	}
	itemID = strings.TrimSpace(itemID)
	if reqID == 0 || itemID == "" || targetChatID == 0 || requestedBy == 0 {
		return delivery, fmt.Errorf("BOOK_ANNOUNCEMENT_CLAIM_INVALID")
	}
	if now.IsZero() {
		now = time.Now()
	}
	leaseUntil := now.Add(5 * time.Minute)
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		pending := model.BookRequestAnnouncementDelivery{
			RequestID:    reqID,
			ItemID:       itemID,
			TargetChatID: targetChatID,
			Status:       bookAnnouncementDeliveryPending,
			RequestedBy:  requestedBy,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&pending).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.BookRequestAnnouncementDelivery{}).
			Where("request_id = ? AND status = ? AND (lease_until IS NULL OR lease_until <= ?)", reqID, bookAnnouncementDeliverySending, now).
			Updates(map[string]interface{}{
				"status":          bookAnnouncementDeliveryUncertain,
				"lease_until":     nil,
				"last_error_code": "sending_lease_expired",
			}).Error; err != nil {
			return err
		}
		if err := tx.Where("request_id = ?", reqID).First(&delivery).Error; err != nil {
			return err
		}
		switch delivery.Status {
		case bookAnnouncementDeliverySent:
			return errBookAnnouncementAlreadySent
		case bookAnnouncementDeliverySending:
			return errBookAnnouncementInProgress
		case bookAnnouncementDeliveryUncertain:
			return errBookAnnouncementUncertain
		case bookAnnouncementDeliveryPending, bookAnnouncementDeliveryFailed:
		default:
			return fmt.Errorf("BOOK_ANNOUNCEMENT_STATUS_INVALID:%s", delivery.Status)
		}
		if delivery.TargetChatID != targetChatID {
			return fmt.Errorf("公告目标与既有投递记录不一致，请恢复原目标后重试")
		}
		res := tx.Model(&model.BookRequestAnnouncementDelivery{}).
			Where("request_id = ? AND status IN ?", reqID, []string{bookAnnouncementDeliveryPending, bookAnnouncementDeliveryFailed}).
			Updates(map[string]interface{}{
				"item_id":             itemID,
				"target_chat_id":      targetChatID,
				"requested_by":        requestedBy,
				"status":              bookAnnouncementDeliverySending,
				"attempt_count":       gorm.Expr("attempt_count + 1"),
				"lease_until":         leaseUntil,
				"last_error_code":     "",
				"telegram_message_id": 0,
				"sent_at":             nil,
				"resolved_at":         nil,
				"resolved_by":         0,
				"resolution":          "",
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errBookAnnouncementInProgress
		}
		return tx.Where("request_id = ?", reqID).First(&delivery).Error
	})
	return delivery, err
}

func finalizeBookRequestAnnouncementDelivery(req model.BookRequest, actorID int64, actorName string, messageID int, now time.Time) error {
	if storage.DB == nil || req.ID == 0 || messageID == 0 {
		return fmt.Errorf("BOOK_ANNOUNCEMENT_FINALIZE_INVALID")
	}
	if now.IsZero() {
		now = time.Now()
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		lastErr = storage.DB.Transaction(func(tx *gorm.DB) error {
			res := tx.Model(&model.BookRequestAnnouncementDelivery{}).
				Where("request_id = ? AND status = ?", req.ID, bookAnnouncementDeliverySending).
				Updates(map[string]interface{}{
					"status":              bookAnnouncementDeliverySent,
					"telegram_message_id": messageID,
					"sent_at":             now,
					"lease_until":         nil,
					"last_error_code":     "",
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("BOOK_ANNOUNCEMENT_FINALIZE_MISSED")
			}
			return CreateBookRequestLogInTx(tx, req.ID, actorID, actorName, "group_announce", req.Status, req.Status, "admin published book request group announcement")
		})
		if lastErr == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
		}
	}
	return lastErr
}

func markBookRequestAnnouncementDelivery(reqID uint, status string, errorCode string, knownMessageID ...int) error {
	if storage.DB == nil || reqID == 0 {
		return fmt.Errorf("BOOK_ANNOUNCEMENT_MARK_INVALID")
	}
	if status != bookAnnouncementDeliveryFailed && status != bookAnnouncementDeliveryUncertain {
		return fmt.Errorf("BOOK_ANNOUNCEMENT_MARK_STATUS_INVALID")
	}
	updates := map[string]interface{}{
		"status":          status,
		"lease_until":     nil,
		"last_error_code": textfmt.TruncateRunes(textfmt.FormatPlainValue(errorCode), 120),
	}
	if len(knownMessageID) > 0 && knownMessageID[0] > 0 {
		updates["telegram_message_id"] = knownMessageID[0]
	}
	res := storage.DB.Model(&model.BookRequestAnnouncementDelivery{}).
		Where("request_id = ? AND status = ?", reqID, bookAnnouncementDeliverySending).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("BOOK_ANNOUNCEMENT_MARK_MISSED")
	}
	return nil
}

func bookRequestGroupAnnouncementAlreadyPublished(reqID uint) (bool, error) {
	if storage.DB == nil || reqID == 0 {
		return false, fmt.Errorf("BOOK_ANNOUNCEMENT_LOOKUP_INVALID")
	}
	var delivery model.BookRequestAnnouncementDelivery
	err := storage.DB.Where("request_id = ?", reqID).First(&delivery).Error
	if err == nil {
		return delivery.Status == bookAnnouncementDeliverySent, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	var count int64
	if err := storage.DB.Model(&model.BookRequestLog{}).Where("request_id = ? AND action = ?", reqID, "group_announce").Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func RecoverBookRequestAnnouncementDeliveriesOnStartup() error {
	if storage.DB == nil {
		return fmt.Errorf("BOOK_ANNOUNCEMENT_DB_EMPTY")
	}
	return storage.DB.Model(&model.BookRequestAnnouncementDelivery{}).
		Where("status = ?", bookAnnouncementDeliverySending).
		Updates(map[string]interface{}{
			"status":          bookAnnouncementDeliveryUncertain,
			"lease_until":     nil,
			"last_error_code": "startup_recovered_sending",
		}).Error
}

// recoverExpiredBookRequestAnnouncementDeliveries converts abandoned sending
// leases to uncertain. It never retries Telegram: a super administrator must
// reconcile the deterministic BR-<request> key before any later send attempt.
func recoverExpiredBookRequestAnnouncementDeliveries(now time.Time) error {
	if storage.DB == nil {
		return fmt.Errorf("BOOK_ANNOUNCEMENT_DB_EMPTY")
	}
	if now.IsZero() {
		now = time.Now()
	}
	return storage.DB.Model(&model.BookRequestAnnouncementDelivery{}).
		Where("status = ? AND (lease_until IS NULL OR lease_until <= ?)", bookAnnouncementDeliverySending, now).
		Updates(map[string]interface{}{
			"status":          bookAnnouncementDeliveryUncertain,
			"lease_until":     nil,
			"last_error_code": "sending_lease_expired",
		}).Error
}

func listUncertainBookRequestAnnouncementDeliveries(limit int) ([]model.BookRequestAnnouncementDelivery, error) {
	if storage.DB == nil {
		return nil, fmt.Errorf("BOOK_ANNOUNCEMENT_DB_EMPTY")
	}
	if err := recoverExpiredBookRequestAnnouncementDeliveries(time.Now()); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	var deliveries []model.BookRequestAnnouncementDelivery
	err := storage.DB.Where("status = ?", bookAnnouncementDeliveryUncertain).
		Order("updated_at ASC, id ASC").Limit(limit).Find(&deliveries).Error
	return deliveries, err
}

func loadUncertainBookRequestAnnouncementDelivery(reqID uint) (model.BookRequestAnnouncementDelivery, error) {
	var delivery model.BookRequestAnnouncementDelivery
	if storage.DB == nil || reqID == 0 {
		return delivery, fmt.Errorf("BOOK_ANNOUNCEMENT_LOOKUP_INVALID")
	}
	err := storage.DB.Where("request_id = ? AND status = ?", reqID, bookAnnouncementDeliveryUncertain).First(&delivery).Error
	return delivery, err
}

// confirmBookRequestAnnouncementSent closes an uncertain delivery after a
// super administrator has located its deterministic BR-<request> key in the
// target group. State, business log and audit evidence commit atomically.
func confirmBookRequestAnnouncementSent(reqID uint, messageID int, actorID int64, actorName string, now time.Time) error {
	if storage.DB == nil || reqID == 0 || messageID <= 0 || actorID == 0 {
		return fmt.Errorf("BOOK_ANNOUNCEMENT_CONFIRM_SENT_INVALID")
	}
	if now.IsZero() {
		now = time.Now()
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var req model.BookRequest
		if err := tx.Where("id = ?", reqID).First(&req).Error; err != nil {
			return err
		}
		res := tx.Model(&model.BookRequestAnnouncementDelivery{}).
			Where("request_id = ? AND status = ?", reqID, bookAnnouncementDeliveryUncertain).
			Updates(map[string]interface{}{
				"status":              bookAnnouncementDeliverySent,
				"telegram_message_id": messageID,
				"sent_at":             now,
				"resolved_at":         now,
				"resolved_by":         actorID,
				"resolution":          "manual_confirmed_sent",
				"lease_until":         nil,
				"last_error_code":     "",
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("BOOK_ANNOUNCEMENT_CONFIRM_SENT_CONFLICT")
		}
		if err := CreateBookRequestLogInTx(tx, req.ID, actorID, actorName, "group_announce", req.Status, req.Status, fmt.Sprintf("super admin confirmed existing group announcement message=%d", messageID)); err != nil {
			return err
		}
		return storage.WriteAuditLogInTx(tx, actorID, "RESOLVE_BOOK_ANNOUNCEMENT_UNCERTAIN_SENT", fmt.Sprintf("book_request:%d", reqID), 0, fmt.Sprintf("超级管理员人工核对公告凭证 %s 已存在，message_id=%d", formatBookAnnouncementDeliveryKey(reqID), messageID))
	})
}

// confirmBookRequestAnnouncementAbsent does not send Telegram inside the
// transaction. It only releases the delivery back to failed/retryable after a
// super administrator explicitly confirms that BR-<request> is absent.
func confirmBookRequestAnnouncementAbsent(reqID uint, actorID int64, now time.Time) error {
	if storage.DB == nil || reqID == 0 || actorID == 0 {
		return fmt.Errorf("BOOK_ANNOUNCEMENT_CONFIRM_ABSENT_INVALID")
	}
	if now.IsZero() {
		now = time.Now()
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.BookRequestAnnouncementDelivery{}).
			Where("request_id = ? AND status = ?", reqID, bookAnnouncementDeliveryUncertain).
			Updates(map[string]interface{}{
				"status":          bookAnnouncementDeliveryFailed,
				"resolved_at":     now,
				"resolved_by":     actorID,
				"resolution":      "manual_confirmed_absent",
				"lease_until":     nil,
				"last_error_code": "manual_confirmed_absent",
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("BOOK_ANNOUNCEMENT_CONFIRM_ABSENT_CONFLICT")
		}
		return storage.WriteAuditLogInTx(tx, actorID, "RESOLVE_BOOK_ANNOUNCEMENT_UNCERTAIN_ABSENT", fmt.Sprintf("book_request:%d", reqID), 0, fmt.Sprintf("超级管理员人工核对公告凭证 %s 不存在，解除自动重发锁", formatBookAnnouncementDeliveryKey(reqID)))
	})
}
