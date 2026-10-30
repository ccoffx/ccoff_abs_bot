package storage

import (
	"ccoff-abs-bot/internal/model"
	"testing"
)

func TestAccountClassificationRequiresCurrentValues(t *testing.T) {
	for _, tc := range []struct {
		kind          string
		formal, trial bool
	}{
		{AccountTypeFormal, true, false}, {AccountTypeTrial, false, true}, {"", false, false}, {"unknown", false, false},
	} {
		u := model.User{AccountType: tc.kind}
		if IsFormalAccount(u) != tc.formal || IsTrialAccount(u) != tc.trial {
			t.Errorf("incorrect classification for %q", tc.kind)
		}
	}
	if !UserProfileStatusInactive(model.User{}) || UserProfileStatusInactive(model.User{Status: "active"}) {
		t.Fatal("only active status may grant access")
	}
}
