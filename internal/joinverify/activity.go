package joinverify

import (
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
)

func (s *Service) trackableGroupMessage(msg *tgbotapi.Message) bool {
	return msg != nil && msg.Chat != nil && msg.Chat.ID == s.groupID && msg.MessageID > 0 &&
		len(msg.NewChatMembers) == 0 && msg.LeftChatMember == nil && msg.PinnedMessage == nil
}

// QueueGroupMessage coalesces observations without database I/O on the polling
// or Telegram response path. The verification worker retries failed writes.
func (s *Service) QueueGroupMessage(msg *tgbotapi.Message) {
	if !s.trackableGroupMessage(msg) {
		return
	}
	s.queueMessageID(int64(msg.MessageID))
}

func (s *Service) queueMessageID(id int64) {
	for previous := s.pendingMessageID.Load(); id > previous; previous = s.pendingMessageID.Load() {
		if s.pendingMessageID.CompareAndSwap(previous, id) {
			return
		}
	}
}

func (s *Service) flushGroupActivity() error {
	id := s.pendingMessageID.Swap(0)
	if id == 0 {
		return nil
	}
	err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: int(id), Chat: &tgbotapi.Chat{ID: s.groupID}})
	if err != nil {
		// Preserve a newer observation that arrived while this write was pending.
		s.queueMessageID(id)
	}
	return err
}

// ObserveGroupMessage persists incoming messages and successful Bot sends. Edits
// do not change message order. Service messages are automatically deleted by the
// app and must not cause every join to replace an otherwise bottommost prompt.
func (s *Service) ObserveGroupMessage(msg *tgbotapi.Message) error {
	if !s.trackableGroupMessage(msg) {
		return nil
	}
	activity := model.JoinVerificationGroupActivity{GroupID: s.groupID, LastMessageID: msg.MessageID}
	return s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "group_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"last_message_id": gorm.Expr("MAX(last_message_id, excluded.last_message_id)"),
		}),
	}).Create(&activity).Error
}

func (s *Service) requestSharedPromptRefresh() error {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	prompt := model.JoinVerificationPrompt{GroupID: s.groupID, RefreshRequested: true}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "group_id"}},
		DoUpdates: clause.Assignments(map[string]any{"refresh_requested": true}),
	}).Create(&prompt).Error
}
