// Package joinverify owns durable channel-follow verification. All Telegram
// side effects have a persisted intent and can be retried after a restart.
package joinverify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const CallbackPrefix = "joinverify:"
const continuePayload = "jv_continue"
const Timeout = 120 * time.Second
const sharedPromptEditInterval = 30 * time.Second
const sharedPromptIdleGrace = time.Minute
const sharedPromptMaxAge = time.Hour
const sharedPromptRetireRetryInterval = 30 * time.Second
const activeStates = "preparing,pending,restoring,kicking,unbanning"
const accessHint = "⚠️ 使用本服务需要加入官方群、关注指定频道，并在 Bot 私聊中完成验证。"

type Service struct {
	db                        *gorm.DB
	api                       API
	groupID, channelID, botID int64
	channelURL                string
	botUsername               string
	admins                    []int64
	locks                     [64]sync.Mutex
	promptMu                  sync.Mutex
	pendingMessageID          atomic.Int64
	unpersistedPromptID       int
	unpersistedPromptCount    int64
	unpersistedPromptSentAt   time.Time
	wake                      chan struct{}
	now                       func() time.Time
}

func New(db *gorm.DB, api API, groupID, channelID, botID int64, channelURL string, admins []int64, botUsername string) *Service {
	return &Service{db: db, api: api, groupID: groupID, channelID: channelID, botID: botID, channelURL: channelURL, botUsername: botUsername, admins: admins, wake: make(chan struct{}, 1), now: time.Now}
}
func (s *Service) lock(userID int64) func() {
	m := &s.locks[uint64(userID)%uint64(len(s.locks))]
	m.Lock()
	return m.Unlock
}
func active(state string) bool {
	for _, v := range strings.Split(activeStates, ",") {
		if v == state {
			return true
		}
	}
	return false
}
func (s *Service) record(userID int64) (model.JoinVerification, error) {
	var r model.JoinVerification
	err := s.db.Where("group_id = ? AND user_id = ?", s.groupID, userID).First(&r).Error
	return r, err
}
func (s *Service) save(r *model.JoinVerification) error { return s.db.Save(r).Error }
func (s *Service) audit(r *model.JoinVerification, action, detail string) {
	if err := storage.WriteAuditLogInTx(s.db, 0, "join_verify_"+action, strconv.FormatInt(r.UserID, 10), 0, fmt.Sprintf("group=%d round=%s %s", r.GroupID, r.Round, detail)); err != nil {
		log.Printf("join verification audit failed: %s", textfmt.FormatPlainError(err))
	}
}
func (s *Service) failure(r *model.JoinVerification, err error) {
	if err == nil {
		return
	}
	log.Printf("join verification failed: user=%d state=%s err=%s", r.UserID, r.State, textfmt.FormatTelegramSendError(err))
	// Store a redacted error; do not store bot tokens or private invite links.
	r.LastError = textfmt.FormatTelegramSendError(err)
	notify := r.NotifiedAt.IsZero() || s.now().Sub(r.NotifiedAt) >= 10*time.Minute
	if notify {
		r.NotifiedAt = s.now()
	}
	if saveErr := s.save(r); saveErr != nil {
		log.Printf("join verification failure persistence: %s", textfmt.FormatPlainError(saveErr))
	}
	if notify {
		s.audit(r, "failure", r.LastError)
		for _, id := range s.admins {
			q := params(id, 0)
			q["text"] = fmt.Sprintf("⚠️ 入群验证操作失败：用户 %d，状态 %s。系统将重试；请检查群与频道管理权限。", r.UserID, r.State)
			if e := s.call("sendMessage", q, nil); e != nil {
				log.Printf("join verification admin notification failed: %s", textfmt.FormatTelegramSendError(e))
			}
		}
	}
}

// Enqueue must finish before the polling loop acknowledges this update. Only
// database I/O happens here; slow Telegram calls run in the service worker.
func (s *Service) Enqueue(update tgbotapi.Update) error {
	e := update.ChatMember
	if e == nil || (e.Chat.ID != s.groupID && e.Chat.ID != s.channelID) || e.NewChatMember.User == nil {
		return nil
	}
	if e.From.ID == s.botID && e.Chat.ID == s.groupID {
		return nil
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	row := model.JoinVerificationEvent{UpdateID: update.UpdateID, Payload: string(data), CreatedAt: s.now()}
	if err = s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		s.Sweep()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}
func (s *Service) Sweep() {
	// Events remain queued until handled successfully. Preserve Telegram ordering.
	var events []model.JoinVerificationEvent
	if err := s.db.Where("done = ?", false).Order("update_id asc").Limit(100).Find(&events).Error; err != nil {
		log.Printf("join verification event read: %s", textfmt.FormatPlainError(err))
		return
	}
	blocked := map[int64]bool{}
	for _, row := range events {
		var e tgbotapi.ChatMemberUpdated
		if err := json.Unmarshal([]byte(row.Payload), &e); err != nil {
			log.Printf("join verification invalid event: %v", err)
			return
		}
		if e.NewChatMember.User == nil {
			continue
		}
		id := e.NewChatMember.User.ID
		if blocked[id] {
			continue
		}
		err := s.handleEvent(e)
		if err == nil && e.Chat.ID == s.groupID && !e.NewChatMember.User.IsBot &&
			!sdkPresent(e.OldChatMember) && sdkPresent(e.NewChatMember) {
			err = s.requestSharedPromptRefresh()
		}
		if err != nil {
			blocked[id] = true
			func() {
				unlock := s.lock(id)
				defer unlock()
				r, readErr := s.record(id)
				if readErr != nil && !errors.Is(readErr, gorm.ErrRecordNotFound) {
					return
				}
				if r.ID == 0 {
					r = model.JoinVerification{GroupID: s.groupID, UserID: id, ChannelID: s.channelID, State: "event_retry"}
				}
				s.failure(&r, err)
			}()
			continue
		}
		if err := s.db.Model(&row).Update("done", true).Error; err != nil {
			return
		}
	}
	_ = s.db.Where("done = ? AND created_at < ?", true, s.now().Add(-48*time.Hour)).Delete(&model.JoinVerificationEvent{}).Error
	var records []model.JoinVerification
	if err := s.db.Where("group_id = ? AND state IN ?", s.groupID, strings.Split(activeStates, ",")).Find(&records).Error; err != nil {
		log.Printf("join verification records read: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, r := range records {
		if blocked[r.UserID] {
			continue
		}
		func() {
			unlock := s.lock(r.UserID)
			defer unlock()
			latest, err := s.record(r.UserID)
			if err != nil {
				return
			}
			if err = s.advance(&latest); err != nil {
				s.failure(&latest, err)
			}
		}()
	}
	// Cleaning prompts is independent of the verification result and retried.
	var cleanup []model.JoinVerification
	if err := s.db.Where("group_id = ? AND private_message_id <> 0 AND state NOT IN ?", s.groupID, strings.Split(activeStates, ",")).Limit(100).Find(&cleanup).Error; err == nil {
		for _, r := range cleanup {
			func() {
				unlock := s.lock(r.UserID)
				defer unlock()
				v, e := s.record(r.UserID)
				if e == nil && !active(v.State) {
					s.cleanup(&v)
				}
			}()
		}
	}
	if err := s.syncSharedPrompt(); err != nil {
		log.Printf("join verification shared prompt: %s", textfmt.FormatTelegramSendError(err))
	}
}
func sdkPresent(m tgbotapi.ChatMember) bool {
	return m.Status == "member" || m.Status == "creator" || m.Status == "administrator" || (m.Status == "restricted" && m.IsMember)
}
func (s *Service) handleEvent(e tgbotapi.ChatMemberUpdated) error {
	if e.NewChatMember.User == nil {
		return nil
	}
	id := e.NewChatMember.User.ID
	unlock := s.lock(id)
	defer unlock()
	r, err := s.record(id)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if e.Chat.ID == s.groupID {
		was, now := sdkPresent(e.OldChatMember), sdkPresent(e.NewChatMember)
		if !now {
			// A human ban is never automatically undone, including during a kick retry.
			if active(r.State) {
				return s.finish(&r, "left", "member left or was removed by an administrator")
			}
			return nil
		}
		if was {
			if active(r.State) && e.From.ID != s.botID && int64(e.Date) >= r.StartedAt.Unix() {
				if err := s.finish(&r, "manual", "administrator changed member permissions; automatic actions stopped"); err != nil {
					return err
				}
				for _, admin := range s.admins {
					q := params(admin, 0)
					q["text"] = fmt.Sprintf("ℹ️ 用户 %d 的群权限被人工修改，本轮入群验证已停止自动恢复及移出，请人工处理。", id)
					_ = s.call("sendMessage", q, nil)
				}
			}
			return nil
		}
		if r.State == "manual" || r.State == "unbanning" || r.State == "kicking" {
			if err = s.finish(&r, "left", "new membership starts a new verification"); err != nil {
				return err
			}
		}
		_, err = s.ensure(id, "join")
		return err
	}
	was, now := sdkPresent(e.OldChatMember), sdkPresent(e.NewChatMember)
	if was && !now {
		if active(r.State) {
			r.Confirmed = false
			if err = s.save(&r); err != nil {
				return err
			}
		}
		_, err = s.ensure(id, "unsubscribe")
		return err
	}
	if now && active(r.State) {
		return s.advance(&r)
	}
	return nil
}

// Check is shared by message and callback business gates. Query errors fail
// closed for business access, but never create a punitive verification round.
func (s *Service) Check(userID int64) (bool, string) {
	unlock := s.lock(userID)
	defer unlock()
	ok, err := s.ensure(userID, "business")
	if err != nil {
		return false, "⚠️ 关注状态暂时无法确认，请稍后重试。"
	}
	if !ok {
		return false, accessHint
	}
	return true, ""
}

// AccessMessage gives blocked messages and historic menu callbacks the same
// actionable entry. It never creates a round or extends its deadline.
func (s *Service) AccessMessage(chatID, userID int64, hint string) tgbotapi.MessageConfig {
	msg := tgbotapi.NewMessage(chatID, hint)
	if hint != accessHint {
		return msg
	}
	unlock := s.lock(userID)
	defer unlock()
	r, err := s.record(userID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		msg.Text = "⚠️ 验证状态暂时无法读取，请稍后重试。"
		return msg
	}
	if r.State == "pending" && s.now().Before(r.Deadline) {
		msg.Text = fmt.Sprintf("⚠️ 请点击「去 Bot 验证」，在私聊中关注频道并完成验证。\n剩余 %d 秒，重新打开入口不会延长期限。", s.remaining(&r))
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("去 Bot 验证", "https://t.me/"+s.botUsername+"?start=jv_"+r.Round)))
		return msg
	}
	if active(r.State) {
		msg.Text = "⚠️ 本轮验证正在处理或已超时，请稍后重试；被移出群后可重新加入。"
		return msg
	}
	if r.State == "manual" {
		msg.Text = "⚠️ 群权限已由管理员调整，请联系管理员处理验证。"
		return msg
	}
	var group struct {
		Username   string `json:"username"`
		InviteLink string `json:"invite_link"`
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	if err := s.call("getChat", params(s.groupID, 0), &group); err == nil {
		url := group.InviteLink
		if group.Username != "" {
			url = "https://t.me/" + group.Username
		}
		if url != "" {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonURL("加入官方群", url)))
		}
	}
	msg.Text = "⚠️ 请先加入官方群、关注频道，再点击「已入群，继续验证」；若没有入群链接，请联系管理员获取。"
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonURL("前往关注频道", s.channelURL)))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonURL("已入群，继续验证", "https://t.me/"+s.botUsername+"?start="+continuePayload)))
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	return msg
}
func (s *Service) ensure(userID int64, reason string) (bool, error) {
	r, err := s.record(userID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	g, err := s.getMember(s.groupID, userID)
	if err != nil {
		return false, err
	}
	if g.admin() {
		if active(r.State) {
			err = s.finish(&r, "passed", "group administrator exempt")
		}
		return true, err
	}
	if !g.present() {
		// Kick/unban intents must remain durable until their worker settles them.
		if r.State == "preparing" || r.State == "pending" || r.State == "restoring" {
			return false, s.finish(&r, "left", "member left")
		}
		return false, nil
	}
	if r.State == "manual" {
		c, err := s.getMember(s.channelID, userID)
		return c.present(), err
	}
	if active(r.State) {
		if err = s.advance(&r); err != nil {
			s.failure(&r, err)
			return false, err
		}
		return r.State == "passed", nil
	}
	c, err := s.getMember(s.channelID, userID)
	if err != nil {
		return false, err
	}
	// Telegram can preserve our indefinite restriction after a member leaves
	// and rejoins. Reuse the durable pre-mute snapshot instead of treating our
	// own mute as a moderator's original restriction in the next round.
	carriedMute := r.Muted && (r.State == "left" || r.State == "expired") &&
		g.Status == "restricted" && g.permissions == (permissions{}) && g.UntilDate == 0
	if c.present() && !carriedMute {
		return true, nil
	}
	token := make([]byte, 12)
	if _, err = rand.Read(token); err != nil {
		return false, err
	}
	original := ""
	if g.Status == "restricted" {
		data, e := json.Marshal(g.permissions)
		if e != nil {
			return false, e
		}
		original = string(data)
	}
	originalUntil := g.UntilDate
	if carriedMute {
		original, originalUntil = r.OriginalPermissions, r.OriginalUntil
	}
	if r.PrivateMessageID != 0 {
		// A failed panel cleanup must not prevent a new verification round.
		cleanup := model.AutoDeleteMsg{ChatID: userID, MessageID: r.PrivateMessageID, DeleteAt: s.now()}
		if err := s.db.Create(&cleanup).Error; err != nil {
			return false, err
		}
	}
	r.PrivateMessageID = 0
	// An already-subscribed rejoin was exempt before any new verification round
	// began. Restore only the known old Bot mute without requiring a new click.
	r.Confirmed = carriedMute && c.present()
	r.GroupID = s.groupID
	r.UserID = userID
	r.ChannelID = s.channelID
	r.Round = hex.EncodeToString(token)
	r.Reason = reason
	r.State = "preparing"
	r.StartedAt = s.now()
	r.Deadline = s.now().Add(Timeout)
	r.OriginalPermissions = original
	r.OriginalUntil = originalUntil
	r.Muted = false
	r.RestorePermissions = ""
	r.RestoreUntil = 0
	r.LastError = ""
	r.NotifiedAt = time.Time{}
	if err = s.save(&r); err != nil {
		return false, err
	}
	s.audit(&r, "start", reason)
	err = s.advance(&r)
	if err != nil {
		s.failure(&r, err)
	}
	return r.State == "passed", err
}
func (s *Service) advance(r *model.JoinVerification) error {
	if !active(r.State) {
		return nil
	}
	// A config change must not cause an old round to act against a new channel.
	if r.ChannelID != s.channelID {
		return fmt.Errorf("pending verification belongs to another channel; restore original configuration to settle it")
	}
	if r.State == "unbanning" {
		return s.unban(r)
	}
	g, err := s.getMember(s.groupID, r.UserID)
	if err != nil {
		return err
	}
	if g.admin() {
		return s.finish(r, "passed", "group administrator exempt")
	}
	if r.State == "kicking" {
		if g.Status == "kicked" {
			r.State = "unbanning"
			if err = s.save(r); err != nil {
				return err
			}
			return s.unban(r)
		}
		if !g.present() {
			return s.finish(r, "left", "member left")
		}
	}
	if !g.present() {
		return s.finish(r, "left", "member left")
	}
	c, err := s.getMember(s.channelID, r.UserID)
	if err != nil {
		return err
	}
	if c.present() && r.Confirmed {
		if r.State == "preparing" && g.Status == "restricted" && g.permissions == (permissions{}) && g.UntilDate == 0 {
			// A previous restrict request may have succeeded with its response lost.
			r.Muted = true
			if err = s.save(r); err != nil {
				return err
			}
		}
		return s.restore(r, g)
	}
	if !c.present() && r.Confirmed {
		r.Confirmed = false
		if err = s.save(r); err != nil {
			return err
		}
	}
	if r.State == "restoring" {
		// Confirmation was revoked. Retain the
		// original snapshot and deadline, and reapply verification restrictions.
		r.State = "preparing"
		r.Muted = false
		if err = s.save(r); err != nil {
			return err
		}
	}
	// If a moderator changed permissions and we missed the event, stop too.
	if r.Muted && (g.Status != "restricted" || g.permissions != (permissions{}) || g.UntilDate != 0) {
		return s.manual(r, "member restrictions changed externally; manual review required")
	}
	if r.State == "preparing" {
		originalMatches := g.Status == "member" && r.OriginalPermissions == ""
		if r.OriginalPermissions != "" {
			var original permissions
			if err := json.Unmarshal([]byte(r.OriginalPermissions), &original); err != nil {
				return err
			}
			originalMatches = g.Status == "restricted" && g.permissions == original && g.UntilDate == r.OriginalUntil
		}
		if r.RestorePermissions != "" {
			var restored permissions
			if err := json.Unmarshal([]byte(r.RestorePermissions), &restored); err != nil {
				return err
			}
			originalMatches = originalMatches || (g.Status == "restricted" && g.permissions == restored && g.UntilDate == r.RestoreUntil)
		}
		muted := g.Status == "restricted" && g.permissions == (permissions{}) && g.UntilDate == 0
		if !originalMatches && !muted {
			return s.manual(r, "permissions changed before verification restriction could be applied")
		}
		// Persisted before this call. Restricting twice after an uncertain response is safe.
		if err = s.restrict(r.UserID, permissions{}, 0); err != nil {
			return err
		}
		r.Muted = true
		r.State = "pending"
		if err = s.save(r); err != nil {
			return err
		}
	}
	if !s.now().Before(r.Deadline) {
		// Channel and membership were freshly checked above, including on restart.
		r.State = "kicking"
		if err = s.save(r); err != nil {
			return err
		}
		if err = s.call("banChatMember", params(s.groupID, r.UserID), nil); err != nil {
			return err
		}
		r.State = "unbanning"
		if err = s.save(r); err != nil {
			return err
		}
		return s.unban(r)
	}
	return nil
}
func (s *Service) restore(r *model.JoinVerification, g member) error {
	if !r.Muted && r.State != "restoring" {
		return s.finish(r, "passed", "channel subscription confirmed")
	}
	muted := g.Status == "restricted" && g.permissions == (permissions{}) && g.UntilDate == 0
	if !muted {
		if r.State == "restoring" {
			var target permissions
			if err := json.Unmarshal([]byte(r.RestorePermissions), &target); err != nil {
				return err
			}
			if (g.Status == "member" && r.OriginalPermissions == "") || (g.Status == "restricted" && g.permissions == target && g.UntilDate == r.RestoreUntil) {
				return s.finish(r, "passed", "previous permission restore confirmed")
			}
		}
		return s.manual(r, "member restrictions changed externally; manual review required")
	}
	// Telegram treats restrictions expiring in under 30 seconds as indefinite.
	// Wait for an almost-expired original restriction rather than extending it.
	if r.OriginalPermissions != "" && r.OriginalUntil > s.now().Unix() && r.OriginalUntil-s.now().Unix() < 30 {
		return nil
	}
	var group chat
	if err := s.call("getChat", params(s.groupID, 0), &group); err != nil {
		return err
	}
	p := group.Permissions
	until := int64(0)
	if r.OriginalPermissions != "" && (r.OriginalUntil == 0 || r.OriginalUntil > s.now().Unix()) {
		var original permissions
		if err := json.Unmarshal([]byte(r.OriginalPermissions), &original); err != nil {
			return err
		}
		// Intersection preserves both the old member restriction and current defaults.
		a, _ := json.Marshal(p)
		b, _ := json.Marshal(original)
		var defaults, old map[string]bool
		if err := json.Unmarshal(a, &defaults); err != nil {
			return err
		}
		if err := json.Unmarshal(b, &old); err != nil {
			return err
		}
		for key, value := range defaults {
			old[key] = value && old[key]
		}
		data, _ := json.Marshal(old)
		if err := json.Unmarshal(data, &p); err != nil {
			return err
		}
		until = r.OriginalUntil
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	r.State = "restoring"
	r.RestorePermissions = string(data)
	r.RestoreUntil = until
	if err = s.save(r); err != nil {
		return err
	}
	if err := s.restrict(r.UserID, p, until); err != nil {
		return err
	}
	return s.finish(r, "passed", "channel subscription confirmed")
}
func (s *Service) unban(r *model.JoinVerification) error {
	q := params(s.groupID, r.UserID)
	q["only_if_banned"] = "true"
	if err := s.call("unbanChatMember", q, nil); err != nil {
		return err
	}
	return s.finish(r, "expired", "verification timed out; removed and allowed to rejoin")
}
func (s *Service) manual(r *model.JoinVerification, detail string) error {
	if err := s.finish(r, "manual", detail); err != nil {
		return err
	}
	for _, id := range s.admins {
		q := params(id, 0)
		q["text"] = fmt.Sprintf("ℹ️ 用户 %d 的群权限发生变化，本轮验证已停止自动恢复及移出，请人工处理。", r.UserID)
		_ = s.call("sendMessage", q, nil)
	}
	return nil
}
func (s *Service) finish(r *model.JoinVerification, state, detail string) error {
	r.State = state
	if state == "manual" {
		// A moderator now owns these restrictions; never reuse our old mute evidence.
		r.Muted = false
	}
	r.LastError = ""
	if err := s.save(r); err != nil {
		return err
	}
	s.audit(r, state, detail)
	s.cleanup(r)
	return nil
}

// IsStart also recognizes malformed verification payloads so they never fall
// through to the ordinary menu or business sessions.
func IsStart(text string) bool {
	if strings.TrimSpace(text) == "入群验证" {
		return true
	}
	parts := strings.Fields(text)
	return len(parts) >= 2 && strings.Split(parts[0], "@")[0] == "/start" && strings.HasPrefix(parts[1], "jv_")
}
func messageMissing(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "message to edit not found")
}
func messageUnchanged(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "message is not modified")
}
func (s *Service) reason(r *model.JoinVerification) string {
	switch r.Reason {
	case "join":
		return "请完成频道关注验证。"
	case "unsubscribe":
		return "检测到你已取消关注指定频道，群内发言已暂停，请重新关注并完成验证。"
	default:
		return "使用本群服务需要关注指定频道，请完成验证。"
	}
}
func (s *Service) remaining(r *model.JoinVerification) int {
	n := int(r.Deadline.Sub(s.now()).Seconds())
	if n < 0 {
		return 0
	}
	return n
}
func (s *Service) syncSharedPrompt() error {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	if err := s.flushGroupActivity(); err != nil {
		return err
	}
	now := s.now()
	var count int64
	if err := s.db.Model(&model.JoinVerification{}).
		Where("group_id = ? AND state IN ? AND deadline > ?", s.groupID, []string{"preparing", "pending", "restoring"}, now).
		Count(&count).Error; err != nil {
		return err
	}
	var prompt model.JoinVerificationPrompt
	err := s.db.First(&prompt, "group_id = ?", s.groupID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if prompt.GroupID == 0 {
		prompt.GroupID = s.groupID
	}
	if count == 0 && prompt.RefreshRequested {
		prompt.RefreshRequested = false
		if err := s.db.Save(&prompt).Error; err != nil {
			return err
		}
	}
	if prompt.MessageID == 0 && prompt.SendState != "" {
		if s.unpersistedPromptID == 0 {
			// Telegram has no sendMessage idempotency key or message-history API.
			// An unknown result must not trigger another group message.
			if prompt.SendState == "sending" {
				prompt.SendState = "uncertain"
				if err := s.db.Save(&prompt).Error; err != nil {
					return err
				}
				log.Printf("join verification group prompt send outcome is uncertain after restart: group=%d", s.groupID)
				return nil
			}
			return nil
		}
		prompt.MessageID = s.unpersistedPromptID
		prompt.MessageCreatedAt = s.unpersistedPromptSentAt
		prompt.PendingCount = s.unpersistedPromptCount
		prompt.LastEditedAt = s.unpersistedPromptSentAt
		prompt.SendState = ""
		prompt.SendAttemptedAt = time.Time{}
		if err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: prompt.MessageID, Chat: &tgbotapi.Chat{ID: s.groupID}}); err != nil {
			return err
		}
		if err := s.db.Save(&prompt).Error; err != nil {
			return err
		}
		s.unpersistedPromptID = 0
	}
	if count == 0 && prompt.MessageID == 0 {
		return nil
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonURL("去 Bot 验证", "https://t.me/"+s.botUsername+"?start="+continuePayload)))
	data, err := json.Marshal(keyboard)
	if err != nil {
		return err
	}
	q := params(s.groupID, 0)
	q["reply_markup"] = string(data)
	old := prompt.MessageID != 0 && (prompt.MessageCreatedAt.IsZero() || now.Sub(prompt.MessageCreatedAt) >= sharedPromptMaxAge)
	if count == 0 {
		if prompt.IdleSince.IsZero() {
			prompt.IdleSince = now
			if err := s.db.Save(&prompt).Error; err != nil {
				return err
			}
		}
		if old || now.Sub(prompt.IdleSince) >= sharedPromptIdleGrace {
			return s.retireSharedPrompt(&prompt, now)
		}
		if prompt.PendingCount == 0 {
			return nil
		}
		q["message_id"] = strconv.Itoa(prompt.MessageID)
		q["text"] = "⚠️ 当前暂无待验证成员。新成员可点击下方按钮，在 Bot 私聊完成入群验证。"
		if err := s.call("editMessageText", q, nil); err != nil && !messageUnchanged(err) {
			if !messageMissing(err) {
				return err
			}
			return s.clearSharedPrompt(&prompt)
		}
		prompt.PendingCount = 0
		prompt.LastEditedAt = now
		return s.db.Save(&prompt).Error
	}
	refresh := false
	if prompt.RefreshRequested && prompt.MessageID != 0 {
		var activity model.JoinVerificationGroupActivity
		err := s.db.First(&activity, "group_id = ?", s.groupID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		// No history (e.g. the first join after upgrading): refresh conservatively.
		refresh = activity.LastMessageID == 0 || activity.LastMessageID > prompt.MessageID
		if !refresh {
			prompt.RefreshRequested = false
			if err := s.db.Save(&prompt).Error; err != nil {
				return err
			}
		}
	}
	if old || refresh {
		if err := s.retireSharedPrompt(&prompt, now); err != nil {
			return err
		}
		if prompt.MessageID != 0 {
			return nil
		}
	}
	if prompt.MessageID != 0 && prompt.IdleSince.IsZero() && (prompt.PendingCount == count || now.Sub(prompt.LastEditedAt) < sharedPromptEditInterval) {
		return nil
	}
	q["text"] = fmt.Sprintf("⚠️ 当前有 %d 位成员待完成入群验证。请本人点击下方按钮，在 Bot 私聊关注频道并确认；各自的剩余时间以私聊面板为准。", count)
	if prompt.MessageID == 0 {
		return s.sendSharedPrompt(&prompt, q, count, now)
	}
	q["message_id"] = strconv.Itoa(prompt.MessageID)
	if err = s.call("editMessageText", q, nil); err != nil && !messageUnchanged(err) {
		if !messageMissing(err) {
			return err
		}
		if err := s.clearSharedPrompt(&prompt); err != nil {
			return err
		}
		delete(q, "message_id")
		return s.sendSharedPrompt(&prompt, q, count, now)
	}
	prompt.PendingCount = count
	prompt.IdleSince = time.Time{}
	prompt.LastEditedAt = now
	return s.db.Save(&prompt).Error
}

func (s *Service) sendSharedPrompt(prompt *model.JoinVerificationPrompt, q tgbotapi.Params, count int64, now time.Time) error {
	// Claim the send before calling Telegram. A lost response cannot be retried
	// safely because sendMessage has no idempotency key and bots cannot search
	// their own group message history by request ID.
	res := s.db.Model(&model.JoinVerificationPrompt{}).
		Where("group_id = ? AND message_id = 0 AND COALESCE(send_state, '') = ''", prompt.GroupID).
		Updates(map[string]any{"send_state": "sending", "send_attempted_at": now, "refresh_requested": false})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		fresh := model.JoinVerificationPrompt{GroupID: prompt.GroupID, SendState: "sending", SendAttemptedAt: now}
		res = s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&fresh)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
	}
	prompt.SendState = "sending"
	prompt.SendAttemptedAt = now
	// The claimed send fulfills the current refresh request, even if saving its
	// result later fails. A subsequent join can independently request a refresh.
	prompt.RefreshRequested = false
	var msg struct {
		MessageID int `json:"message_id"`
	}
	if err := s.call("sendMessage", q, &msg); err != nil {
		var apiErr *tgbotapi.Error
		if errors.As(err, &apiErr) {
			// Telegram explicitly rejected the request; it did not create a message.
			prompt.SendState = ""
			prompt.SendAttemptedAt = time.Time{}
		} else {
			prompt.SendState = "uncertain"
			err = fmt.Errorf("group prompt send outcome is uncertain; inspect the group before retrying: %w", err)
		}
		if saveErr := s.db.Save(prompt).Error; saveErr != nil {
			return saveErr
		}
		return err
	}
	if msg.MessageID == 0 {
		prompt.SendState = "uncertain"
		if err := s.db.Save(prompt).Error; err != nil {
			return err
		}
		return fmt.Errorf("sendMessage returned no message ID; group prompt status is uncertain")
	}
	s.unpersistedPromptID = msg.MessageID
	s.unpersistedPromptCount = count
	s.unpersistedPromptSentAt = now
	if err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: msg.MessageID, Chat: &tgbotapi.Chat{ID: s.groupID}}); err != nil {
		return err
	}
	prompt.MessageID = msg.MessageID
	prompt.MessageCreatedAt = now
	prompt.PendingCount = count
	prompt.IdleSince = time.Time{}
	prompt.LastEditedAt = now
	prompt.LastRetireAttemptAt = time.Time{}
	prompt.SendState = ""
	prompt.SendAttemptedAt = time.Time{}
	prompt.RefreshRequested = false
	if err := s.db.Save(prompt).Error; err != nil {
		return err
	}
	s.unpersistedPromptID = 0
	return nil
}

func (s *Service) clearSharedPrompt(prompt *model.JoinVerificationPrompt) error {
	prompt.MessageID = 0
	prompt.PendingCount = 0
	prompt.MessageCreatedAt = time.Time{}
	prompt.IdleSince = time.Time{}
	prompt.LastEditedAt = time.Time{}
	prompt.LastRetireAttemptAt = time.Time{}
	prompt.CloseByEdit = false
	prompt.SendState = ""
	prompt.SendAttemptedAt = time.Time{}
	return s.db.Save(prompt).Error
}

func (s *Service) retireSharedPrompt(prompt *model.JoinVerificationPrompt, now time.Time) error {
	if !prompt.LastRetireAttemptAt.IsZero() && now.Sub(prompt.LastRetireAttemptAt) < sharedPromptRetireRetryInterval {
		return nil
	}
	q := params(s.groupID, 0)
	q["message_id"] = strconv.Itoa(prompt.MessageID)
	if prompt.CloseByEdit {
		return s.closeSharedPromptByEdit(prompt, q, now)
	}
	err := s.call("deleteMessage", q, nil)
	if err != nil {
		message := strings.ToLower(err.Error())
		switch {
		case strings.Contains(message, "message to delete not found"):
		case strings.Contains(message, "message can't be deleted"):
			prompt.CloseByEdit = true
			return s.closeSharedPromptByEdit(prompt, q, now)
		default:
			prompt.LastRetireAttemptAt = now
			if saveErr := s.db.Save(prompt).Error; saveErr != nil {
				return saveErr
			}
			return err
		}
	}
	return s.clearSharedPrompt(prompt)
}

func (s *Service) closeSharedPromptByEdit(prompt *model.JoinVerificationPrompt, q tgbotapi.Params, now time.Time) error {
	q["text"] = "本条入群验证提醒已结束，请使用新的群提醒。"
	q["reply_markup"] = `{"inline_keyboard":[]}`
	if err := s.call("editMessageText", q, nil); err != nil && !messageMissing(err) && !messageUnchanged(err) {
		prompt.CloseByEdit = true
		prompt.LastRetireAttemptAt = now
		if saveErr := s.db.Save(prompt).Error; saveErr != nil {
			return saveErr
		}
		return err
	}
	return s.clearSharedPrompt(prompt)
}

// Start opens an owner-bound round, or rechecks membership from the reusable
// pre-join entry. Reopening an active round never resets its deadline.
func (s *Service) Start(msg *tgbotapi.Message) string {
	if msg == nil || msg.From == nil || msg.Chat == nil || !msg.Chat.IsPrivate() || msg.Chat.ID != msg.From.ID || !IsStart(msg.Text) {
		return "验证入口无效。"
	}
	unlock := s.lock(msg.From.ID)
	defer unlock()
	parts := strings.Fields(msg.Text)
	if strings.TrimSpace(msg.Text) == "入群验证" || (len(parts) == 2 && parts[1] == continuePayload) {
		ok, err := s.ensure(msg.From.ID, "business")
		if err != nil {
			return "验证状态暂时无法确认，请稍后重试。"
		}
		if ok {
			return "✅ 已确认入群及频道关注。若仍无法发言，请联系管理员检查原有群权限限制。"
		}
		r, err := s.record(msg.From.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && !active(r.State) && r.State != "manual") {
			return "请先加入官方群，再点击「已入群，继续验证」，或私聊发送「入群验证」。"
		}
		if err != nil {
			return "验证状态暂时无法读取，请稍后重试。"
		}
		if r.State == "manual" {
			return "群权限已由管理员调整，请联系管理员处理验证。"
		}
		return s.openPrivatePanel(&r)
	}
	r, err := s.record(msg.From.ID)
	if err != nil || len(parts) != 2 || parts[1] != "jv_"+r.Round {
		return "此验证仅限本人操作，或本轮验证已失效。"
	}
	if !active(r.State) {
		return "本轮验证已结束。"
	}
	return s.openPrivatePanel(&r)
}

func (s *Service) openPrivatePanel(r *model.JoinVerification) string {
	if !s.now().Before(r.Deadline) {
		return "本轮验证已超时，请等待处理；被移出后可重新加入。"
	}
	q := params(r.UserID, 0)
	q["text"] = fmt.Sprintf("%s\n剩余 %d 秒。关注后请点击「我已关注，验证」。超时将被移出群，可重新加入。", s.reason(r), s.remaining(r))
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonURL("前往关注频道", s.channelURL)),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("我已关注，验证", CallbackPrefix+r.Round)))
	data, _ := json.Marshal(keyboard)
	q["reply_markup"] = string(data)
	method := "sendMessage"
	if r.PrivateMessageID != 0 {
		method = "editMessageText"
		q["message_id"] = strconv.Itoa(r.PrivateMessageID)
	}
	var out struct {
		MessageID int `json:"message_id"`
	}
	err := s.call(method, q, &out)
	if method == "editMessageText" && messageMissing(err) {
		delete(q, "message_id")
		err = s.call("sendMessage", q, &out)
		if err == nil {
			r.PrivateMessageID = 0
		}
	}
	if err != nil && !messageUnchanged(err) {
		s.failure(r, err)
		return "验证面板暂时无法打开，请重新点击群内入口。"
	}
	if r.PrivateMessageID == 0 {
		r.PrivateMessageID = out.MessageID
	}
	if err = s.save(r); err != nil {
		return "验证面板暂时无法保存，请重新点击群内入口。"
	}
	return ""
}
func (s *Service) cleanup(r *model.JoinVerification) {
	if r.PrivateMessageID != 0 {
		q := params(r.UserID, 0)
		q["message_id"] = strconv.Itoa(r.PrivateMessageID)
		q["text"] = "本轮验证已结束。"
		if r.State == "passed" {
			q["text"] = "验证通过，已恢复验证前的群权限。"
		}
		if r.State == "expired" {
			q["text"] = "验证超时，已移出群，可重新加入。"
		}
		q["reply_markup"] = `{"inline_keyboard":[]}`
		err := s.call("editMessageText", q, nil)
		if err == nil || messageUnchanged(err) || strings.Contains(strings.ToLower(err.Error()), "message to edit not found") {
			r.PrivateMessageID = 0
			if err := s.save(r); err != nil {
				log.Printf("private verification cleanup persistence: %s", textfmt.FormatPlainError(err))
			}
		} else {
			log.Printf("private verification cleanup: %s", textfmt.FormatTelegramSendError(err))
		}
	}
}
func (s *Service) Callback(cb *tgbotapi.CallbackQuery) string {
	if cb == nil || cb.From == nil || cb.Message == nil || cb.Message.Chat == nil || (!cb.Message.Chat.IsPrivate() || cb.Message.Chat.ID != cb.From.ID) {
		return "验证消息无效。"
	}
	unlock := s.lock(cb.From.ID)
	defer unlock()
	r, err := s.record(cb.From.ID)
	if err != nil || !strings.HasPrefix(cb.Data, CallbackPrefix) || r.Round != strings.TrimPrefix(cb.Data, CallbackPrefix) {
		return "此验证仅限本人操作，或本轮验证已失效。"
	}
	if !active(r.State) {
		return "本轮验证已结束。"
	}
	if r.PrivateMessageID == 0 || r.PrivateMessageID != cb.Message.MessageID {
		return "验证面板已失效，请重新点击群内入口。"
	}
	if !s.now().Before(r.Deadline) {
		return "本轮验证已超时，请等待处理；被移出后可重新加入。"
	}
	c, err := s.getMember(s.channelID, r.UserID)
	if err != nil {
		return "关注状态暂时无法确认，请稍后重试。"
	}
	if !c.present() {
		if r.Confirmed {
			r.Confirmed = false
			if err = s.save(&r); err != nil {
				return "验证暂时失败，请稍后重试。"
			}
		}
		return "尚未确认关注，请先关注频道，再点击验证。"
	}
	if !s.now().Before(r.Deadline) {
		return "本轮验证已超时，请等待处理；被移出后可重新加入。"
	}
	r.Confirmed = true
	if err = s.save(&r); err != nil {
		return "验证暂时失败，请稍后重试。"
	}
	if err = s.advance(&r); err != nil {
		s.failure(&r, err)
		return "验证暂时失败，请稍后重试。"
	}
	if r.State == "passed" {
		return "✅ 验证通过，已恢复验证前的群权限。"
	}
	return "验证正在处理，请稍后重试。"
}
