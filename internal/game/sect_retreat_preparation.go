package game

import (
	"ccoff-abs-bot/internal/background"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

// Keep potentially slow retreat preparation away from message workers and
// unrelated background commands. Jobs do not charge prestige until committed.
var sectRetreatPreparations = background.New(2, 64)

func revalidateRetreatOperator(expected model.SectMember, sectWide bool) (model.SectMember, error) {
	var current model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, expected.UserID, &current, false); err != nil {
		return current, err
	}
	if current.ID != expected.ID || current.SectID != expected.SectID {
		return current, storage.ErrNotInSect
	}
	if sectWide {
		if current.Role != SectRoleOwner && current.Role != SectRoleElder {
			return current, storage.ErrSectNoPermission
		}
	} else if current.CaveUnlockedAt == nil {
		return current, storage.ErrSectCaveLocked
	}
	return current, nil
}
