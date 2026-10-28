package activity

import (
	"crypto/rand"
	"encoding/json"
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
)

const (
	treasureBoxCount      = 9
	treasureEntryCost     = 3
	treasureDailyRounds   = 3
	treasureRoundDuration = time.Minute
	treasureCooldown      = time.Minute
	treasurePublishing    = "publishing"
	TreasureActive        = "active"
	TreasureClosed        = "closed"
	TreasureCancelled     = "cancelled"
)

var (
	treasureMutex         sync.Mutex
	ErrTreasureBusy       = errors.New("本群已有寻宝局正在进行，请在原藏宝图开箱")
	ErrTreasureDailyLimit = errors.New("本群今日已开满 3 局，请明天再来")
	ErrTreasureCooldown   = errors.New("藏宝图正在整理，请在上一局结束 1 分钟后再发起")
	ErrTreasureExpired    = errors.New("本局已结束，未扣除积分")
	ErrTreasurePicked     = errors.New("你本局已经开过宝箱了，不会重复扣分")
	ErrTreasureBoxTaken   = errors.New("这个宝箱已经被打开了，请选择其他宝箱")
	ErrTreasurePanel      = errors.New("藏宝图已失效，请使用本群最新面板")
)

func ShuffledTreasureRewards() ([]int, error) {
	rewards := []int{0, 0, 0, 0, 2, 2, 2, 5, 10}
	for i := len(rewards) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, err
		}
		j := int(n.Int64())
		rewards[i], rewards[j] = rewards[j], rewards[i]
	}
	return rewards, nil
}

func TreasureRewards(round model.TreasureRound) ([]int, error) {
	var rewards []int
	if err := json.Unmarshal([]byte(round.RewardsJSON), &rewards); err != nil {
		return nil, err
	}
	counts := map[int]int{}
	for _, reward := range rewards {
		counts[reward]++
	}
	if len(rewards) != treasureBoxCount || counts[0] != 4 || counts[2] != 3 || counts[5] != 1 || counts[10] != 1 || round.EntryCost != treasureEntryCost {
		return nil, fmt.Errorf("TREASURE_REWARDS_INVALID")
	}
	return rewards, nil
}

func CreateTreasureRound(chatID int64, now time.Time) (model.TreasureRound, error) {
	if chatID >= 0 {
		return model.TreasureRound{}, ErrTreasurePanel
	}
	rewards, err := ShuffledTreasureRewards()
	if err != nil {
		return model.TreasureRound{}, err
	}
	encoded, err := json.Marshal(rewards)
	if err != nil {
		return model.TreasureRound{}, err
	}
	round := model.TreasureRound{ChatID: chatID, CreatedAt: now, DayKey: now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02"), Status: treasurePublishing, EntryCost: treasureEntryCost, RewardsJSON: string(encoded), ExpiresAt: now.Add(treasureRoundDuration)}
	treasureMutex.Lock()
	defer treasureMutex.Unlock()
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		var active, today int64
		if err := tx.Model(&model.TreasureRound{}).Where("chat_id = ? AND status IN ?", chatID, []string{treasurePublishing, TreasureActive}).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return ErrTreasureBusy
		}
		if err := tx.Model(&model.TreasureRound{}).Where("chat_id = ? AND day_key = ? AND status <> ?", chatID, round.DayKey, TreasureCancelled).Count(&today).Error; err != nil {
			return err
		}
		if today >= treasureDailyRounds {
			return ErrTreasureDailyLimit
		}
		var latest model.TreasureRound
		if err := tx.Where("chat_id = ? AND status = ?", chatID, TreasureClosed).Order("id DESC").First(&latest).Error; err == nil {
			if now.Before(latest.ClosedAt.Add(treasureCooldown)) {
				return ErrTreasureCooldown
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		res := tx.Create(&round)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("TREASURE_CREATE_MISSED")
		}
		return nil
	})
	return round, err
}

func ActivateTreasureRound(roundID uint, messageID int, now time.Time) error {
	if messageID <= 0 {
		return ErrTreasurePanel
	}
	treasureMutex.Lock()
	defer treasureMutex.Unlock()
	res := storage.DB.Model(&model.TreasureRound{}).Where("id = ? AND status = ? AND expires_at > ?", roundID, treasurePublishing, now).
		Updates(map[string]interface{}{"status": TreasureActive, "message_id": messageID})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrTreasureExpired
	}
	return nil
}

func PickTreasureBox(roundID uint, chatID int64, messageID, boxIndex int, user *tgbotapi.User) (model.TreasurePick, error) {
	if user == nil || user.ID == 0 || boxIndex < 0 || boxIndex >= treasureBoxCount {
		return model.TreasurePick{}, ErrTreasurePanel
	}
	treasureMutex.Lock()
	defer treasureMutex.Unlock()
	var pick model.TreasurePick
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var round model.TreasureRound
		if err := tx.First(&round, roundID).Error; err != nil {
			return err
		}
		if round.ChatID != chatID || chatID >= 0 || round.MessageID != messageID || messageID <= 0 {
			return ErrTreasurePanel
		}
		now := time.Now()
		if round.Status != TreasureActive || !now.Before(round.ExpiresAt) {
			return ErrTreasureExpired
		}
		rewards, err := TreasureRewards(round)
		if err != nil {
			return err
		}
		var existing int64
		if err := tx.Model(&model.TreasurePick{}).Where("round_id = ? AND user_id = ?", roundID, user.ID).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return ErrTreasurePicked
		}
		if err := tx.Model(&model.TreasurePick{}).Where("round_id = ? AND box_index = ?", roundID, boxIndex).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return ErrTreasureBoxTaken
		}
		_, name, err := storage.EnsureUserWalletInTx(tx, user)
		if err != nil {
			return err
		}
		pick = model.TreasurePick{CreatedAt: now, RoundID: roundID, BoxIndex: boxIndex, UserID: user.ID, UserName: textfmt.LotteryDisplayText(name, 24, "道友"), Cost: round.EntryCost, Reward: rewards[boxIndex]}
		res := tx.Create(&pick)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("TREASURE_PICK_MISSED")
		}
		ref := strconv.FormatUint(uint64(roundID), 10)
		if err := storage.ApplyPointDeltaInTx(tx, user.ID, -pick.Cost, "treasure_cost", fmt.Sprintf("寻宝开第 %d 个宝箱，消耗 %d 积分", boxIndex+1, pick.Cost), "treasure", ref); err != nil {
			return err
		}
		return storage.ApplyPointDeltaInTx(tx, user.ID, pick.Reward, "treasure_reward", fmt.Sprintf("寻宝第 %d 个宝箱获得 %d 积分", boxIndex+1, pick.Reward), "treasure", ref)
	})
	if err != nil {
		return model.TreasurePick{}, err
	}
	return pick, nil
}

// 到期或开满才结算；入池和结束状态原子提交，重启/重试不会重复入池。
func CloseTreasureRound(roundID uint, now time.Time) (bool, bool, error) {
	treasureMutex.Lock()
	defer treasureMutex.Unlock()
	closed, burst := false, false
	err := storage.RunFusionPoolLockedTransaction(func(tx *gorm.DB) error {
		var round model.TreasureRound
		if err := tx.First(&round, roundID).Error; err != nil {
			return err
		}
		if round.Status != TreasureActive {
			return nil
		}
		var picks []model.TreasurePick
		if err := tx.Where("round_id = ?", roundID).Find(&picks).Error; err != nil {
			return err
		}
		if now.Before(round.ExpiresAt) && len(picks) < treasureBoxCount {
			return nil
		}
		net := 0
		for _, pick := range picks {
			net += pick.Cost - pick.Reward
		}
		if net < 0 {
			net = 0
		}
		pool := 0
		if net > 0 {
			var err error
			pool, burst, err = storage.AddPointsToFusionPoolInTx(tx, net)
			if err != nil {
				return err
			}
		}
		res := tx.Model(&model.TreasureRound{}).Where("id = ? AND status = ?", roundID, TreasureActive).
			Updates(map[string]interface{}{"status": TreasureClosed, "closed_at": now, "pool_injected": net, "pool_after": pool})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("TREASURE_CLOSE_MISSED")
		}
		closed = true
		return nil
	})
	if err != nil {
		return false, false, err
	}
	return closed, burst, nil
}
