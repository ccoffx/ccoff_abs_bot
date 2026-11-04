package telegram

import (
	"fmt"
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

// ==========================================
// 🛡️ 核心基建：强健的权限与发卡防抖组件
// ==========================================
type groupMemberCacheEntry struct {
	inGroup   bool
	checkedAt time.Time
	expireAt  time.Time
}

func IsUserInGroup(bot *tgbotapi.BotAPI, userID int64, groupID int64) bool {
	if groupID == 0 {
		return true
	}

	cacheKey := fmt.Sprintf("%d:%d", groupID, userID)

	if cached, ok := groupMemberCache.Load(cacheKey); ok {
		if entry, ok := cached.(*groupMemberCacheEntry); ok && time.Now().Before(entry.expireAt) {
			return entry.inGroup
		}
	}

	member, err := bot.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: groupID,
			UserID: userID,
		},
	})
	if err != nil {
		now := time.Now()
		groupMemberCache.Store(cacheKey, &groupMemberCacheEntry{
			inGroup:   false,
			checkedAt: now,
			expireAt:  now.Add(groupMemberNegativeTTL),
		})
		return false
	}

	inGroup := member.Status == "member" ||
		member.Status == "creator" ||
		member.Status == "administrator" ||
		(member.Status == "restricted" && member.IsMember)

	ttl := groupMemberNegativeTTL
	if inGroup {
		ttl = groupMemberPositiveTTL
	}

	now := time.Now()
	groupMemberCache.Store(cacheKey, &groupMemberCacheEntry{
		inGroup:   inGroup,
		checkedAt: now,
		expireAt:  now.Add(ttl),
	})

	return inGroup
}

func IsUserInGroupFresh(bot *tgbotapi.BotAPI, userID int64, groupID int64) bool {
	if groupID == 0 {
		return true
	}

	cacheKey := fmt.Sprintf("%d:%d", groupID, userID)
	// Business gates must observe a leave immediately. The ordinary membership
	// cache is still updated for callers that do not require a live decision.
	member, err := bot.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: groupID,
			UserID: userID,
		},
	})
	if err != nil {
		log.Printf("⚠️ 实时群成员校验失败: user=%d group=%d err=%s", userID, groupID, textfmt.FormatTelegramSendError(err))
		now := time.Now()
		groupMemberCache.Store(cacheKey, &groupMemberCacheEntry{
			inGroup:   false,
			checkedAt: now,
			expireAt:  now.Add(groupMemberNegativeTTL),
		})
		return false
	}

	inGroup := member.Status == "member" ||
		member.Status == "creator" ||
		member.Status == "administrator" ||
		(member.Status == "restricted" && member.IsMember)

	now := time.Now()
	groupMemberCache.Store(cacheKey, &groupMemberCacheEntry{
		inGroup:   inGroup,
		checkedAt: now,
		expireAt:  now.Add(groupMemberFreshTTL),
	})

	return inGroup
}

func IsMessageFromNoticeGroup(msg *tgbotapi.Message) bool {
	return config.AppConfig != nil &&
		config.AppConfig.NoticeGroupID != 0 &&
		msg != nil &&
		msg.Chat != nil &&
		msg.Chat.ID == config.AppConfig.NoticeGroupID
}

func RequireSuperAdmin(bot *tgbotapi.BotAPI, chatID int64, userID int64) bool {
	if !storage.IsSuperAdmin(userID) {
		ReplyText(bot, chatID, "❌ 权限不足：该操作仅限超级管理员。")
		return false
	}
	return true
}
