package activity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"ccoff-abs-bot/internal/titles"
)

const (
	TribulationEntryCost  = 5
	TribulationYang       = "yang"
	TribulationYin        = "yin"
	tribulationPublishing = "publishing"
	TribulationActive     = "active"
	TribulationSettling   = "settling"
	TribulationClosed     = "closed"
	TribulationCancelled  = "cancelled"
)

var (
	tribulationMutex             sync.Mutex
	ErrTribulationBusy           = errors.New("本群已有天雷猜阵正在进行，请在原面板参与")
	ErrTribulationPanel          = errors.New("猜阵面板已失效，请使用本群原面板")
	ErrTribulationExpired        = errors.New("本局已封阵或结束，本次未扣分")
	ErrTribulationJoined         = errors.New("你已入阵，不能修改选择；不会重复扣分")
	ErrTribulationJoinClosed     = errors.New("入阵已截止，请等待下一局")
	ErrTribulationSwitchDisabled = errors.New("换阵功能已取消，入阵后不能修改选择")
	ErrTribulationTitleNotHeld   = titles.ErrNotHeld
)

func validTribulationSide(side string) bool { return side == TribulationYang || side == TribulationYin }

func tribulationDeadlines(round *model.TribulationRound, now time.Time) {
	round.JoinUntil = now.Add(30 * time.Second)
	round.SwitchUntil = round.JoinUntil // 保留旧数据库字段，新局没有换阵阶段。
	round.ExpiresAt = round.JoinUntil
}

func CreateTribulationRound(chatID int64, chatName string, now time.Time) (model.TribulationRound, error) {
	if chatID >= 0 {
		return model.TribulationRound{}, ErrTribulationPanel
	}
	tribulationMutex.Lock()
	defer tribulationMutex.Unlock()
	round := model.TribulationRound{ChatID: chatID, ChatName: textfmt.LotteryDisplayText(chatName, 40, "游戏群"), CreatedAt: now, Status: tribulationPublishing, EntryCost: TribulationEntryCost}
	tribulationDeadlines(&round, now)
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.TribulationRound{}).Where("chat_id = ? AND status IN ?", chatID, []string{tribulationPublishing, TribulationActive, TribulationSettling}).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrTribulationBusy
		}
		if err := tx.Create(&round).Error; err != nil {
			return err
		}
		return titles.TrackGroupInTx(tx, chatID, chatName)
	})
	return round, err
}

func ActivateTribulationRound(id uint, messageID int, now time.Time) error {
	if messageID <= 0 {
		return ErrTribulationPanel
	}
	tribulationMutex.Lock()
	defer tribulationMutex.Unlock()
	round := model.TribulationRound{}
	tribulationDeadlines(&round, now)
	res := storage.DB.Model(&model.TribulationRound{}).Where("id = ? AND status = ? AND expires_at > ?", id, tribulationPublishing, now).Updates(map[string]any{
		"status": TribulationActive, "message_id": messageID, "join_until": round.JoinUntil, "switch_until": round.SwitchUntil, "expires_at": round.ExpiresAt, "panel_dirty": true,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrTribulationExpired
	}
	return nil
}

// 入阵使用服务器收到操作时的时间；旧面板的换阵回调一律拒绝。
func ChooseTribulationSide(id uint, chatID int64, messageID int, side string, switching bool, user *tgbotapi.User, now time.Time) (model.TribulationEntry, error) {
	if user == nil || user.ID <= 0 || user.IsBot || !validTribulationSide(side) {
		return model.TribulationEntry{}, ErrTribulationPanel
	}
	tribulationMutex.Lock()
	defer tribulationMutex.Unlock()
	var entry model.TribulationEntry
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var round model.TribulationRound
		if err := tx.First(&round, id).Error; err != nil {
			return err
		}
		if chatID >= 0 || round.ChatID != chatID || messageID <= 0 || round.MessageID != messageID {
			return ErrTribulationPanel
		}
		if switching {
			return ErrTribulationSwitchDisabled
		}
		if round.Status != TribulationActive {
			return ErrTribulationExpired
		}
		if !now.Before(round.JoinUntil) {
			return ErrTribulationJoinClosed
		}
		var count int64
		if err := tx.Model(&model.TribulationEntry{}).Where("round_id = ? AND user_id = ?", id, user.ID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrTribulationJoined
		}
		_, name, err := storage.EnsureUserWalletInTx(tx, user)
		if err != nil {
			return err
		}
		entry = model.TribulationEntry{RoundID: id, UserID: user.ID, UserName: textfmt.LotteryDisplayText(name, 24, "道友"), JoinedAt: now, InitialSide: side, Side: side, Cost: round.EntryCost}
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
		if err := storage.ApplyPointDeltaInTx(tx, user.ID, -entry.Cost, "tribulation_cost", fmt.Sprintf("天雷猜阵入阵，消耗 %d 积分", entry.Cost), "tribulation", strconv.FormatUint(uint64(id), 10)); err != nil {
			return err
		}
		if err := titles.EnsureMemberInTx(tx, chatID, user.ID); err != nil {
			return err
		}
		return tx.Model(&round).Update("panel_dirty", true).Error
	})
	if err != nil {
		return model.TribulationEntry{}, err
	}
	return entry, nil
}

// 先单独提交开奖结果，再在另一事务中发奖。即使发奖失败/重启，也只按已保存的阵营重试。
func CloseTribulationRound(id uint, now time.Time) (bool, error) {
	tribulationMutex.Lock()
	defer tribulationMutex.Unlock()
	if err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var round model.TribulationRound
		if err := tx.First(&round, id).Error; err != nil {
			return err
		}
		if round.Status != TribulationActive || now.Before(round.ExpiresAt) {
			return nil
		}
		var count int64
		if err := tx.Model(&model.TribulationEntry{}).Where("round_id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return tx.Model(&round).Updates(map[string]any{"status": TribulationCancelled, "closed_at": now, "panel_dirty": true}).Error
		}
		n, err := rand.Int(rand.Reader, big.NewInt(2))
		if err != nil {
			return err
		}
		winner := TribulationYang
		if n.Int64() == 1 {
			winner = TribulationYin
		}
		return tx.Model(&round).Updates(map[string]any{"status": TribulationSettling, "winning_side": winner, "panel_dirty": true}).Error
	}); err != nil {
		return false, err
	}
	closed := false
	err := titles.Transaction(func(tx *gorm.DB) error {
		var round model.TribulationRound
		if err := tx.First(&round, id).Error; err != nil {
			return err
		}
		if round.Status != TribulationSettling {
			return nil
		}
		if !validTribulationSide(round.WinningSide) {
			return fmt.Errorf("TRIBULATION_RESULT_INVALID")
		}
		var entries []model.TribulationEntry
		// 按入阵顺序结算，历史记录和积分流水保持稳定顺序。
		if err := tx.Where("round_id = ?", id).Order("id").Find(&entries).Error; err != nil {
			return err
		}
		for _, entry := range entries {
			reward := 0
			if entry.Side == round.WinningSide {
				reward = entry.Cost * 2
			}
			// 避雷属于实际参与的 Telegram 身份。途中注销或换绑不会
			// 令该玩家应得的奖励阻塞整局；仅恢复普通钱包，不恢复 ABS 账号。
			if reward > 0 {
				if _, _, err := storage.EnsureUserWalletInTx(tx, &tgbotapi.User{ID: entry.UserID, FirstName: entry.UserName}); err != nil {
					return err
				}
			}
			if err := storage.ApplyPointDeltaInTx(tx, entry.UserID, reward, "tribulation_reward", fmt.Sprintf("天雷猜阵结算，获得 %d 积分（净%+d）", reward, reward-entry.Cost), "tribulation", strconv.FormatUint(uint64(id), 10)); err != nil {
				return err
			}
			if err := tx.Model(&entry).Update("reward", reward).Error; err != nil {
				return err
			}
		}
		res := tx.Model(&round).Where("status = ?", TribulationSettling).Updates(map[string]any{"status": TribulationClosed, "closed_at": now, "title_eligible": false, "panel_dirty": true})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("TRIBULATION_CLOSE_MISSED")
		}
		closed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return closed, nil
}
