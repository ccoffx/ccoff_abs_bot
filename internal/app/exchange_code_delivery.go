package app

import (
	"errors"
	"fmt"
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var errExchangeCodeDeliveryPending = errors.New("EXCHANGE_CODE_DELIVERY_PENDING")

func ensureNoPendingExchangeCodeReceiptInTx(tx *gorm.DB, userID int64) error {
	var receipt model.ExchangeCodeReceipt
	err := tx.Select("id").Where("user_id = ? AND delivered_at IS NULL", userID).First(&receipt).Error
	if err == nil {
		return errExchangeCodeDeliveryPending
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return err
}

func createExchangeCodeReceiptInTx(tx *gorm.DB, userID int64, kind, code string, price int) error {
	if tx == nil || userID == 0 || code == "" || price < 0 {
		return fmt.Errorf("EXCHANGE_CODE_RECEIPT_INVALID")
	}
	var reply string
	switch kind {
	case "invite":
		reply = fmt.Sprintf("🎉 **兑换成功！扣除 %d 积分**\n🎁 你的专属邀请码为：`%s`", price, code)
	case "renew":
		reply = fmt.Sprintf("🎉 **兑换成功！扣除 %d 积分**\n🎁 30天续期卡密为：`%s`", price, code)
	default:
		return fmt.Errorf("EXCHANGE_CODE_RECEIPT_KIND_INVALID")
	}
	id := "EXC-" + security.GenerateRandomCode(20)
	encrypted, err := security.EncryptSecretReceipt("exchange-code", id, reply)
	if err != nil {
		return err
	}
	res := tx.Create(&model.ExchangeCodeReceipt{ID: id, UserID: userID, Kind: kind, ReplyEncrypted: encrypted})
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return errExchangeCodeDeliveryPending
		}
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("EXCHANGE_CODE_RECEIPT_CREATE_MISSED")
	}
	return nil
}

func deliverPendingExchangeCodeReceipt(bot *tgbotapi.BotAPI, userID int64) (bool, error) {
	var receipt model.ExchangeCodeReceipt
	err := storage.DB.Where("user_id = ? AND delivered_at IS NULL", userID).Order("created_at ASC").First(&receipt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	reply, err := security.DecryptSecretReceipt("exchange-code", receipt.ID, receipt.ReplyEncrypted)
	if err != nil {
		return true, err
	}
	if err := telegram.ReplyTextChecked(bot, userID, reply); err != nil {
		return true, err
	}
	res := storage.DB.Model(&model.ExchangeCodeReceipt{}).
		Where("id = ? AND user_id = ? AND delivered_at IS NULL", receipt.ID, userID).
		Update("delivered_at", time.Now())
	if res.Error != nil {
		return true, res.Error
	}
	if res.RowsAffected != 1 {
		return true, fmt.Errorf("EXCHANGE_CODE_RECEIPT_ACK_MISSED")
	}
	return true, nil
}

func replayPendingExchangeCode(bot *tgbotapi.BotAPI, userID int64) bool {
	found, err := deliverPendingExchangeCodeReceipt(bot, userID)
	if err != nil {
		log.Printf("⚠️ 积分兑换卡密交付失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, userID, "⚠️ 已兑换的卡密暂未确认送达。请稍后再次进入“积分兑换”取回原卡密；本次不会重复扣分。")
	}
	return found || err != nil
}
