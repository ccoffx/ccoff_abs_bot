package titles

import (
	"fmt"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
)

// CleanupOtherGroups removes only group-scoped title data. Global title ownership,
// player accounts and balances are independent and must remain intact.
// An absent group configuration must never erase every group's data.
func CleanupOtherGroups() error {
	groupID := defaultGroupID()
	if groupID >= 0 {
		return fmt.Errorf("TITLE_GROUP_NOT_CONFIGURED")
	}
	return Transaction(func(tx *gorm.DB) error {
		for _, table := range []any{&model.TitleEquipment{}, &model.TitleMemberSyncRequest{}, &model.TitleMember{}, &model.TribulationTitleHolder{}, &model.TribulationTitleHistory{}, &model.TribulationTitleStat{}, &model.TitleGroup{}} {
			if err := tx.Where("chat_id <> ?", groupID).Delete(table).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
