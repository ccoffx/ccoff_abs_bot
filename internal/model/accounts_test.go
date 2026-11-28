package model

import "testing"

func TestUserDisplayLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		user User
		want string
	}{
		{"cached name", User{TelegramID: 12, Username: "ghost_tg_12", DisplayName: " 真人 "}, "真人"},
		{"cached name preferred", User{TelegramID: 12, Username: "abs_account", DisplayName: "Telegram昵称"}, "Telegram昵称"},
		{"normal account", User{TelegramID: 12, Username: " abs_account "}, "abs_account"},
		{"missing cached name", User{TelegramID: 12, Username: "ghost_tg_12"}, "道友12"},
		{"invalid cached name", User{TelegramID: 12, Username: "account", DisplayName: "@ghost_tg_12"}, "account"},
		{"empty names", User{TelegramID: 12}, "道友12"},
		{"unknown identity", User{}, "未知用户"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.user.DisplayLabel(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
