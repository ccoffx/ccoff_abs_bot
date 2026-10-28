package storage

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/textfmt"
)

func ApplyPointDeltaInTx(
	tx *gorm.DB,
	userID int64,
	delta int,
	txType string,
	description string,
	refType string,
	refID string,
) error {
	if tx == nil {
		return fmt.Errorf("数据库事务为空")
	}

	if userID == 0 {
		return fmt.Errorf("用户ID为空")
	}

	if delta == 0 {
		return nil
	}

	updateQuery := tx.Model(&model.User{}).Where("telegram_id = ?", userID)

	// 扣积分时加上余额条件，避免扣成负数。
	if delta < 0 {
		updateQuery = updateQuery.Where("points >= ?", -delta)
	}

	res := updateQuery.UpdateColumn("points", gorm.Expr("points + ?", delta))
	if res.Error != nil {
		return res.Error
	}

	if res.RowsAffected == 0 {
		var exists int64
		if err := tx.Model(&model.User{}).
			Where("telegram_id = ?", userID).
			Count(&exists).Error; err != nil {
			return err
		}

		if exists == 0 {
			return ErrUserNotFound
		}

		if delta < 0 {
			return ErrPointsNotEnough
		}

		return fmt.Errorf("POINT_UPDATE_FAILED")
	}

	var afterUser model.User
	if err := tx.Select("telegram_id", "username", "points").
		Where("telegram_id = ?", userID).
		First(&afterUser).Error; err != nil {
		return err
	}

	afterPoints := afterUser.Points
	beforePoints := afterPoints - delta

	txRes := tx.Create(&model.PointTransaction{
		UserID:        userID,
		UserName:      afterUser.Username,
		Type:          txType,
		Delta:         delta,
		BalanceBefore: beforePoints,
		BalanceAfter:  afterPoints,
		Description:   description,
		RefType:       refType,
		RefID:         refID,
	})
	if txRes.Error != nil {
		return txRes.Error
	}
	if txRes.RowsAffected == 0 {
		return fmt.Errorf("POINT_TRANSACTION_CREATE_MISSED")
	}
	return nil
}

func SignInMonthKey(t time.Time) string {
	loc := time.FixedZone("CST", 8*3600)
	return t.In(loc).Format("200601")
}

func SignInDateKey(t time.Time) string {
	loc := time.FixedZone("CST", 8*3600)
	return t.In(loc).Format("2006-01-02")
}

func IsUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}

	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "unique") ||
		strings.Contains(errText, "constraint failed") ||
		strings.Contains(errText, "duplicate")
}

func getUserRoleFromDBChecked(db *gorm.DB, userID int64) (string, error) {
	if config.AppConfig != nil && config.AppConfig.AdminIDs != nil && config.AppConfig.AdminIDs[userID] {
		return "super_admin", nil
	}
	if db == nil {
		return "user", fmt.Errorf("ROLE_DB_EMPTY")
	}
	var u model.User
	err := db.Where("telegram_id = ?", userID).First(&u).Error
	if err == nil {
		if strings.TrimSpace(u.Role) == "" {
			return "user", nil
		}
		return u.Role, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "user", nil
	}
	return "user", err
}

func getUserRoleFromDB(db *gorm.DB, userID int64) string {
	role, err := getUserRoleFromDBChecked(db, userID)
	if err != nil {
		log.Printf("⚠️ 用户角色读取失败，按普通用户处理: user=%d err=%s", userID, textfmt.FormatPlainError(err))
	}
	return role
}

func GetUserRole(userID int64) string {
	return getUserRoleFromDB(DB, userID)
}

func getAuditActorRoleInTx(tx *gorm.DB, actorID int64) (string, error) {
	if actorID == 0 {
		return "system", nil
	}
	return getUserRoleFromDBChecked(tx, actorID)
}

func IsSuperAdmin(userID int64) bool {
	return GetUserRole(userID) == "super_admin"
}

func AbsInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func AdminAdjustDailyLimitExceeded(todayTotal int, delta int) bool {
	return todayTotal+AbsInt(delta) > 20000
}

// ghostWalletUsernamePrefix 钱包占位用户名前缀（用户没有 Telegram 用户名时）。
// 该前缀的 username 是内部占位，不得出现在对外公告里（见 resolveSplashVictimLabels）。
const GhostWalletUsernamePrefix = "ghost_tg_"

func ghostWalletUsername(userID int64) string {
	return fmt.Sprintf("%s%d", GhostWalletUsernamePrefix, userID)
}

func EnsureUserWalletInTx(tx *gorm.DB, tgUser *tgbotapi.User) (model.User, string, error) {
	if tgUser == nil {
		return model.User{}, "", ErrTelegramUserMissing
	}

	displayName := textfmt.GetTelegramDisplayName(tgUser)
	var u model.User
	err := tx.Where("telegram_id = ?", tgUser.ID).First(&u).Error
	if err == nil {
		syncUserDisplayNameTx(tx, &u, displayName)
		return u, displayName, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.User{}, displayName, err
	}

	u = model.User{
		TelegramID:  tgUser.ID,
		Username:    ghostWalletUsername(tgUser.ID),
		DisplayName: strings.TrimSpace(strings.TrimPrefix(displayName, "@")),
		Points:      0,
	}
	res := tx.Create(&u)
	if res.Error != nil {
		if IsUniqueConstraintError(res.Error) {
			if retryErr := tx.Where("telegram_id = ?", tgUser.ID).First(&u).Error; retryErr == nil {
				syncUserDisplayNameTx(tx, &u, displayName)
				return u, displayName, nil
			}
		}
		return model.User{}, displayName, res.Error
	}
	if res.RowsAffected == 0 {
		return model.User{}, displayName, fmt.Errorf("USER_WALLET_CREATE_MISSED")
	}

	return u, displayName, nil
}

// syncUserDisplayNameTx 钱包事务内顺带刷新显示名缓存（尽力而为：失败仅记日志，不影响钱包主流程）。
// 群公告（雷劫外溢等）按 DB 选人，靠这份缓存按真人称呼，替代 ghost_tg_* 占位用户名。
// 仅在值变化时写入，避免热路径写放大。
func syncUserDisplayNameTx(tx *gorm.DB, u *model.User, displayName string) {
	if tx == nil || u == nil {
		return
	}
	fresh := strings.TrimSpace(strings.TrimPrefix(displayName, "@"))
	if fresh == "" || strings.TrimSpace(u.DisplayName) == fresh {
		return
	}
	if err := tx.Model(&model.User{}).Where("id = ?", u.ID).Update("display_name", fresh).Error; err != nil {
		log.Printf("⚠️ 用户显示名缓存刷新失败: user=%d err=%s", u.ID, textfmt.FormatPlainError(err))
		return
	}
	u.DisplayName = fresh
}

func EnsureUserWallet(tgUser *tgbotapi.User) (model.User, string, error) {
	var u model.User
	var displayName string
	err := DB.Transaction(func(tx *gorm.DB) error {
		txUser, txDisplayName, innerErr := EnsureUserWalletInTx(tx, tgUser)
		if innerErr != nil {
			return innerErr
		}
		u = txUser
		displayName = txDisplayName
		return nil
	})
	if err != nil {
		return model.User{}, "", err
	}
	return u, displayName, nil
}
