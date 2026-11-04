package storage

import (
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/textfmt"
)

func WriteAuditLog(actorID int64, action string, target string, detail string) {
	writeAuditLogWithDelta(actorID, action, target, 0, detail)
}

func writeAuditLogWithDelta(actorID int64, action string, target string, delta int, detail string) {
	if err := WriteAuditLogInTx(DB, actorID, action, target, delta, detail); err != nil {
		log.Printf("⚠️ 写入审计日志失败: actor=%d action=%s target=%s err=%s", actorID, textfmt.FormatPlainValue(action), textfmt.FormatPlainValue(target), textfmt.FormatPlainError(err))
	}
}

func WriteAuditLogInTx(tx *gorm.DB, actorID int64, action string, target string, delta int, detail string) error {
	if tx == nil {
		return fmt.Errorf("AUDIT_TX_EMPTY")
	}
	role, err := getAuditActorRoleInTx(tx, actorID)
	if err != nil {
		return err
	}

	target = textfmt.FormatAuditTextForStorage(target, textfmt.AuditTargetMaxRunes)
	detail = textfmt.FormatAuditTextForStorage(detail, textfmt.AuditDetailMaxRunes)

	res := tx.Create(&model.AuditLog{
		ActorID:   actorID,
		ActorRole: role,
		Action:    action,
		Target:    target,
		Delta:     delta,
		Detail:    detail,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("AUDIT_LOG_CREATE_MISSED")
	}
	return nil
}

func AuditDayRange(t time.Time) (time.Time, time.Time) {
	loc := time.FixedZone("CST", 8*3600)
	local := t.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}
