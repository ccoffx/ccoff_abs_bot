package activity

import (
	"fmt"
	"strings"
	"time"

	"ccoff-abs-bot/internal/announcements"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/textfmt"
	"gorm.io/gorm"
)

func announcementLocation() *time.Location { return time.FixedZone("CST", 8*3600) }
func queueLotteryIntro(tx *gorm.DB, a model.LotteryActivity) error {
	if !a.AnnounceIsChannel {
		return nil
	}
	var prizes []model.LotteryPrize
	if err := tx.Where("activity_id = ?", a.ID).Order("id asc").Find(&prizes).Error; err != nil {
		return err
	}
	rule := fmt.Sprintf("满 %d 人自动开奖", a.ParticipantLimit)
	if a.DrawAt != nil {
		rule = "开奖时间：" + a.DrawAt.In(announcementLocation()).Format("2006-01-02 15:04:05")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🎲 积分抽奖开启：%s\n\n规则：%s\n参与消耗：%d 积分\n领奖期限：开奖后 24 小时\n\n奖品：", textfmt.LotteryDisplayText(a.Title, 80, "-"), rule, a.EntryCost)
	for _, p := range prizes {
		fmt.Fprintf(&b, "\n- %s × %d", textfmt.LotteryDisplayText(p.DisplayName, 80, "-"), p.Quantity)
	}
	b.WriteString("\n\n点击「参与抽奖」进入 Bot，确认后才扣分报名。")
	return announcements.Queue(tx, model.AnnouncementDelivery{Key: fmt.Sprintf("lottery:%d:intro", a.ID), Kind: "lottery_intro", TargetChatID: a.AnnounceChatID, ReminderGroupID: a.AnnounceReminderGroupID, ActorID: a.CreatedByID, Text: b.String()})
}
func queueLotteryResult(tx *gorm.DB, a model.LotteryActivity, count, winnerCount int, winners []lotteryDrawWinnerInfo) error {
	if !a.AnnounceIsChannel {
		return nil
	}
	return announcements.Queue(tx, model.AnnouncementDelivery{Key: fmt.Sprintf("lottery:%d:result", a.ID), Kind: "lottery_result", TargetChatID: a.AnnounceChatID, ReminderGroupID: a.AnnounceReminderGroupID, ActorID: a.CreatedByID, Text: lotteryResultText(a, count, winnerCount, winners)})
}
