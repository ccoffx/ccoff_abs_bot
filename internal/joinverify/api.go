package joinverify

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
)

type API interface {
	MakeRequest(string, tgbotapi.Params) (*tgbotapi.APIResponse, error)
}

// Use current Bot API permissions directly: the project's Telegram SDK predates
// separate media permissions. Omitting them would silently lose restrictions.
type permissions struct {
	SendMessages       bool `json:"can_send_messages"`
	SendAudios         bool `json:"can_send_audios"`
	SendDocuments      bool `json:"can_send_documents"`
	SendPhotos         bool `json:"can_send_photos"`
	SendVideos         bool `json:"can_send_videos"`
	SendVideoNotes     bool `json:"can_send_video_notes"`
	SendVoiceNotes     bool `json:"can_send_voice_notes"`
	SendPolls          bool `json:"can_send_polls"`
	SendOtherMessages  bool `json:"can_send_other_messages"`
	AddWebPagePreviews bool `json:"can_add_web_page_previews"`
	ChangeInfo         bool `json:"can_change_info"`
	InviteUsers        bool `json:"can_invite_users"`
	PinMessages        bool `json:"can_pin_messages"`
	ManageTopics       bool `json:"can_manage_topics"`
}

type member struct {
	permissions
	Status             string `json:"status"`
	IsMember           bool   `json:"is_member"`
	UntilDate          int64  `json:"until_date"`
	CanRestrictMembers bool   `json:"can_restrict_members"`
	CanDeleteMessages  bool   `json:"can_delete_messages"`
}

func (m member) present() bool {
	return m.Status == "member" || m.admin() || (m.Status == "restricted" && m.IsMember)
}
func (m member) admin() bool { return m.Status == "creator" || m.Status == "administrator" }

type chat struct {
	Type        string      `json:"type"`
	Permissions permissions `json:"permissions"`
}

func (s *Service) call(method string, params tgbotapi.Params, out any) error {
	resp, err := s.api.MakeRequest(method, params)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if resp == nil || !resp.Ok {
		return fmt.Errorf("%s: unsuccessful response", method)
	}
	if out != nil {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}
func params(chatID, userID int64) tgbotapi.Params {
	p := tgbotapi.Params{"chat_id": strconv.FormatInt(chatID, 10)}
	if userID != 0 {
		p["user_id"] = strconv.FormatInt(userID, 10)
	}
	return p
}
func (s *Service) getMember(chatID, userID int64) (member, error) {
	var m member
	err := s.call("getChatMember", params(chatID, userID), &m)
	return m, err
}
func (s *Service) restrict(userID int64, p permissions, until int64) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	q := params(s.groupID, userID)
	q["permissions"] = string(data)
	q["use_independent_chat_permissions"] = "true"
	q["until_date"] = strconv.FormatInt(until, 10)
	return s.call("restrictChatMember", q, nil)
}
func (s *Service) Validate() error {
	var g chat
	if err := s.call("getChat", params(s.groupID, 0), &g); err != nil {
		return err
	}
	if g.Type != "supergroup" {
		return fmt.Errorf("JOIN_VERIFY requires a supergroup")
	}
	m, err := s.getMember(s.groupID, s.botID)
	if err != nil {
		return err
	}
	if !m.admin() || (m.Status != "creator" && (!m.CanRestrictMembers || !m.CanDeleteMessages)) {
		return fmt.Errorf("JOIN_VERIFY Bot requires group administrator, restrict/ban and delete-message permissions")
	}
	var c chat
	if err := s.call("getChat", params(s.channelID, 0), &c); err != nil {
		return err
	}
	if c.Type != "channel" {
		return fmt.Errorf("JOIN_VERIFY_CHANNEL_ID must identify a channel")
	}
	m, err = s.getMember(s.channelID, s.botID)
	if err != nil {
		return err
	}
	if !m.admin() {
		return fmt.Errorf("JOIN_VERIFY Bot must be a channel administrator")
	}
	var pending int64
	if err := s.db.Model(&model.JoinVerification{}).Where("state IN ? AND (group_id <> ? OR channel_id <> ?)", strings.Split(activeStates, ","), s.groupID, s.channelID).Count(&pending).Error; err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("JOIN_VERIFY has unfinished rounds for another group/channel; restore the previous configuration and settle them first")
	}
	return nil
}
