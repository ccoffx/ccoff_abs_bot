package game

import "errors"

var (
	ErrCultivationTreasureUnavailable  = errors.New("当前境界无需兑换突破至宝（斗王至半帝可兑换）")
	ErrCultivationTreasureNoProfile    = errors.New("请先建立斗气档案后再兑换")
	ErrCultivationTreasureTokens       = errors.New("秘境信物不足，请参与宗门秘境获取")
	ErrCultivationTreasureExpired      = errors.New("兑换确认已失效，请重新打开突破材料面板")
	ErrCultivationTreasureCompleted    = errors.New("本次兑换已完成，请查看乾坤袋；如需再次兑换，请重新打开面板")
	ErrCultivationTreasureRealmChanged = errors.New("境界已发生变化，请重新打开面板确认所需至宝")
)

var errListeningAbuseEffectiveStartDayInit = errors.New("LISTENING_ABUSE_EFFECTIVE_START_DAY_INIT_FAILED")
