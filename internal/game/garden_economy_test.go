package game

import (
	"testing"
	"time"
)

func TestGardenPurchasedSeedsHaveTieredExpectedReturns(t *testing.T) {
	var previousProfitTwice int
	var previousPrice int
	for _, seed := range GardenSeeds {
		if !seed.Purchasable {
			if got := GardenHerbBaseSellPrice(seed); got != seed.SellPrice {
				t.Fatalf("%s: reward-only herb price changed to %d", seed.Key, got)
			}
			continue
		}
		t.Run(seed.Key, func(t *testing.T) {
			// 使用两倍期望收入，保留半株产量的精度。
			base := GardenHerbBaseSellPrice(seed)
			revenueTwice := base * (seed.YieldMin + seed.YieldMax)
			profitTwice := revenueTwice - seed.Price*2
			if profitTwice < 0 || profitTwice*100 > seed.Price*2*5 {
				t.Fatalf("ordinary expected profit must be 0%%-5%%: price=%d profit*2=%d", seed.Price, profitTwice)
			}
			if previousPrice > 0 && profitTwice*previousPrice < previousProfitTwice*seed.Price {
				t.Fatal("higher tier must not have a lower ordinary expected profit rate")
			}
			previousProfitTwice, previousPrice = profitTwice, seed.Price
			for day := time.Date(2026, 1, 1, 22, 0, 0, 0, time.FixedZone("CST", 8*3600)); day.Year() == 2026; day = day.AddDate(0, 0, 1) {
				key := gardenHerbMarketDayKey(day)
				urgent := gardenHerbMarketPrice(seed, key)
				urgentRevenueTwice := urgent * (seed.YieldMin + seed.YieldMax)
				minReturn, maxReturn := 105, 110
				if seed.Key == "huiqicao" {
					// 回气草每株加 1 分就会增加 20% 期望收益。
					minReturn, maxReturn = 120, 120
				}
				if urgent <= base || urgentRevenueTwice*100 < seed.Price*2*minReturn || urgentRevenueTwice*100 > seed.Price*2*maxReturn {
					t.Fatalf("%s: urgent return must exceed ordinary and be %d%%-%d%%: base=%d urgent=%d", key, minReturn, maxReturn, base, urgent)
				}
				if gardenHerbMarketLimit(seed, key) <= 0 {
					t.Fatalf("%s: urgent quota must remain positive", key)
				}
			}
		})
	}
}

func TestGardenQinglingYangqiCheaperThanDirectPurchase(t *testing.T) {
	recipe, ok := gardenRecipeByKey("qinglingyangqi")
	if !ok {
		t.Fatal("recipe missing")
	}
	seedCost, sellValue := float64(recipe.AlchemyCost), recipe.AlchemyCost
	for _, material := range recipe.Materials {
		seed, ok := gardenSeedByHerbName(material.ItemName)
		if !ok || !seed.Purchasable {
			t.Fatalf("material cannot be grown from purchased seeds: %s", material.ItemName)
		}
		seedCost += float64(material.Quantity*seed.Price*2) / float64(seed.YieldMin+seed.YieldMax)
		sellValue += material.Quantity * GardenHerbBaseSellPrice(seed)
	}
	for _, item := range TreasureShopItems {
		if item.Name == recipe.ProductName {
			// 连普通回收的机会成本也低于直购，确保玩家有炼丹的动力。
			if seedCost > float64(item.Price)*0.85 || sellValue >= item.Price {
				t.Fatalf("alchemy is not attractive: seed cost=%.2f sell opportunity=%d direct=%d", seedCost, sellValue, item.Price)
			}
			return
		}
	}
	t.Fatal("direct purchase price missing")
}
