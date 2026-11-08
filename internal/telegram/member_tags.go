package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var ErrMemberTagAdministrator = errors.New("管理员保留原有头衔，称号仅在档案中佩戴")

// 当前 SDK 尚未提供成员标签，直接调用 Bot API。撤下时仅清理机器人
// 上次写入的标签，避免删除群管理员或成员后来手动设置的其他标签。
func SyncRegularMemberTag(bot *tgbotapi.BotAPI, chatID, userID int64, desired string, previous ...string) error {
	if bot == nil || chatID >= 0 || userID <= 0 || utf8.RuneCountInString(desired) > 16 {
		return fmt.Errorf("MEMBER_TAG_ARGUMENT_INVALID")
	}
	params := tgbotapi.Params{"chat_id": strconv.FormatInt(chatID, 10), "user_id": strconv.FormatInt(userID, 10)}
	response, err := bot.MakeRequest("getChatMember", params)
	if err != nil {
		return err
	}
	if response == nil || !response.Ok {
		return fmt.Errorf("MEMBER_TAG_LOOKUP_FAILED")
	}
	var member struct {
		Status   string `json:"status"`
		Tag      string `json:"tag"`
		IsMember bool   `json:"is_member"`
	}
	if err := json.Unmarshal(response.Result, &member); err != nil {
		return err
	}
	if member.Status == "creator" || member.Status == "administrator" {
		return ErrMemberTagAdministrator
	}
	if member.Status != "member" && !(member.Status == "restricted" && member.IsMember) {
		return fmt.Errorf("MEMBER_TAG_NOT_IN_GROUP")
	}
	if desired == "" {
		owned := false
		for _, tag := range previous {
			if tag != "" && member.Tag == tag {
				owned = true
			}
		}
		if !owned {
			return nil
		}
	}
	if member.Tag == desired {
		return nil
	}
	params["tag"] = desired
	response, err = bot.MakeRequest("setChatMemberTag", params)
	if err != nil {
		return err
	}
	if response == nil || !response.Ok {
		return fmt.Errorf("MEMBER_TAG_UPDATE_FAILED")
	}
	return nil
}
