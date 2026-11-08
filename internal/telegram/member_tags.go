package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var (
	ErrMemberTitleOwner           = errors.New("Telegram 不允许机器人修改群主头衔，请在群设置中手动设置为当前称号")
	ErrMemberTitleAdminUneditable = errors.New("机器人无权修改这位管理员的头衔，请在群设置中手动设置为当前称号")
)

// 普通成员使用群标签，管理员使用头衔；不改变管理员权限。
// 撤下时仅清理机器人曾写入的称号，保留后来手动设置的其他内容。
// 返回本次处理的 Telegram 字段；beforeWrite 在实际写入前持久化该字段的意图。
func SyncMemberTitle(bot *tgbotapi.BotAPI, chatID, userID int64, desired string, previous map[string][]string, beforeWrite func(field string) error) (string, error) {
	if bot == nil || chatID >= 0 || userID <= 0 || utf8.RuneCountInString(desired) > 16 {
		return "", fmt.Errorf("MEMBER_TAG_ARGUMENT_INVALID")
	}
	params := tgbotapi.Params{"chat_id": strconv.FormatInt(chatID, 10), "user_id": strconv.FormatInt(userID, 10)}
	response, err := bot.MakeRequest("getChatMember", params)
	if err != nil {
		return "", err
	}
	if response == nil || !response.Ok {
		return "", fmt.Errorf("MEMBER_TAG_LOOKUP_FAILED")
	}
	var member struct {
		Status      string `json:"status"`
		Tag         string `json:"tag"`
		CustomTitle string `json:"custom_title"`
		CanBeEdited bool   `json:"can_be_edited"`
		IsMember    bool   `json:"is_member"`
	}
	if err := json.Unmarshal(response.Result, &member); err != nil {
		return "", err
	}
	method, field, current := "setChatMemberTag", "tag", member.Tag
	switch member.Status {
	case "creator", "administrator":
		method, field, current = "setChatAdministratorCustomTitle", "custom_title", member.CustomTitle
	case "member":
	case "restricted":
		if !member.IsMember {
			return "", fmt.Errorf("MEMBER_TAG_NOT_IN_GROUP")
		}
	default:
		return "", fmt.Errorf("MEMBER_TAG_NOT_IN_GROUP")
	}
	if desired == "" {
		owned := false
		for _, tag := range previous[field] {
			if tag != "" && current == tag {
				owned = true
			}
		}
		if !owned {
			return field, nil
		}
	}
	if current == desired {
		return field, nil
	}
	if member.Status == "creator" {
		return field, ErrMemberTitleOwner
	}
	if member.Status == "administrator" && !member.CanBeEdited {
		return field, ErrMemberTitleAdminUneditable
	}
	// Resolve the current role before persisting the intent. A failed local
	// write must prevent the external update, whose result may be uncertain.
	if beforeWrite != nil {
		if err := beforeWrite(field); err != nil {
			return field, err
		}
	}
	params[field] = desired
	response, err = bot.MakeRequest(method, params)
	if err != nil {
		return field, err
	}
	if response == nil || !response.Ok {
		return field, fmt.Errorf("MEMBER_TAG_UPDATE_FAILED")
	}
	return field, nil
}
