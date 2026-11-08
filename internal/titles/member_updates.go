package titles

import (
	"errors"
	"log"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

var memberUpdateMutex sync.Mutex

// 成员身份变化可能令 Telegram 丢弃原标签，或改用管理员头衔字段。
// 只对身份/可编辑权限变化重新排队，机器人自己更新头衔不会形成同步循环。
func HandleMemberUpdate(update *tgbotapi.ChatMemberUpdated) error {
	if update == nil || update.Chat.ID >= 0 || update.Chat.ID != defaultGroupID() {
		return nil
	}
	old, next := update.OldChatMember, update.NewChatMember
	if next.User == nil || next.User.ID <= 0 || next.User.IsBot ||
		(old.Status == next.Status && old.CanBeEdited == next.CanBeEdited && old.IsMember == next.IsMember) {
		return nil
	}
	if next.Status != "member" && next.Status != "administrator" && next.Status != "creator" &&
		!(next.Status == "restricted" && next.IsMember) {
		return nil
	}
	// 接收线程只写入独立队列，不获取 titleMutex，也不触碰佩戴表。
	// 称号计算或佩戴写入失败不能堵住所有用户的 Telegram 更新。
	return RequestMemberSync(update.Chat.ID, next.User.ID, update.Chat.Title)
}

// RequestMemberSync also lets confirmed group administration operations request
// synchronization when a permission edit does not change the member's role.
func RequestMemberSync(chatID, userID int64, groupName string) error {
	if chatID >= 0 || chatID != defaultGroupID() || userID <= 0 {
		return nil
	}
	return storage.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "chat_id"}, {Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"group_name": groupName, "version": gorm.Expr("version + 1"), "next_attempt_at": time.Time{},
		}),
	}).Create(&model.TitleMemberSyncRequest{ChatID: chatID, UserID: userID, GroupName: groupName, Version: 1}).Error
}

func ProcessMemberUpdates(now time.Time) {
	if !memberUpdateMutex.TryLock() {
		return
	}
	defer memberUpdateMutex.Unlock()
	started := time.Now()
	var requests []model.TitleMemberSyncRequest
	if err := storage.DB.Where("next_attempt_at <= ?", now).Order("next_attempt_at, chat_id, user_id").Limit(10).Find(&requests).Error; err != nil {
		log.Printf("⚠️ 读取成员身份称号同步事件失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, request := range requests {
		err := Transaction(func(tx *gorm.DB) error {
			if request.ChatID == defaultGroupID() {
				if err := resyncMemberInTx(tx, request); err != nil {
					return err
				}
			}
			// 重排与消费事件在同一事务中提交；较新事件保留到下一批。
			return tx.Where("chat_id = ? AND user_id = ? AND version = ?", request.ChatID, request.UserID, request.Version).Delete(&model.TitleMemberSyncRequest{}).Error
		})
		if err != nil {
			log.Printf("⚠️ 处理成员身份称号同步事件失败，将后台重试: user=%d err=%s", request.UserID, textfmt.FormatPlainError(err))
			if retryErr := storage.DB.Model(&model.TitleMemberSyncRequest{}).
				Where("chat_id = ? AND user_id = ? AND version = ?", request.ChatID, request.UserID, request.Version).
				Update("next_attempt_at", now.Add(time.Since(started)).Add(30*time.Second)).Error; retryErr != nil {
				log.Printf("⚠️ 保存成员身份同步重试时间失败: %s", textfmt.FormatPlainError(retryErr))
			}
		}
	}
}

func resyncMemberInTx(tx *gorm.DB, request model.TitleMemberSyncRequest) error {
	if err := TrackGroupInTx(tx, request.ChatID, request.GroupName); err != nil {
		return err
	}
	// 事件只持久化成员关系与已佩戴称号的重同步，不在长轮询
	// 中计算境界。新成员的默认称号由独立巡检创建；境界配置
	// 异常不能阻塞管理员身份事件或让整个更新流持续重试。
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.TitleMember{ChatID: request.ChatID, UserID: request.UserID}).Error; err != nil {
		return err
	}
	var equipment model.TitleEquipment
	err := tx.Where("chat_id = ? AND user_id = ?", request.ChatID, request.UserID).Take(&equipment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return setDesiredInTx(tx, equipment.ChatID, equipment.UserID, equipment.TitleKey, equipment.DesiredTag, true)
}
