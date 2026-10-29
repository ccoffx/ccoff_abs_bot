package activity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

var errLotteryAlreadyClaimed = errors.New("LOTTERY_ALREADY_CLAIMED")
var lotteryClaimLocation = time.FixedZone("CST", 8*3600)

func formatLotteryClaimDeadline(activity model.LotteryActivity) string {
	if activity.ClaimDeadlineAt == nil {
		return "未开奖"
	}
	return activity.ClaimDeadlineAt.In(lotteryClaimLocation).Format("2006-01-02 15:04:05") + "（北京时间）"
}

// HandleLotteryClaimCommand only grants to the sender, in a private chat.
// It can be invoked without discarding an in-progress form.
func HandleLotteryClaimCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if bot == nil || msg == nil || msg.Chat == nil || msg.From == nil || strings.TrimSpace(text) != lotteryClaimCommand {
		return false
	}
	if !msg.Chat.IsPrivate() {
		telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
		hint := "🎲 请打开 Bot 私聊，点击 Start / 开始后发送：" + textfmt.Copyable(lotteryClaimCommand) + "。\n奖励须在开奖后 24 小时内领取，逾期未领取自动失效。群内不会发放奖品或展示卡密。"
		if bot.Self.UserName != "" {
			hint += "\nhttps://t.me/" + bot.Self.UserName
		}
		SendLotteryGroupPlainText(bot, msg.Chat.ID, hint)
		return true
	}
	claimAllLotteryRewards(bot, msg.Chat.ID, msg.From.ID)
	return true
}

func claimAllLotteryRewards(bot *tgbotapi.BotAPI, chatID, userID int64) {
	var winners []model.LotteryWinner
	if err := storage.DB.Where("user_id = ? AND (status = ? OR (status = ? AND reward_delivered_at IS NULL))", userID, lotteryWinnerPending, lotteryWinnerClaimed).Order("id asc").Find(&winners).Error; err != nil {
		telegram.SendPlainText(bot, chatID, "❌ 查询奖励失败，请稍后重新发送 "+textfmt.Copyable(lotteryClaimCommand)+"。")
		return
	}
	handled := false
	for _, winner := range winners {
		// The transaction reloads the activity and winner to reject stale status,
		// expiry, and concurrent claims. Each reward commits independently.
		reply, err := claimLotteryWinner(model.LotteryActivity{Model: gorm.Model{ID: winner.ActivityID}}, winner)
		if errors.Is(err, errLotteryAlreadyClaimed) {
			continue
		}
		handled = true
		if errors.Is(err, storage.ErrLotteryClaimExpired) {
			telegram.SendPlainText(bot, chatID, fmt.Sprintf("⏳ 抽奖 #%d 的奖励已超过 24 小时领取期限，已失效。", winner.ActivityID))
			continue
		}
		if err != nil {
			log.Printf("抽奖领奖失败: activity=%d winner=%d user=%d err=%s", winner.ActivityID, winner.ID, userID, textfmt.FormatPlainError(err))
			telegram.SendPlainText(bot, chatID, fmt.Sprintf("❌ 抽奖 #%d 的奖励暂时领取失败，已保留重试资格。请稍后发送 %s。", winner.ActivityID, textfmt.Copyable(lotteryClaimCommand)))
			continue
		}
		if err := sendLotteryPlainText(bot, chatID, reply); err != nil {
			log.Printf("抽奖领取结果发送失败，保留原结果供重试: winner=%d user=%d err=%s", winner.ID, userID, textfmt.FormatTelegramSendError(err))
			telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 抽奖 #%d 用户 %d 已领取奖励，但私聊发送失败。已加密保存领取结果，用户可再次发送领取奖励重试。", winner.ActivityID, userID))
			continue
		}
		deliveredAt := time.Now()
		res := storage.DB.Model(&model.LotteryWinner{}).Where("id = ? AND user_id = ? AND status = ? AND reward_delivered_at IS NULL", winner.ID, userID, lotteryWinnerClaimed).Update("reward_delivered_at", &deliveredAt)
		if res.Error != nil {
			log.Printf("抽奖领取结果送达标记失败: winner=%d err=%s", winner.ID, textfmt.FormatPlainError(res.Error))
		}
	}
	if !handled {
		telegram.SendPlainText(bot, chatID, "当前没有待领取的抽奖奖励。已领取的奖励不会重复发放；奖励须在开奖后 24 小时内领取，逾期自动失效。")
	}
}

// The persisted reply includes any newly generated code. Both the asset change
// and this ciphertext commit together, so failed delivery never loses the code.
func lotteryRewardCipher() (cipher.AEAD, error) {
	pepper := security.GetSensitivePepper()
	if pepper == "" {
		return nil, storage.ErrSecurityPepperNotConfigured
	}
	key := sha256.Sum256([]byte("lottery-reward-result:" + pepper))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func lotteryRewardAAD(winner model.LotteryWinner) []byte {
	return []byte(fmt.Sprintf("activity=%d:winner=%d:user=%d", winner.ActivityID, winner.ID, winner.UserID))
}
func encryptLotteryRewardResult(reply string, winner model.LotteryWinner) (string, error) {
	aead, err := lotteryRewardCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := aead.Seal(nonce, nonce, []byte(reply), lotteryRewardAAD(winner))
	return "gcm$" + base64.RawURLEncoding.EncodeToString(payload), nil
}
func decryptLotteryRewardResult(winner model.LotteryWinner) (string, error) {
	if !strings.HasPrefix(winner.RewardResultEncrypted, "gcm$") {
		return "", fmt.Errorf("INVALID_LOTTERY_REWARD_CIPHER")
	}
	aead, err := lotteryRewardCipher()
	if err != nil {
		return "", err
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(winner.RewardResultEncrypted, "gcm$"))
	if err != nil {
		return "", err
	}
	if len(payload) < aead.NonceSize()+aead.Overhead() {
		return "", fmt.Errorf("INVALID_LOTTERY_REWARD_PAYLOAD")
	}
	plain, err := aead.Open(nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], lotteryRewardAAD(winner))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func expirePendingLotteryRewards(now time.Time) {
	var winners []model.LotteryWinner
	err := storage.DB.Model(&model.LotteryWinner{}).Select("lottery_winners.*").Joins("JOIN lottery_activities a ON a.id = lottery_winners.activity_id").Where("lottery_winners.status = ? AND a.status = ? AND a.deleted_at IS NULL AND a.claim_deadline_at <= ?", lotteryWinnerPending, lotteryStatusDrawn, now).Order("lottery_winners.id asc").Limit(100).Find(&winners).Error
	if err != nil {
		log.Printf("查询过期抽奖奖励失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, winner := range winners {
		err := storage.DB.Transaction(func(tx *gorm.DB) error {
			_, err := expireLotteryWinnerInTx(tx, winner.ActivityID, winner.ID, winner.UserID, "24 hour claim deadline exceeded")
			return err
		})
		if err != nil {
			log.Printf("抽奖奖励过期处理失败: winner=%d err=%s", winner.ID, textfmt.FormatPlainError(err))
		}
	}
}
