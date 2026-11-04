package market

import (
	"errors"
	"fmt"

	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

var errMarketplaceNoCommonPrice = errors.New("同批卡密没有共同的可售单价范围，请按续期天数拆分后分别上架")
var errMarketplaceReferencePriceUnavailable = errors.New("该卡密的系统参考原价为零或异常，暂不支持上架，请联系管理员")
var errMarketplaceNoAllowedPrice = errors.New("该商品的最低可售单价超过交易行单价上限，暂不可上架")

// Pricing uses the same integer bounds for the prompt and the final transaction.
// Multiple codes share one unit price, so their allowed ranges must intersect.
type marketplacePricing struct {
	OriginalMin int
	OriginalMax int
	Min         int
	Max         int
}

func marketplacePricingForValue(value int) marketplacePricing {
	p := marketplacePricing{Min: marketplaceMinUnitPrice, Max: marketplaceMaxPrice}
	if value > 0 {
		p.OriginalMin, p.OriginalMax = value, value
		p.Min = max(p.Min, marketplaceMinOriginalValuePrice(value))
		p.Max = min(p.Max, marketplaceMaxOriginalValuePrice(value))
	}
	return p
}

func (p marketplacePricing) text() string {
	if p.OriginalMax <= 0 {
		return fmt.Sprintf("可填单价：%d–%d 积分/件。\n系统暂无参考原价。", p.Min, p.Max)
	}
	original := fmt.Sprintf("%d", p.OriginalMin)
	if p.OriginalMin != p.OriginalMax {
		original = fmt.Sprintf("%d–%d", p.OriginalMin, p.OriginalMax)
	}
	return fmt.Sprintf("可填单价：%d–%d 积分/件（含边界，按原价的 85%%–115%% 及单价上限计算）。\n系统参考原价：%s 积分/件。", p.Min, p.Max, original)
}

func (p marketplacePricing) prompt() string {
	return p.text() + "\n\n请发送范围内的整数单价。"
}

type marketplacePriceError struct {
	Cause   error
	Pricing marketplacePricing
}

func (e *marketplacePriceError) Error() string { return e.Cause.Error() + ": " + e.Pricing.text() }
func (e *marketplacePriceError) Unwrap() error { return e.Cause }

func (p marketplacePricing) validate(price int) error {
	if price < p.Min {
		return &marketplacePriceError{Cause: storage.ErrMarketplacePriceBelowFloor, Pricing: p}
	}
	if price > p.Max {
		return &marketplacePriceError{Cause: storage.ErrMarketplacePriceAboveCeiling, Pricing: p}
	}
	return nil
}

func marketplaceSecretPricingTx(tx *gorm.DB, secrets []marketplaceSecretClassification) (marketplacePricing, error) {
	if _, err := marketplaceSecretListingSource(secrets); err != nil {
		return marketplacePricing{}, err
	}
	p := marketplacePricingForValue(0)
	for _, secret := range secrets {
		value, err := marketplaceSecretOriginalValueTx(tx, secret)
		if err != nil {
			return marketplacePricing{}, err
		}
		unit := marketplacePricingForValue(value)
		if unit.Min > unit.Max {
			return marketplacePricing{}, errMarketplaceNoAllowedPrice
		}
		p.Min, p.Max = max(p.Min, unit.Min), min(p.Max, unit.Max)
		if value > 0 {
			if p.OriginalMin == 0 || value < p.OriginalMin {
				p.OriginalMin = value
			}
			p.OriginalMax = max(p.OriginalMax, value)
		}
	}
	if p.Min > p.Max {
		return marketplacePricing{}, errMarketplaceNoCommonPrice
	}
	return p, nil
}

func inspectMarketplaceSecretPricing(sellerID int64, secrets []string) (marketplacePricing, error) {
	var pricing marketplacePricing
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		classified := make([]marketplaceSecretClassification, 0, len(secrets))
		for _, raw := range secrets {
			secret, err := classifyMarketplaceSecretTx(tx, security.HashSensitiveToken(raw), sellerID)
			if err != nil {
				return err
			}
			classified = append(classified, secret)
		}
		var err error
		pricing, err = marketplaceSecretPricingTx(tx, classified)
		return err
	})
	return pricing, err
}
