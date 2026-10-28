package activity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const BlindBoxCost = 20

func ExecuteBlindBoxOpen(tgUser *tgbotapi.User) (string, string, error) {
	if tgUser == nil {
		return "", "", storage.ErrTelegramUserMissing
	}

	userID := tgUser.ID
	safeName := textfmt.EscapeMarkdown(tgUser.FirstName)
	resultPrefix := fmt.Sprintf("📦 权益盲盒开启中...\n💰 已扣除 `%d` 积分。\n\n", BlindBoxCost)

	var txReplyMsg, txBroadcastMsg string
	blindBoxRefID := fmt.Sprintf("blind_box:%d:%s", userID, security.GenerateRandomCode(8))
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := storage.ApplyPointDeltaInTx(
			tx,
			userID,
			-BlindBoxCost,
			"blind_box_cost",
			fmt.Sprintf("开启权益盲盒，消耗 %d 积分", BlindBoxCost),
			"blind_box",
			blindBoxRefID,
		); err != nil {
			if errors.Is(err, storage.ErrPointsNotEnough) {
				return storage.ErrPointsNotEnough
			}
			return err
		}

		nBig, err := rand.Int(rand.Reader, big.NewInt(1000))
		if err != nil {
			return err
		}
		roll := int(nBig.Int64()) + 1

		switch {
		case roll <= 739:
			code := fmt.Sprintf("R%d-%s", 1, security.GenerateRandomCode(16))
			if err := storage.CreateRenewCodeRecord(tx, code, 1); err != nil {
				return err
			}
			txReplyMsg = resultPrefix + fmt.Sprintf("🎉 恭喜获得保底奖励：**【1天续期卡】**！\n💳 专属卡密：`%s`\n(卡密已升级为16位安全密钥，请在此发送充值)", code)
		case roll <= 939:
			code := fmt.Sprintf("R%d-%s", 3, security.GenerateRandomCode(16))
			if err := storage.CreateRenewCodeRecord(tx, code, 3); err != nil {
				return err
			}
			txReplyMsg = resultPrefix + fmt.Sprintf("🎉 恭喜获得小奖：**【3天续期卡】**！\n💳 专属卡密：`%s`\n(卡密已升级为16位安全密钥，请在此发送充值)", code)
		case roll <= 959:
			code := security.GenerateRandomCode(16)
			if err := storage.CreateInviteCodeRecord(tx, code); err != nil {
				return err
			}
			txReplyMsg = resultPrefix + fmt.Sprintf("🎉 运气不错！恭喜获得：**【专属邀请码】**！\n🎫 邀请码：`%s`\n(可直接用于开户)", code)
		case roll <= 989:
			code := fmt.Sprintf("R%d-%s", 30, security.GenerateRandomCode(16))
			if err := storage.CreateRenewCodeRecord(tx, code, 30); err != nil {
				return err
			}
			txReplyMsg = resultPrefix + fmt.Sprintf("🎊 运气爆棚！获得大奖：**【30天续期月卡】**！\n💳 专属卡密：`%s`", code)
			txBroadcastMsg = fmt.Sprintf("🎰 **欧皇降临！**\n\n恭喜 @%s 在权益盲盒中单抽入魂，斩获大奖 **【💳 30天续期月卡】**！", safeName)
		case roll <= 999:
			code := fmt.Sprintf("R%d-%s", 90, security.GenerateRandomCode(16))
			if err := storage.CreateRenewCodeRecord(tx, code, 90); err != nil {
				return err
			}
			txReplyMsg = resultPrefix + fmt.Sprintf("🌟 鸿运当头！获得珍稀大奖：**【90天续期季卡】**！\n💳 专属卡密：`%s`", code)
			txBroadcastMsg = fmt.Sprintf("🌟 **鸿运降临！**\n\n恭喜 @%s 在权益盲盒中斩获珍稀大奖 **【💳 90天续期季卡】**！", safeName)
		case roll <= 1000:
			code := fmt.Sprintf("R%d-%s", 365, security.GenerateRandomCode(16))
			if err := storage.CreateRenewCodeRecord(tx, code, 365); err != nil {
				return err
			}
			txReplyMsg = resultPrefix + fmt.Sprintf("👑 欧皇附体！！！获得终极大奖：**【365天尊享年卡】**！！！\n💳 专属卡密：`%s`", code)
			txBroadcastMsg = fmt.Sprintf("👑👑 **全服通报：终极欧皇诞生！** 👑👑\n\n天呐！@%s 爆发了惊人气运，直接抽中了终极大奖 **【👑 365天尊享年卡】**！！！", safeName)
		}
		return nil
	})

	if err != nil {
		return "", "", err
	}

	return txReplyMsg, txBroadcastMsg, nil
}
