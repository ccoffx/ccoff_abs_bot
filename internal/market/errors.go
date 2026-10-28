package market

import "errors"

var (
	ErrCodeCashoutInvalid      = errors.New("CODE_CASHOUT_INVALID")
	ErrCodeCashoutPriceChanged = errors.New("CODE_CASHOUT_PRICE_CHANGED")
	ErrCodeCashoutPriceInvalid = errors.New("CODE_CASHOUT_PRICE_INVALID")
)
