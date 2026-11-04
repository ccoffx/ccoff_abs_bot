package app

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var errAdminCodeDeliveryPending = errors.New("ADMIN_CODE_DELIVERY_PENDING")

func ensureNoPendingAdminCodeReceiptInTx(tx *gorm.DB, actorID int64) error {
	var receipt model.AdminCodeReceipt
	err := tx.Select("id").Where("actor_id = ? AND delivered_at IS NULL", actorID).First(&receipt).Error
	if err == nil {
		return errAdminCodeDeliveryPending
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return err
}

func createAdminCodeReceiptInTx(tx *gorm.DB, actorID int64, kind string, days int, codes []string) error {
	if tx == nil || actorID == 0 || len(codes) == 0 {
		return fmt.Errorf("ADMIN_CODE_RECEIPT_INVALID")
	}
	var reply strings.Builder
	switch kind {
	case "invite":
		reply.WriteString("✅ **成功生成邀请码：**\n\n")
	case "renew":
		fmt.Fprintf(&reply, "✅ **成功生成 %d 天续期卡密：**\n\n", days)
	case "whitelist":
		reply.WriteString("🏳️ 已生成永久白名单卡，请妥善保存并仅交给指定用户：\n\n")
	default:
		return fmt.Errorf("ADMIN_CODE_RECEIPT_KIND_INVALID")
	}
	for _, code := range codes {
		if kind == "whitelist" {
			fmt.Fprintln(&reply, code)
		} else {
			fmt.Fprintf(&reply, "`%s`\n", code)
		}
	}
	if len(utf16.Encode([]rune(reply.String()))) > 3800 {
		return fmt.Errorf("ADMIN_CODE_RECEIPT_TOO_LONG")
	}
	id := "ACB-" + security.GenerateRandomCode(20)
	encrypted, err := security.EncryptSecretReceipt("admin-code-batch", id, reply.String())
	if err != nil {
		return err
	}
	res := tx.Create(&model.AdminCodeReceipt{ID: id, ActorID: actorID, Kind: kind, ReplyEncrypted: encrypted})
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return errAdminCodeDeliveryPending
		}
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("ADMIN_CODE_RECEIPT_CREATE_MISSED")
	}
	return nil
}

// Delivery is acknowledged only after Telegram accepts the exact encrypted
// batch. A failed send or database update leaves it available for replay.
func deliverPendingAdminCodeReceipt(bot *tgbotapi.BotAPI, actorID int64) (bool, error) {
	var receipt model.AdminCodeReceipt
	err := storage.DB.Where("actor_id = ? AND delivered_at IS NULL", actorID).Order("created_at ASC").First(&receipt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	reply, err := security.DecryptSecretReceipt("admin-code-batch", receipt.ID, receipt.ReplyEncrypted)
	if err != nil {
		return true, err
	}
	if err := telegram.ReplyTextChecked(bot, actorID, reply); err != nil {
		return true, err
	}
	res := storage.DB.Model(&model.AdminCodeReceipt{}).
		Where("id = ? AND actor_id = ? AND delivered_at IS NULL", receipt.ID, actorID).
		Update("delivered_at", time.Now())
	if res.Error != nil {
		return true, res.Error
	}
	if res.RowsAffected != 1 {
		return true, fmt.Errorf("ADMIN_CODE_RECEIPT_ACK_MISSED")
	}
	return true, nil
}

func replayPendingAdminCodes(bot *tgbotapi.BotAPI, actorID int64) bool {
	found, err := deliverPendingAdminCodeReceipt(bot, actorID)
	if err != nil {
		log.Printf("⚠️ 管理员卡密批次交付失败: actor=%d err=%s", actorID, textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, actorID, "⚠️ 已生成的卡密暂未确认送达。请稍后再次选择“生成邀请码”或“生成续期卡”取回同一批卡密；本次不会重复生成。")
	}
	return found || err != nil
}
