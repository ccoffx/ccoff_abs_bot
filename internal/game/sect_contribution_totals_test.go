package game

import (
	"testing"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestSectContributionQueriesUseTotalsAndRetainHistoricalMeaning(t *testing.T) {
	setupSectRegressionTest(t)
	db := storage.DB
	contributionMust(t, db.Create(&model.SectShopPurchase{SectID: 1, UserID: 1, CostContribution: 40}).Error)
	contributionMust(t, db.Create(&model.SectShopPurchase{SectID: 2, UserID: 1, CostContribution: 9000}).Error)
	var member model.SectMember
	contributionMust(t, db.Where("sect_id = 1 AND user_id = 1").First(&member).Error)
	// Querying ranks/eligibility must not consult any historical purchase rows.
	contributionMust(t, db.Migrator().DropTable(&model.SectShopPurchase{}))
	got, err := sectShopMemberTotalContributionTx(db, member)
	contributionMust(t, err)
	if got != member.Contribution+40 {
		t.Fatal("total contribution changed meaning", got, member)
	}
	rows, err := querySectContributionRankRows(db, 1, false, 20)
	contributionMust(t, err)
	if len(rows) != 1 || rows[0].TotalContribution != member.Contribution+40 {
		t.Fatal("rank included another sect or lost spending", rows)
	}
	rows, err = querySectContributionRankRows(db, 1, true, 20)
	contributionMust(t, err)
	if len(rows) != 1 || rows[0].TotalContribution != member.WeeklyContribution {
		t.Fatal("weekly rank changed", rows)
	}
}
