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
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const CallbackPrefix = "joinverify:"
const Timeout = 120 * time.Second
const activeStates = "preparing,pending,restoring,kicking,unbanning"

type Service struct {
	db                        *gorm.DB
	api                       API
	groupID, channelID, botID int64
	channelURL                string
	admins                    []int64
	locks                     [64]sync.Mutex
	wake                      chan struct{}
	now                       func() time.Time
}

func New(db *gorm.DB, api API, groupID, channelID, botID int64, channelURL string, admins []int64) *Service {
	return &Service{db: db, api: api, groupID: groupID, channelID: channelID, botID: botID, channelURL: channelURL, admins: admins, wake: make(chan struct{}, 1), now: time.Now}
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
		if err := s.handleEvent(e); err != nil {
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
	if err := s.db.Where("group_id = ? AND message_id <> 0 AND state NOT IN ?", s.groupID, strings.Split(activeStates, ",")).Limit(100).Find(&cleanup).Error; err == nil {
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
		return false, "⚠️ 请先加入官方群，关注指定频道，并点击群内「我已关注，验证」按钮完成验证。"
	}
	return true, ""
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
	if c.present() {
		return true, nil
	}
	if r.MessageID != 0 {
		// An undeletable old prompt must not prevent a new verification round.
		cleanup := model.AutoDeleteMsg{ChatID: s.groupID, MessageID: r.MessageID, DeleteAt: s.now()}
		if err := s.db.Create(&cleanup).Error; err != nil {
			return false, err
		}
		r.MessageID = 0
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
	r.GroupID = s.groupID
	r.UserID = userID
	r.ChannelID = s.channelID
	r.Round = hex.EncodeToString(token)
	r.Reason = reason
	r.State = "preparing"
	r.StartedAt = s.now()
	r.Deadline = s.now().Add(Timeout)
	r.OriginalPermissions = original
	r.OriginalUntil = g.UntilDate
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
	if c.present() {
		if r.State == "preparing" && g.Status == "restricted" && g.permissions == (permissions{}) && g.UntilDate == 0 {
			// A previous restrict request may have succeeded with its response lost.
			r.Muted = true
			if err = s.save(r); err != nil {
				return err
			}
		}
		return s.restore(r, g)
	}
	if r.State == "restoring" {
		// Subscription was removed while a restore was being retried. Retain the
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
	if r.MessageID == 0 {
		return s.prompt(r)
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
	r.LastError = ""
	if err := s.save(r); err != nil {
		return err
	}
	s.audit(r, state, detail)
	s.cleanup(r)
	return nil
}
func (s *Service) prompt(r *model.JoinVerification) error {
	q := params(s.groupID, 0)
	q["text"] = fmt.Sprintf("<a href=\"tg://user?id=%d\">新成员</a>，请在 2 分钟内关注指定频道，关注后点击「我已关注，验证」。超时将被移出群，可重新加入。", r.UserID)
	q["parse_mode"] = "HTML"
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonURL("前往关注频道", s.channelURL)),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("我已关注，验证", CallbackPrefix+r.Round)),
	)
	data, err := json.Marshal(keyboard)
	if err != nil {
		return err
	}
	q["reply_markup"] = string(data)
	var msg struct {
		MessageID int `json:"message_id"`
	}
	if err = s.call("sendMessage", q, &msg); err != nil {
		return err
	}
	r.MessageID = msg.MessageID
	return s.save(r)
}
func (s *Service) cleanup(r *model.JoinVerification) {
	if r.MessageID == 0 {
		return
	}
	q := params(s.groupID, 0)
	q["message_id"] = strconv.Itoa(r.MessageID)
	err := s.call("deleteMessage", q, nil)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "message to delete not found") {
		log.Printf("join verification prompt cleanup: %s", textfmt.FormatTelegramSendError(err))
		return
	}
	r.MessageID = 0
	if err = s.save(r); err != nil {
		log.Printf("join verification cleanup persistence: %s", textfmt.FormatPlainError(err))
	}
}
func (s *Service) Callback(cb *tgbotapi.CallbackQuery) string {
	if cb == nil || cb.From == nil || cb.Message == nil || cb.Message.Chat == nil || cb.Message.Chat.ID != s.groupID {
		return "验证消息无效。"
	}
	unlock := s.lock(cb.From.ID)
	defer unlock()
	r, err := s.record(cb.From.ID)
	if err != nil || r.Round != strings.TrimPrefix(cb.Data, CallbackPrefix) || r.MessageID != cb.Message.MessageID {
		return "此验证仅限本人操作，或本轮验证已失效。"
	}
	if !active(r.State) {
		return "本轮验证已结束。"
	}
	if !s.now().Before(r.Deadline) {
		return "本轮验证已超时，请等待处理；被移出后可重新加入。"
	}
	if err = s.advance(&r); err != nil {
		s.failure(&r, err)
		return "验证暂时失败，请稍后重试。"
	}
	if r.State == "passed" {
		return "✅ 验证成功，欢迎加入！"
	}
	return "尚未确认关注，请先关注频道，再点击验证。"
}
