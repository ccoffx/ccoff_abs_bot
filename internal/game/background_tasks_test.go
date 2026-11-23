package game

import (
	"strings"
	"testing"

	"ccoff-abs-bot/internal/background"
)

func TestBackgroundSubmissionOnlyRepliesForPendingOrRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result background.Result
		want   string
	}{
		{"accepted", background.Accepted, ""},
		{"pending", background.Pending, "已在处理中"},
		{"full", background.Full, "请稍后重新提交"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot, replies := secretRealmQueryBot(t)
			replyBackgroundSubmission(bot, 1, tc.result, "闭关准备")
			got := replies()
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("unexpected submission reply: %q", got)
			}
		})
	}
}
