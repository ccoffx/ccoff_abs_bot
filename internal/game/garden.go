package game

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

const (
	gardenMaxPlots                = 6
	gardenUnlockMajor             = 1
	gardenStatusGrowing           = "growing"
	gardenStatusHarvest           = "harvested"
	gardenCallbackPrefix          = "garden:"
	gardenMarketOfferMax          = 2
	gardenBaseReturnPct           = 90
	gardenMaturityNoticeInterval  = 1 * time.Minute
	gardenMaturityNoticeBatchSize = 200
)

var gardenPlotCosts = map[int]int{
	1: 0,
	2: 20,
	3: 50,
	4: 100,
	5: 180,
	6: 300,
}

type gardenSeedConfig struct {
	Key          string
	SeedName     string
	HerbName     string
	Price        int
	GrowDuration time.Duration
	YieldMin     int
	YieldMax     int
	SellPrice    int
	DailyLimit   int
	Purchasable  bool
}

type gardenMaterial struct {
	ItemName string
	Quantity int
}

type gardenHerbMarketOffer struct {
	SeedKey  string
	HerbName string
	Price    int
	Limit    int
}

type gardenHerbEconomyConfig struct {
	MarketPriceMin int
	MarketPriceMax int
	MarketLimitMin int
	MarketLimitMax int
}

type GardenRecipeConfig struct {
	Key         string
	Name        string
	ProductName string
	UnlockPrice int
	AlchemyCost int
	Materials   []gardenMaterial
}

func gardenPointDescriptionName(name string) string {
	return textfmt.LotteryDisplayText(name, 80, "-")
}

func PillEffectSummary(itemName string) string {
	switch strings.TrimSpace(itemName) {
	case "回气丹":
		return "吞服后增加 1.0 小时丹药修为，本周最多 3 颗。"
	case "青灵养气丹":
		return "吞服后增加 3.0 小时丹药修为，本周最多 2 颗。"
	case "万载灵髓":
		return "吞服后增加 10.0 小时丹药修为，本月最多 1 颗。"
	case "凝元丹":
		return "斗师突破专用丹药，达到斗者大圆满后渡劫时自动消耗。"
	case "三纹清灵丹":
		return "大斗师突破专用丹药，达到斗师大圆满后渡劫时自动消耗。"
	case "斗灵丹":
		return "斗灵突破专用丹药，达到大斗师大圆满后渡劫时自动消耗。"
	case "皇极丹":
		return "斗王突破专用丹药，达到斗灵大圆满后渡劫时自动消耗。"
	default:
		return ""
	}
}

func PillEffectMarkdownLine(itemName string) string {
	if summary := PillEffectSummary(itemName); summary != "" {
		return "功效：" + textfmt.EscapeMarkdown(summary)
	}
	return ""
}

var GardenSeeds = []gardenSeedConfig{
	{Key: "huiqicao", SeedName: "回气草种子", HerbName: "回气草", Price: 15, GrowDuration: 2 * time.Hour, YieldMin: 2, YieldMax: 4, SellPrice: 3, DailyLimit: 10, Purchasable: true},
	{Key: "qingling", SeedName: "青灵叶种子", HerbName: "青灵叶", Price: 25, GrowDuration: 4 * time.Hour, YieldMin: 2, YieldMax: 3, SellPrice: 6, DailyLimit: 8, Purchasable: true},
	{Key: "chiyang", SeedName: "赤阳花种子", HerbName: "赤阳花", Price: 40, GrowDuration: 6 * time.Hour, YieldMin: 1, YieldMax: 3, SellPrice: 10, DailyLimit: 6, Purchasable: true},
	{Key: "yueyingteng", SeedName: "月影藤种子", HerbName: "月影藤", Price: 60, GrowDuration: 8 * time.Hour, YieldMin: 1, YieldMax: 2, SellPrice: 16, DailyLimit: 4, Purchasable: true},
	{Key: "xuanyanshen", SeedName: "玄岩参种子", HerbName: "玄岩参", Price: 90, GrowDuration: 12 * time.Hour, YieldMin: 1, YieldMax: 2, SellPrice: 25, DailyLimit: 3, Purchasable: true},
	{Key: "zijingzhi", SeedName: "紫晶芝种子", HerbName: "紫晶芝", Price: 140, GrowDuration: 18 * time.Hour, YieldMin: 1, YieldMax: 1, SellPrice: 45, DailyLimit: 1, Purchasable: true},
	{Key: "longxue", SeedName: "龙血果种子", HerbName: "龙血果", Price: 0, GrowDuration: 24 * time.Hour, YieldMin: 1, YieldMax: 1, SellPrice: 80, DailyLimit: 0, Purchasable: false},
	{Key: "chixinlian", SeedName: "炽心莲种子", HerbName: "炽心莲", Price: 0, GrowDuration: 36 * time.Hour, YieldMin: 1, YieldMax: 1, SellPrice: 120, DailyLimit: 0, Purchasable: false},
}

var gardenHerbEconomy = map[string]gardenHerbEconomyConfig{
	"huiqicao":    {MarketPriceMin: 5, MarketPriceMax: 6, MarketLimitMin: 15, MarketLimitMax: 15},
	"qingling":    {MarketPriceMin: 10, MarketPriceMax: 11, MarketLimitMin: 12, MarketLimitMax: 12},
	"chiyang":     {MarketPriceMin: 20, MarketPriceMax: 22, MarketLimitMin: 8, MarketLimitMax: 8},
	"yueyingteng": {MarketPriceMin: 40, MarketPriceMax: 43, MarketLimitMin: 5, MarketLimitMax: 5},
	"xuanyanshen": {MarketPriceMin: 60, MarketPriceMax: 64, MarketLimitMin: 3, MarketLimitMax: 3},
	"zijingzhi":   {MarketPriceMin: 140, MarketPriceMax: 148, MarketLimitMin: 1, MarketLimitMax: 1},
}

var GardenRecipes = []GardenRecipeConfig{
	{Key: "huiqi", Name: "回气丹方", ProductName: "回气丹", UnlockPrice: 20, AlchemyCost: 3, Materials: []gardenMaterial{{ItemName: "回气草", Quantity: 8}, {ItemName: "青灵叶", Quantity: 5}}},
	{Key: "ningyuan", Name: "凝元丹方", ProductName: "凝元丹", UnlockPrice: 30, AlchemyCost: 5, Materials: []gardenMaterial{{ItemName: "回气草", Quantity: 3}, {ItemName: "赤阳花", Quantity: 1}}},
	{Key: "sanwenqingling", Name: "三纹清灵丹方", ProductName: "三纹清灵丹", UnlockPrice: 60, AlchemyCost: 8, Materials: []gardenMaterial{{ItemName: "赤阳花", Quantity: 2}, {ItemName: "月影藤", Quantity: 1}}},
	{Key: "qinglingyangqi", Name: "青灵养气丹方", ProductName: "青灵养气丹", UnlockPrice: 100, AlchemyCost: 30, Materials: []gardenMaterial{{ItemName: "青灵叶", Quantity: 8}, {ItemName: "玄岩参", Quantity: 3}, {ItemName: "紫晶芝", Quantity: 1}}},
	{Key: "douling", Name: "斗灵丹方", ProductName: "斗灵丹", UnlockPrice: 160, AlchemyCost: 25, Materials: []gardenMaterial{{ItemName: "玄岩参", Quantity: 2}, {ItemName: "紫晶芝", Quantity: 1}}},
	{Key: "huangji", Name: "皇极丹方", ProductName: "皇极丹", UnlockPrice: 280, AlchemyCost: 50, Materials: []gardenMaterial{{ItemName: "龙血果", Quantity: 1}, {ItemName: "炽心莲", Quantity: 1}, {ItemName: "紫晶芝", Quantity: 1}}},
}

func GardenSeedByKey(key string) (gardenSeedConfig, bool) {
	for _, cfg := range GardenSeeds {
		if cfg.Key == key {
			return cfg, true
		}
	}
	return gardenSeedConfig{}, false
}

func gardenSeedByHerbName(name string) (gardenSeedConfig, bool) {
	for _, cfg := range GardenSeeds {
		if cfg.HerbName == name {
			return cfg, true
		}
	}
	return gardenSeedConfig{}, false
}

func GardenHerbBaseSellPrice(cfg gardenSeedConfig) int {
	if cfg.SellPrice < 0 {
		return 0
	}
	if !cfg.Purchasable {
		return cfg.SellPrice
	}
	price := gardenHerbReturnUnitPriceFloor(cfg, gardenBaseReturnPct)
	if price < cfg.SellPrice {
		return cfg.SellPrice
	}
	return price
}

func gardenHerbReturnUnitPriceFloor(cfg gardenSeedConfig, returnPct int) int {
	if !cfg.Purchasable || cfg.Price <= 0 || cfg.YieldMin <= 0 || cfg.YieldMax <= 0 {
		return cfg.SellPrice
	}

	denom := cfg.YieldMin + cfg.YieldMax
	if returnPct <= 0 {
		returnPct = 100
	}
	return cfg.Price * returnPct * 2 / (denom * 100)
}

func gardenMarketScore(dayKey string, seedKey string, salt string) int {
	score := 0
	for _, ch := range dayKey + ":" + seedKey + ":" + salt {
		score = (score*131 + int(ch)) & 0x7fffffff
	}
	return score
}

func gardenHerbMarketPrice(cfg gardenSeedConfig, dayKey string) int {
	basePrice := GardenHerbBaseSellPrice(cfg)
	if basePrice <= 0 {
		return 0
	}
	if !cfg.Purchasable {
		return basePrice
	}

	economy, ok := gardenHerbEconomy[cfg.Key]
	if !ok {
		return basePrice + 1
	}
	minPrice := economy.MarketPriceMin
	maxPrice := economy.MarketPriceMax
	if maxPrice < minPrice {
		maxPrice = minPrice
	}
	price := minPrice
	if maxPrice > minPrice {
		price += gardenMarketScore(dayKey, cfg.Key, "price") % (maxPrice - minPrice + 1)
	}
	if price <= basePrice {
		return basePrice + 1
	}
	return price
}

func gardenHerbMarketLimit(cfg gardenSeedConfig, dayKey string) int {
	economy, ok := gardenHerbEconomy[cfg.Key]
	if !cfg.Purchasable || !ok {
		return 0
	}
	minLimit := economy.MarketLimitMin
	maxLimit := economy.MarketLimitMax
	if minLimit < 0 {
		minLimit = 0
	}
	if maxLimit < minLimit {
		maxLimit = minLimit
	}
	limit := minLimit
	if maxLimit > minLimit {
		limit += gardenMarketScore(dayKey, cfg.Key, "limit") % (maxLimit - minLimit + 1)
	}
	if cfg.DailyLimit > 0 {
		maxBySeed := cfg.DailyLimit * (cfg.YieldMin + cfg.YieldMax) / 2
		if maxBySeed > 0 && limit > maxBySeed {
			limit = maxBySeed
		}
	}
	return limit
}

func gardenHerbMarketDayKey(t time.Time) string {
	loc := time.FixedZone("CST", 8*3600)
	local := t.In(loc)
	if local.Hour() < 22 {
		local = local.AddDate(0, 0, -1)
	}
	return local.Format("2006-01-02")
}

func gardenTodayHerbMarketOffers(t time.Time) []gardenHerbMarketOffer {
	dayKey := gardenHerbMarketDayKey(t)
	candidates := make([]gardenSeedConfig, 0, len(GardenSeeds))
	for _, cfg := range GardenSeeds {
		if cfg.Purchasable && GardenHerbBaseSellPrice(cfg) > 0 {
			candidates = append(candidates, cfg)
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		left := gardenMarketScore(dayKey, candidates[i].Key, "pick")
		right := gardenMarketScore(dayKey, candidates[j].Key, "pick")
		if left == right {
			return candidates[i].Key < candidates[j].Key
		}
		return left > right
	})

	if len(candidates) > gardenMarketOfferMax {
		candidates = candidates[:gardenMarketOfferMax]
	}

	selected := make(map[string]gardenSeedConfig, len(candidates))
	for _, cfg := range candidates {
		selected[cfg.Key] = cfg
	}

	offers := make([]gardenHerbMarketOffer, 0, len(candidates))
	for _, cfg := range GardenSeeds {
		if _, ok := selected[cfg.Key]; !ok {
			continue
		}
		offers = append(offers, gardenHerbMarketOffer{
			SeedKey:  cfg.Key,
			HerbName: cfg.HerbName,
			Price:    gardenHerbMarketPrice(cfg, dayKey),
			Limit:    gardenHerbMarketLimit(cfg, dayKey),
		})
	}
	return offers
}

func gardenTodayHerbMarketOfferMap(t time.Time) map[string]gardenHerbMarketOffer {
	offers := gardenTodayHerbMarketOffers(t)
	result := make(map[string]gardenHerbMarketOffer, len(offers))
	for _, offer := range offers {
		result[offer.SeedKey] = offer
	}
	return result
}

func gardenRecipeByKey(key string) (GardenRecipeConfig, bool) {
	for _, cfg := range GardenRecipes {
		if cfg.Key == key {
			return cfg, true
		}
	}
	return GardenRecipeConfig{}, false
}

func HandleGardenEntry(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return
	}
	if !msg.Chat.IsPrivate() {
		telegram.SendPlainText(bot, msg.Chat.ID, "道友的药园需静心打理，请私聊我进入药园。")
		return
	}
	if _, _, err := storage.EnsureUserWallet(msg.From); err != nil {
		log.Printf("⚠️ 药园钱包初始化失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 钱包初始化失败，请稍后再试。")
		return
	}
	if ok, reason := ensureGardenAccessible(msg.From.ID); !ok {
		telegram.ReplyText(bot, msg.Chat.ID, reason)
		return
	}
	text, markup := renderGardenHome(msg.From.ID)
	sendGardenScreen(bot, msg.Chat.ID, text, markup)
}

func HandleGardenSellCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.From == nil || msg.Chat == nil || !strings.HasPrefix(text, "回收灵草") {
		return false
	}
	if !msg.Chat.IsPrivate() {
		telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
		telegram.SendPlainText(bot, msg.Chat.ID, "药铺回收涉及积分变动，请私聊我发送："+textfmt.Copyable("回收灵草 玄岩参 1"))
		return true
	}
	if _, _, err := storage.EnsureUserWallet(msg.From); err != nil {
		log.Printf("⚠️ 药铺回收钱包初始化失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 钱包初始化失败，请稍后再试。")
		return true
	}
	if ok, reason := ensureGardenAccessible(msg.From.ID); !ok {
		telegram.ReplyText(bot, msg.Chat.ID, reason)
		return true
	}

	parts := strings.Fields(text)
	if len(parts) != 3 {
		telegram.ReplyText(bot, msg.Chat.ID, "用法："+textfmt.Copyable("回收灵草 玄岩参 1")+"\n请填写要回收的具体数量。")
		return true
	}

	cfg, ok := gardenSeedByHerbName(parts[1])
	if !ok {
		telegram.ReplyText(bot, msg.Chat.ID, "未找到这种灵草，请检查名称。")
		return true
	}

	qty, err := strconv.Atoi(parts[2])
	if err != nil || qty <= 0 {
		telegram.ReplyText(bot, msg.Chat.ID, "回收数量必须是正整数，请发送："+textfmt.Copyable("回收灵草 玄岩参 1"))
		return true
	}

	points, soldQty, err := GardenSellHerbQuantity(msg.From.ID, cfg.Key, qty)
	if err != nil {
		telegram.ReplyText(bot, msg.Chat.ID, GardenActionErrorText(err))
		return true
	}
	telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf("✅ 药铺回收【%s】x%d，获得 %d 积分。", textfmt.InventoryItemMarkdownName(cfg.HerbName), soldQty, points))
	return true
}

func HandleGardenCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || cb.From == nil || !strings.HasPrefix(cb.Data, gardenCallbackPrefix) {
		return false
	}
	if cb.Message == nil || cb.Message.Chat == nil {
		telegram.AnswerCallback(bot, cb.ID, "药园入口已失效，请重新发送“药园”")
		return true
	}
	if !cb.Message.Chat.IsPrivate() {
		telegram.AnswerCallback(bot, cb.ID, "请私聊打理药园")
		return true
	}

	userID := cb.From.ID
	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID

	if _, _, err := storage.EnsureUserWallet(cb.From); err != nil {
		log.Printf("⚠️ 药园钱包初始化失败: user=%d err=%s", cb.From.ID, textfmt.FormatPlainError(err))
		telegram.AnswerCallback(bot, cb.ID, "钱包初始化失败")
		return true
	}

	if ok, reason := ensureGardenAccessible(userID); !ok {
		telegram.AnswerCallback(bot, cb.ID, "尚未开启药园")
		editGardenScreen(bot, chatID, messageID, reason, gardenBackHomeMarkup())
		return true
	}

	parts := strings.Split(cb.Data, ":")
	if len(parts) < 2 {
		telegram.AnswerCallback(bot, cb.ID, "未知药园操作")
		return true
	}

	switch parts[1] {
	case "home":
		telegram.AnswerCallback(bot, cb.ID, "已返回药园")
		text, markup := renderGardenHome(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "fields":
		telegram.AnswerCallback(bot, cb.ID, "灵田已刷新")
		text, markup := renderGardenFields(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "open":
		if err := gardenOpenNextPlot(userID); err != nil {
			telegram.AnswerCallback(bot, cb.ID, GardenActionErrorText(err))
		} else {
			telegram.AnswerCallback(bot, cb.ID, "灵田开垦成功")
		}
		text, markup := renderGardenFields(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "plantlist":
		if len(parts) < 3 {
			telegram.AnswerCallback(bot, cb.ID, "灵田编号异常")
			return true
		}
		plotNo, err := strconv.Atoi(parts[2])
		if err != nil || plotNo <= 0 {
			telegram.AnswerCallback(bot, cb.ID, "灵田编号异常")
			return true
		}
		telegram.AnswerCallback(bot, cb.ID, "请选择种子")
		text, markup := renderGardenPlantList(userID, plotNo)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "plant":
		if len(parts) < 4 {
			telegram.AnswerCallback(bot, cb.ID, "种植参数异常")
			return true
		}
		plotNo, err := strconv.Atoi(parts[2])
		if err != nil || plotNo <= 0 {
			telegram.AnswerCallback(bot, cb.ID, "灵田编号异常")
			return true
		}
		if err := gardenPlantSeed(userID, plotNo, parts[3]); err != nil {
			telegram.AnswerCallback(bot, cb.ID, GardenActionErrorText(err))
		} else {
			telegram.AnswerCallback(bot, cb.ID, "种植成功")
		}
		text, markup := renderGardenFields(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "harvest":
		if len(parts) < 3 {
			telegram.AnswerCallback(bot, cb.ID, "灵田编号异常")
			return true
		}
		plotNo, err := strconv.Atoi(parts[2])
		if err != nil || plotNo <= 0 {
			telegram.AnswerCallback(bot, cb.ID, "灵田编号异常")
			return true
		}
		qty, herb, err := gardenHarvestPlot(userID, plotNo)
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, GardenActionErrorText(err))
		} else {
			telegram.AnswerCallback(bot, cb.ID, fmt.Sprintf("收获 %s x%d", herb, qty))
		}
		text, markup := renderGardenFields(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "harvestall":
		count, qty, err := gardenHarvestAll(userID)
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, GardenActionErrorText(err))
		} else {
			telegram.AnswerCallback(bot, cb.ID, fmt.Sprintf("收获 %d 块田，共 %d 株药草", count, qty))
		}
		text, markup := renderGardenFields(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "shop":
		telegram.AnswerCallback(bot, cb.ID, "种子商店已刷新")
		text, markup := renderGardenSeedShop(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "buy":
		if len(parts) < 3 {
			telegram.AnswerCallback(bot, cb.ID, "种子参数异常")
			return true
		}
		if err := gardenBuySeed(userID, parts[2]); err != nil {
			telegram.AnswerCallback(bot, cb.ID, GardenActionErrorText(err))
		} else {
			telegram.AnswerCallback(bot, cb.ID, "种子已入袋")
		}
		text, markup := renderGardenSeedShop(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "herbs":
		telegram.AnswerCallback(bot, cb.ID, "草药背包已刷新")
		text, markup := renderGardenHerbs(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "market":
		telegram.AnswerCallback(bot, cb.ID, "药铺回收已刷新")
		text, markup := renderGardenHerbMarket(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "sellone":
		if len(parts) < 3 {
			telegram.AnswerCallback(bot, cb.ID, "药草参数异常")
			return true
		}
		points, _, err := GardenSellHerbQuantity(userID, parts[2], 1)
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, GardenActionErrorText(err))
		} else {
			telegram.AnswerCallback(bot, cb.ID, fmt.Sprintf("药铺回收 +%d 积分", points))
		}
		text, markup := renderGardenHerbMarket(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "sellqty":
		if len(parts) < 3 {
			telegram.AnswerCallback(bot, cb.ID, "药草参数异常")
			return true
		}
		cfg, ok := GardenSeedByKey(parts[2])
		if !ok {
			telegram.AnswerCallback(bot, cb.ID, "药草参数异常")
			return true
		}
		session := telegram.GetSession(userID)
		session.SetStep("WAITING_GARDEN_SELL_QTY")
		session.SetTemp("garden_sell_seed_key", cfg.Key)
		session.SetTemp("garden_sell_herb_name", cfg.HerbName)
		telegram.UserSessions.Store(userID, session)
		telegram.AnswerCallback(bot, cb.ID, "请发送回收数量")
		telegram.SendPlainText(bot, chatID, fmt.Sprintf("药铺准备回收【%s】。\n请发送要回收的数量，例如："+textfmt.Copyable("3")+"\n发送“"+textfmt.Copyable("取消")+"”可退出。", cfg.HerbName))
	case "recipes":
		telegram.AnswerCallback(bot, cb.ID, "丹方已刷新")
		text, markup := renderGardenRecipes(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "recipebuy":
		if len(parts) < 3 {
			telegram.AnswerCallback(bot, cb.ID, "丹方参数异常")
			return true
		}
		if err := gardenBuyRecipe(userID, parts[2]); err != nil {
			telegram.AnswerCallback(bot, cb.ID, GardenActionErrorText(err))
		} else {
			telegram.AnswerCallback(bot, cb.ID, "丹方已参悟")
		}
		text, markup := renderGardenRecipes(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	case "alchemy":
		if len(parts) < 3 {
			telegram.AnswerCallback(bot, cb.ID, "丹方参数异常")
			return true
		}
		product, err := gardenAlchemy(userID, parts[2])
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, GardenActionErrorText(err))
		} else {
			telegram.AnswerCallback(bot, cb.ID, product+" 炼成")
		}
		text, markup := renderGardenRecipes(userID)
		editGardenScreen(bot, chatID, messageID, text, markup)
	default:
		telegram.AnswerCallback(bot, cb.ID, "未知药园操作")
	}
	return true
}

func ensureGardenAccessible(userID int64) (bool, string) {
	var plotCount int64
	if err := storage.DB.Model(&model.GardenPlot{}).Where("user_id = ?", userID).Count(&plotCount).Error; err != nil {
		log.Printf("⚠️ 读取药园灵田失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return false, "❌ 药园灵脉紊乱，请稍后再试。"
	}
	if plotCount > 0 {
		return true, ""
	}

	cul := GetOrCreateCultivation(userID)
	if cul == nil {
		return false, "❌ 斗气档案读取失败，请稍后再试。"
	}
	if cul.MajorRealm < gardenUnlockMajor {
		return false, fmt.Sprintf("🌱 **【药园尚未开启】**\n\n当前境界：%s\n药园需达到 **斗者境界** 后开启。", GetRealmName(cul))
	}

	if err := createGardenInitialPlotIfMissing(userID); err != nil {
		log.Printf("⚠️ 初始化药园失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return false, "❌ 药园开辟失败，请稍后再试。"
	}
	return true, ""
}

func renderGardenHome(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	var plotCount int64
	var readyCount int64
	plotErr := storage.DB.Model(&model.GardenPlot{}).Where("user_id = ?", userID).Count(&plotCount).Error
	readyErr := storage.DB.Model(&model.GardenPlanting{}).
		Where("user_id = ? AND status = ? AND matures_at <= ?", userID, gardenStatusGrowing, time.Now()).
		Count(&readyCount).Error

	seedTotal, seedErr := gardenInventoryTotalWithError(userID, gardenSeedItemNames())
	herbTotal, herbErr := gardenInventoryTotalWithError(userID, gardenHerbItemNames())
	var recipeCount int64
	recipeErr := storage.DB.Model(&model.GardenRecipeUnlock{}).Where("user_id = ?", userID).Count(&recipeCount).Error
	if plotErr != nil {
		log.Printf("⚠️ 读取药园灵田数量失败: user=%d err=%s", userID, textfmt.FormatPlainError(plotErr))
	}
	if readyErr != nil {
		log.Printf("⚠️ 读取药园成熟数量失败: user=%d err=%s", userID, textfmt.FormatPlainError(readyErr))
	}
	if seedErr != nil {
		log.Printf("⚠️ 读取药园种子数量失败: user=%d err=%s", userID, textfmt.FormatPlainError(seedErr))
	}
	if herbErr != nil {
		log.Printf("⚠️ 读取药园药草数量失败: user=%d err=%s", userID, textfmt.FormatPlainError(herbErr))
	}
	if recipeErr != nil {
		log.Printf("⚠️ 读取药园丹方数量失败: user=%d err=%s", userID, textfmt.FormatPlainError(recipeErr))
	}

	text := fmt.Sprintf("🌱 **【药园】**\n\n灵田：`%s/%d` 块\n成熟：`%s` 块\n种子：`%s` 枚\n药草：`%s` 株\n已学丹方：`%s` 张",
		GardenCountText(plotCount, plotErr == nil),
		gardenMaxPlots,
		GardenCountText(readyCount, readyErr == nil),
		GardenCountText(int64(seedTotal), seedErr == nil),
		GardenCountText(int64(herbTotal), herbErr == nil),
		GardenCountText(recipeCount, recipeErr == nil))
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("灵田管理", "garden:fields"),
			tgbotapi.NewInlineKeyboardButtonData("种子商店", "garden:shop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("草药背包", "garden:herbs"),
			tgbotapi.NewInlineKeyboardButtonData("药铺回收", "garden:market"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("丹方炼丹", "garden:recipes"),
		),
	)
	return text, markup
}

func GardenCountText(count int64, available bool) string {
	if !available {
		return "读取失败"
	}
	return fmt.Sprintf("%d", count)
}

func renderGardenFields(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	plots, plotErr := gardenUserPlotsWithError(userID)
	if plotErr != nil {
		log.Printf("⚠️ 读取灵田列表失败: user=%d err=%s", userID, textfmt.FormatPlainError(plotErr))
	}
	active, plantingErr := gardenActivePlantingsByPlotWithError(userID)
	if plantingErr != nil {
		log.Printf("⚠️ 读取种植记录失败: user=%d err=%s", userID, textfmt.FormatPlainError(plantingErr))
	}
	now := time.Now()

	var b strings.Builder
	b.WriteString("🌾 **【灵田管理】**\n\n")
	if plotErr != nil || plantingErr != nil {
		b.WriteString("灵田状态读取失败，请稍后再试。\n")
	} else if len(plots) == 0 {
		b.WriteString("尚未开辟灵田。\n")
	} else {
		for _, plot := range plots {
			if planting, ok := active[plot.ID]; ok {
				if !planting.MaturesAt.After(now) {
					b.WriteString(fmt.Sprintf("`%d` 号灵田：%s，**已成熟**\n", plot.PlotNo, textfmt.InventoryItemMarkdownName(planting.HerbName)))
				} else {
					b.WriteString(fmt.Sprintf("`%d` 号灵田：%s，剩余 `%s`\n", plot.PlotNo, textfmt.InventoryItemMarkdownName(planting.HerbName), gardenDurationText(time.Until(planting.MaturesAt))))
				}
			} else {
				b.WriteString(fmt.Sprintf("`%d` 号灵田：空闲\n", plot.PlotNo))
			}
		}
	}

	rows := make([][]tgbotapi.InlineKeyboardButton, 0)
	if plotErr == nil && plantingErr == nil {
		for _, plot := range plots {
			if planting, ok := active[plot.ID]; ok {
				if !planting.MaturesAt.After(now) {
					rows = append(rows, tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("收获 %d 号田", plot.PlotNo), fmt.Sprintf("garden:harvest:%d", plot.PlotNo)),
					))
				}
				continue
			}
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("种植 %d 号田", plot.PlotNo), fmt.Sprintf("garden:plantlist:%d", plot.PlotNo)),
			))
		}

		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("一键收获成熟", "garden:harvestall"),
		))
		if len(plots) < gardenMaxPlots {
			nextNo := len(plots) + 1
			cost := gardenPlotCosts[nextNo]
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("开垦第 %d 块田（%d积分）", nextNo, cost), "garden:open"),
			))
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("返回药园", "garden:home"),
	))
	return b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func renderGardenPlantList(userID int64, plotNo int) (string, tgbotapi.InlineKeyboardMarkup) {
	inv, invErr := gardenInventoryByNamesWithError(userID, gardenSeedItemNames())
	if invErr != nil {
		log.Printf("⚠️ 读取药园种子库存失败: user=%d err=%s", userID, textfmt.FormatPlainError(invErr))
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🌱 **【选择种子】**\n\n目标灵田：`%d` 号\n\n", plotNo))

	rows := make([][]tgbotapi.InlineKeyboardButton, 0)
	if invErr != nil {
		b.WriteString("种子库存读取失败，请稍后再试。\n")
	} else {
		for _, cfg := range GardenSeeds {
			qty := inv[cfg.SeedName]
			if qty <= 0 {
				continue
			}
			b.WriteString(fmt.Sprintf("%s：`%d` 枚，成熟 `%s`，产量 `%s`\n", textfmt.InventoryItemMarkdownName(cfg.SeedName), qty, gardenDurationText(cfg.GrowDuration), gardenYieldText(cfg)))
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("种 %s", cfg.HerbName), fmt.Sprintf("garden:plant:%d:%s", plotNo, cfg.Key)),
			))
		}
		if len(rows) == 0 {
			b.WriteString("乾坤袋中暂无种子，请先去种子商店购买。\n")
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("种子商店", "garden:shop"),
		tgbotapi.NewInlineKeyboardButtonData("返回灵田", "garden:fields"),
	))
	return b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func renderGardenSeedShop(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	dayKey := storage.SignInDateKey(time.Now())
	purchases, purchaseErr := gardenTodaySeedPurchasesWithError(userID, dayKey)
	if purchaseErr != nil {
		log.Printf("⚠️ 读取种子限购记录失败: user=%d day=%s err=%s", userID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(purchaseErr))
	}
	inv, invErr := gardenInventoryByNamesWithError(userID, gardenSeedItemNames())
	if invErr != nil {
		log.Printf("⚠️ 读取种子商店库存失败: user=%d err=%s", userID, textfmt.FormatPlainError(invErr))
	}
	points := 0
	pointsAvailable := true
	var u model.User
	if err := storage.DB.Select("points").Where("telegram_id = ?", userID).First(&u).Error; err == nil {
		points = u.Points
	} else {
		pointsAvailable = false
		log.Printf("⚠️ 种子商店钱包读取失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
	}
	pointsText := GardenCountText(int64(points), pointsAvailable)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("🛒 **【种子商店】**\n\n账户可用积分：`%s`\n每日限购按北京时间刷新。\n\n", pointsText))

	rows := make([][]tgbotapi.InlineKeyboardButton, 0)
	for _, cfg := range GardenSeeds {
		if !cfg.Purchasable {
			continue
		}
		leftText := "读取失败"
		left := 0
		if purchaseErr == nil {
			bought := purchases[cfg.Key]
			left = cfg.DailyLimit - bought
			if left < 0 {
				left = 0
			}
			leftText = fmt.Sprintf("%d/%d", left, cfg.DailyLimit)
		}
		heldText := GardenCountText(int64(inv[cfg.SeedName]), invErr == nil)
		b.WriteString(fmt.Sprintf("%s：`%d` 积分，产量 `%s`，成熟 `%s`，今日剩余 `%s`，持有 `%s`\n",
			textfmt.InventoryItemMarkdownName(cfg.SeedName), cfg.Price, gardenYieldText(cfg), gardenDurationText(cfg.GrowDuration), leftText, heldText))
		label := fmt.Sprintf("买 %s（%d）", cfg.HerbName, cfg.Price)
		if purchaseErr != nil {
			label = fmt.Sprintf("%s 额度读取失败", cfg.HerbName)
		} else if left <= 0 {
			label = fmt.Sprintf("%s 已售罄", cfg.HerbName)
		}
		if purchaseErr == nil {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("garden:buy:%s", cfg.Key)),
			))
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("返回药园", "garden:home"),
	))
	return b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func renderGardenHerbs(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	inv, invErr := gardenInventoryByNamesWithError(userID, gardenHerbItemNames())
	if invErr != nil {
		log.Printf("⚠️ 读取草药背包失败: user=%d err=%s", userID, textfmt.FormatPlainError(invErr))
	}

	var b strings.Builder
	b.WriteString("🌿 **【草药背包】**\n\n")
	hasHerb := false
	if invErr != nil {
		b.WriteString("草药库存读取失败，请稍后再试。\n")
	} else {
		for _, cfg := range GardenSeeds {
			qty := inv[cfg.HerbName]
			if qty <= 0 {
				continue
			}
			hasHerb = true
			b.WriteString(fmt.Sprintf("%s：`%d` 株\n", textfmt.InventoryItemMarkdownName(cfg.HerbName), qty))
		}
		if !hasHerb {
			b.WriteString("暂未收获药草。\n")
		}
	}

	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("药铺回收", "garden:market"),
			tgbotapi.NewInlineKeyboardButtonData("丹方炼丹", "garden:recipes"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回药园", "garden:home"),
		),
	}
	return b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func renderGardenHerbMarket(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	inv, invErr := gardenInventoryByNamesWithError(userID, gardenHerbItemNames())
	if invErr != nil {
		log.Printf("⚠️ 读取药铺回收库存失败: user=%d err=%s", userID, textfmt.FormatPlainError(invErr))
	}
	now := time.Now()
	dayKey := gardenHerbMarketDayKey(now)
	offers := gardenTodayHerbMarketOffers(now)
	offerBySeed := gardenTodayHerbMarketOfferMap(now)
	marketSales, marketSalesErr := gardenTodayHerbMarketSalesWithError(userID, dayKey)
	if marketSalesErr != nil {
		log.Printf("⚠️ 读取药市急收额度失败: user=%d day=%s err=%s", userID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(marketSalesErr))
	}

	var b strings.Builder
	b.WriteString("🏪 **【药铺回收】**\n\n药铺回收会直接兑换积分，急收额度按北京时间每日 22:00 刷新。\n")
	if len(offers) > 0 {
		b.WriteString("今日药市急收：\n")
		for _, offer := range offers {
			leftText := "读取失败"
			if marketSalesErr == nil {
				left := offer.Limit - marketSales[offer.SeedKey]
				if left < 0 {
					left = 0
				}
				leftText = fmt.Sprintf("%d/%d", left, offer.Limit)
			}
			b.WriteString(fmt.Sprintf("- %s：`%d` 积分/株，剩余 `%s`\n", textfmt.InventoryItemMarkdownName(offer.HerbName), offer.Price, leftText))
		}
	}
	b.WriteString("\n")

	rows := make([][]tgbotapi.InlineKeyboardButton, 0)
	if invErr != nil {
		b.WriteString("药草库存读取失败，请稍后再试。\n")
	} else {
		for _, cfg := range GardenSeeds {
			qty := inv[cfg.HerbName]
			if qty <= 0 {
				continue
			}
			basePrice := GardenHerbBaseSellPrice(cfg)
			line := fmt.Sprintf("%s：`%d` 株，基础回收 `%d` 积分/株", textfmt.InventoryItemMarkdownName(cfg.HerbName), qty, basePrice)
			if offer, ok := offerBySeed[cfg.Key]; ok && offer.Price > basePrice {
				leftText := "读取失败"
				if marketSalesErr == nil {
					left := offer.Limit - marketSales[offer.SeedKey]
					if left < 0 {
						left = 0
					}
					leftText = fmt.Sprintf("%d", left)
				}
				line += fmt.Sprintf("，急收 `%d` 积分/株（剩 `%s`）", offer.Price, leftText)
			}
			b.WriteString(line + "\n")
			if marketSalesErr == nil {
				rows = append(rows, tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("回收1株 "+cfg.HerbName, "garden:sellone:"+cfg.Key),
					tgbotapi.NewInlineKeyboardButtonData("指定数量", "garden:sellqty:"+cfg.Key),
				))
			}
		}
		if len(rows) == 0 {
			b.WriteString("暂未收获可回收药草。\n")
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("草药背包", "garden:herbs"),
		tgbotapi.NewInlineKeyboardButtonData("返回药园", "garden:home"),
	))
	return b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func renderGardenRecipes(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	unlocked, unlockedErr := gardenUnlockedRecipesWithError(userID)
	if unlockedErr != nil {
		log.Printf("⚠️ 读取丹方解锁记录失败: user=%d err=%s", userID, textfmt.FormatPlainError(unlockedErr))
	}
	inv, invErr := gardenInventoryByNamesWithError(userID, append(gardenHerbItemNames(), gardenPillItemNames()...))
	if invErr != nil {
		log.Printf("⚠️ 读取丹方炼丹库存失败: user=%d err=%s", userID, textfmt.FormatPlainError(invErr))
	}

	var b strings.Builder
	b.WriteString("🧪 **【丹方炼丹】**\n\n丹方永久解锁，炼丹当前为必成。\n\n")
	if unlockedErr != nil {
		b.WriteString("丹方解锁状态读取失败，请稍后再试。\n\n")
	}
	if invErr != nil {
		b.WriteString("炼丹材料库存读取失败，请稍后再试。\n\n")
	}

	rows := make([][]tgbotapi.InlineKeyboardButton, 0)
	for _, cfg := range GardenRecipes {
		state := "未学"
		if unlockedErr != nil {
			state = "读取失败"
		} else if unlocked[cfg.Key] {
			state = "已学"
		}
		b.WriteString(fmt.Sprintf("**%s** -> %s（%s）\n", textfmt.EscapeMarkdown(cfg.Name), textfmt.InventoryItemMarkdownName(cfg.ProductName), state))
		if effectLine := PillEffectMarkdownLine(cfg.ProductName); effectLine != "" {
			b.WriteString(effectLine + "\n")
		}
		productCountText := GardenCountText(int64(inv[cfg.ProductName]), invErr == nil)
		b.WriteString(fmt.Sprintf("解锁 `%d` 积分，炼丹 `%d` 积分，持有成丹 `%s`\n", cfg.UnlockPrice, cfg.AlchemyCost, productCountText))
		b.WriteString("材料：")
		for i, mat := range cfg.Materials {
			if i > 0 {
				b.WriteString("，")
			}
			matCountText := GardenCountText(int64(inv[mat.ItemName]), invErr == nil)
			b.WriteString(fmt.Sprintf("%s x%d（有%s）", textfmt.InventoryItemMarkdownName(mat.ItemName), mat.Quantity, matCountText))
		}
		b.WriteString("\n\n")

		if unlockedErr != nil || invErr != nil {
			continue
		}
		if unlocked[cfg.Key] {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("炼制 "+cfg.ProductName, "garden:alchemy:"+cfg.Key),
			))
		} else {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("参悟 %s（%d）", cfg.Name, cfg.UnlockPrice), "garden:recipebuy:"+cfg.Key),
			))
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("返回药园", "garden:home"),
	))
	return b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func createGardenInitialPlotIfMissing(userID int64) error {
	if userID == 0 {
		return fmt.Errorf("GARDEN_INITIAL_PLOT_INVALID")
	}
	plot := model.GardenPlot{
		UserID:     userID,
		PlotNo:     1,
		UnlockedAt: time.Now(),
	}
	res := storage.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&plot)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	return nil
}

func createGardenPlotInTx(tx *gorm.DB, plot *model.GardenPlot) error {
	if tx == nil || plot == nil {
		return fmt.Errorf("GARDEN_PLOT_INVALID")
	}
	entry := *plot
	res := tx.Create(&entry)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return storage.ErrGardenPlotMax
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("GARDEN_PLOT_CREATE_MISSED")
	}
	*plot = entry
	return nil
}

func createGardenPlantingInTx(tx *gorm.DB, planting *model.GardenPlanting) error {
	if tx == nil || planting == nil {
		return fmt.Errorf("GARDEN_PLANTING_INVALID")
	}
	entry := *planting
	res := tx.Create(&entry)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return storage.ErrGardenPlotBusy
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("GARDEN_PLANTING_CREATE_MISSED")
	}
	*planting = entry
	return nil
}

func createGardenRecipeUnlockInTx(tx *gorm.DB, unlock *model.GardenRecipeUnlock) error {
	if tx == nil || unlock == nil {
		return fmt.Errorf("GARDEN_RECIPE_UNLOCK_INVALID")
	}
	entry := *unlock
	res := tx.Create(&entry)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return storage.ErrGardenRecipeUnlocked
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("GARDEN_RECIPE_UNLOCK_CREATE_MISSED")
	}
	*unlock = entry
	return nil
}

func createGardenSeedPurchaseIfMissingInTx(tx *gorm.DB, purchase *model.GardenSeedPurchase) error {
	if tx == nil || purchase == nil {
		return fmt.Errorf("GARDEN_SEED_PURCHASE_INVALID")
	}
	entry := *purchase
	entry.SeedKey = textfmt.FormatPlainValue(entry.SeedKey)
	entry.DayKey = textfmt.FormatPlainValue(entry.DayKey)
	if entry.UserID == 0 || entry.SeedKey == "" || entry.DayKey == "" {
		return fmt.Errorf("GARDEN_SEED_PURCHASE_INVALID")
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	*purchase = entry
	return nil
}

func createGardenHerbMarketSaleIfMissingInTx(tx *gorm.DB, sale *model.GardenHerbMarketSale) error {
	if tx == nil || sale == nil {
		return fmt.Errorf("GARDEN_HERB_MARKET_SALE_INVALID")
	}
	entry := *sale
	entry.SeedKey = textfmt.FormatPlainValue(entry.SeedKey)
	entry.DayKey = textfmt.FormatPlainValue(entry.DayKey)
	if entry.UserID == 0 || entry.SeedKey == "" || entry.DayKey == "" {
		return fmt.Errorf("GARDEN_HERB_MARKET_SALE_INVALID")
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	*sale = entry
	return nil
}

func gardenOpenNextPlot(userID int64) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.GardenPlot{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
			return err
		}
		if count >= gardenMaxPlots {
			return storage.ErrGardenPlotMax
		}
		nextNo := int(count) + 1
		cost := gardenPlotCosts[nextNo]
		if cost > 0 {
			if err := storage.ApplyPointDeltaInTx(tx, userID, -cost, "garden_plot_open", fmt.Sprintf("开垦第 %d 块灵田，消耗 %d 积分", nextNo, cost), "garden_plot", strconv.Itoa(nextNo)); err != nil {
				return err
			}
		}
		plot := model.GardenPlot{
			UserID:     userID,
			PlotNo:     nextNo,
			UnlockedAt: time.Now(),
		}
		if err := createGardenPlotInTx(tx, &plot); err != nil {
			return err
		}
		return nil
	})
}

func gardenBuySeed(userID int64, seedKey string) error {
	cfg, ok := GardenSeedByKey(seedKey)
	if !ok || !cfg.Purchasable {
		return storage.ErrGardenSeedNotAvailable
	}
	dayKey := storage.SignInDateKey(time.Now())

	return storage.DB.Transaction(func(tx *gorm.DB) error {
		purchase := model.GardenSeedPurchase{
			UserID:   userID,
			SeedKey:  cfg.Key,
			DayKey:   dayKey,
			Quantity: 0,
		}
		if err := createGardenSeedPurchaseIfMissingInTx(tx, &purchase); err != nil {
			return err
		}
		res := tx.Model(&model.GardenSeedPurchase{}).
			Where("user_id = ? AND seed_key = ? AND day_key = ? AND quantity < ?", userID, cfg.Key, dayKey, cfg.DailyLimit).
			UpdateColumn("quantity", gorm.Expr("quantity + 1"))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrGardenDailyLimit
		}
		if err := storage.ApplyPointDeltaInTx(tx, userID, -cfg.Price, "garden_seed_buy", fmt.Sprintf("购买【%s】，消耗 %d 积分", gardenPointDescriptionName(cfg.SeedName), cfg.Price), "garden_seed", cfg.Key); err != nil {
			return err
		}
		return GardenGrantInventoryInTx(tx, userID, cfg.SeedName, 1)
	})
}

func gardenPlantSeed(userID int64, plotNo int, seedKey string) error {
	cfg, ok := GardenSeedByKey(seedKey)
	if !ok {
		return storage.ErrGardenSeedUnknown
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var plot model.GardenPlot
		if err := tx.Where("user_id = ? AND plot_no = ?", userID, plotNo).First(&plot).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrGardenPlotNotFound
			}
			return err
		}
		var activeCount int64
		if err := tx.Model(&model.GardenPlanting{}).Where("plot_id = ? AND status = ?", plot.ID, gardenStatusGrowing).Count(&activeCount).Error; err != nil {
			return err
		}
		if activeCount > 0 {
			return storage.ErrGardenPlotBusy
		}
		res := tx.Model(&model.Inventory{}).
			Where("user_id = ? AND item_name = ? AND quantity > 0", userID, cfg.SeedName).
			UpdateColumn("quantity", gorm.Expr("quantity - 1"))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrGardenSeedNotEnough
		}
		now := time.Now()
		planting := model.GardenPlanting{
			UserID:    userID,
			PlotID:    plot.ID,
			PlotNo:    plot.PlotNo,
			SeedKey:   cfg.Key,
			SeedName:  cfg.SeedName,
			HerbName:  cfg.HerbName,
			PlantedAt: now,
			MaturesAt: now.Add(cfg.GrowDuration),
			Status:    gardenStatusGrowing,
		}
		if err := createGardenPlantingInTx(tx, &planting); err != nil {
			return err
		}
		return nil
	})
}

func gardenHarvestPlot(userID int64, plotNo int) (int, string, error) {
	var harvestedQty int
	var herbName string
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var planting model.GardenPlanting
		if err := tx.Where("user_id = ? AND plot_no = ? AND status = ?", userID, plotNo, gardenStatusGrowing).First(&planting).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrGardenNoActivePlant
			}
			return err
		}
		if planting.MaturesAt.After(time.Now()) {
			return storage.ErrGardenNotMature
		}
		cfg, ok := GardenSeedByKey(planting.SeedKey)
		if !ok {
			return storage.ErrGardenSeedUnknown
		}
		qty := security.RandomIntRange(cfg.YieldMin, cfg.YieldMax)
		now := time.Now()
		res := tx.Model(&model.GardenPlanting{}).
			Where("id = ? AND status = ?", planting.ID, gardenStatusGrowing).
			Updates(map[string]interface{}{
				"status":       gardenStatusHarvest,
				"harvested_at": now,
				"quantity":     qty,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrGardenAlreadyHarvested
		}
		if err := GardenGrantInventoryInTx(tx, userID, cfg.HerbName, qty); err != nil {
			return err
		}
		harvestedQty = qty
		herbName = cfg.HerbName
		return nil
	})
	return harvestedQty, herbName, err
}

func gardenHarvestAll(userID int64) (int, int, error) {
	var harvestedPlots int
	var harvestedQty int
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		txHarvestedPlots := 0
		txHarvestedQty := 0
		var plantings []model.GardenPlanting
		if err := tx.Where("user_id = ? AND status = ? AND matures_at <= ?", userID, gardenStatusGrowing, time.Now()).
			Order("plot_no asc").
			Find(&plantings).Error; err != nil {
			return err
		}
		if len(plantings) == 0 {
			return storage.ErrGardenNoMaturePlant
		}
		for _, planting := range plantings {
			cfg, ok := GardenSeedByKey(planting.SeedKey)
			if !ok {
				return storage.ErrGardenSeedUnknown
			}
			qty := security.RandomIntRange(cfg.YieldMin, cfg.YieldMax)
			now := time.Now()
			res := tx.Model(&model.GardenPlanting{}).
				Where("id = ? AND status = ?", planting.ID, gardenStatusGrowing).
				Updates(map[string]interface{}{
					"status":       gardenStatusHarvest,
					"harvested_at": now,
					"quantity":     qty,
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				continue
			}
			if err := GardenGrantInventoryInTx(tx, userID, cfg.HerbName, qty); err != nil {
				return err
			}
			txHarvestedPlots++
			txHarvestedQty += qty
		}
		if txHarvestedPlots == 0 {
			return storage.ErrGardenNoMaturePlant
		}
		harvestedPlots = txHarvestedPlots
		harvestedQty = txHarvestedQty
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return harvestedPlots, harvestedQty, nil
}

func GardenSellHerbQuantity(userID int64, seedKey string, requestedQty int) (int, int, error) {
	cfg, ok := GardenSeedByKey(seedKey)
	if !ok {
		return 0, 0, storage.ErrGardenHerbNotSellable
	}
	basePrice := GardenHerbBaseSellPrice(cfg)
	if basePrice <= 0 {
		return 0, 0, storage.ErrGardenHerbNotSellable
	}
	if requestedQty <= 0 {
		return 0, 0, storage.ErrGardenHerbQuantityInvalid
	}

	var gained int
	var soldQty int
	now := time.Now()
	dayKey := gardenHerbMarketDayKey(now)
	offer, hasOffer := gardenTodayHerbMarketOfferMap(now)[cfg.Key]
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		txGained := 0
		txSoldQty := 0
		qty := requestedQty
		if qty <= 0 {
			return storage.ErrGardenHerbQuantityInvalid
		}
		txSoldQty = qty

		urgentQty := 0
		urgentPrice := basePrice
		if hasOffer && offer.Price > basePrice && offer.Limit > 0 {
			marketSale := model.GardenHerbMarketSale{
				UserID:   userID,
				SeedKey:  cfg.Key,
				DayKey:   dayKey,
				Quantity: 0,
			}
			if err := createGardenHerbMarketSaleIfMissingInTx(tx, &marketSale); err != nil {
				return err
			}

			var sale model.GardenHerbMarketSale
			if err := tx.Where("user_id = ? AND seed_key = ? AND day_key = ?", userID, cfg.Key, dayKey).First(&sale).Error; err != nil {
				return err
			}
			left := offer.Limit - sale.Quantity
			if left > 0 {
				urgentQty = qty
				if urgentQty > left {
					urgentQty = left
				}
				res := tx.Model(&model.GardenHerbMarketSale{}).
					Where("user_id = ? AND seed_key = ? AND day_key = ? AND quantity <= ?", userID, cfg.Key, dayKey, offer.Limit-urgentQty).
					UpdateColumn("quantity", gorm.Expr("quantity + ?", urgentQty))
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 0 {
					urgentQty = 0
				}
				urgentPrice = offer.Price
			}
		}

		res := tx.Model(&model.Inventory{}).
			Where("user_id = ? AND item_name = ? AND quantity >= ?", userID, cfg.HerbName, qty).
			UpdateColumn("quantity", gorm.Expr("quantity - ?", qty))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrGardenHerbNotEnough
		}
		baseQty := qty - urgentQty
		txGained = baseQty*basePrice + urgentQty*urgentPrice
		desc := fmt.Sprintf("药铺回收【%s】x%d，获得 %d 积分", gardenPointDescriptionName(cfg.HerbName), qty, txGained)
		if urgentQty > 0 {
			desc = fmt.Sprintf("药铺回收【%s】x%d，急收 x%d，获得 %d 积分", gardenPointDescriptionName(cfg.HerbName), qty, urgentQty, txGained)
		}
		if err := storage.ApplyPointDeltaInTx(tx, userID, txGained, "garden_herb_sell", desc, "garden_herb", cfg.Key); err != nil {
			return err
		}
		gained = txGained
		soldQty = txSoldQty
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return gained, soldQty, nil
}

func gardenBuyRecipe(userID int64, recipeKey string) error {
	cfg, ok := gardenRecipeByKey(recipeKey)
	if !ok {
		return storage.ErrGardenRecipeUnknown
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.GardenRecipeUnlock{}).Where("user_id = ? AND recipe_key = ?", userID, cfg.Key).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return storage.ErrGardenRecipeUnlocked
		}
		if err := storage.ApplyPointDeltaInTx(tx, userID, -cfg.UnlockPrice, "garden_recipe_unlock", fmt.Sprintf("参悟【%s】，消耗 %d 积分", gardenPointDescriptionName(cfg.Name), cfg.UnlockPrice), "garden_recipe", cfg.Key); err != nil {
			return err
		}
		unlock := model.GardenRecipeUnlock{
			UserID:     userID,
			RecipeKey:  cfg.Key,
			UnlockedAt: time.Now(),
		}
		if err := createGardenRecipeUnlockInTx(tx, &unlock); err != nil {
			return err
		}
		return nil
	})
}

func gardenAlchemy(userID int64, recipeKey string) (string, error) {
	cfg, ok := gardenRecipeByKey(recipeKey)
	if !ok {
		return "", storage.ErrGardenRecipeUnknown
	}

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.GardenRecipeUnlock{}).Where("user_id = ? AND recipe_key = ?", userID, cfg.Key).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return storage.ErrGardenRecipeLocked
		}
		for _, mat := range cfg.Materials {
			// 库存按物品原名（plain）存储与查询，此处扣减也必须用原名。
			// 之前误用 inventoryItemMarkdownName（markdown 转义）做 WHERE 匹配，
			// 当前因素材名不含 markdown 特殊字符而侥幸生效；一旦后续素材名
			// 含 _ * [ 反引号 等字符，将匹配不到已持有库存导致炼丹静默失败。
			res := tx.Model(&model.Inventory{}).
				Where("user_id = ? AND item_name = ? AND quantity >= ?", userID, mat.ItemName, mat.Quantity).
				UpdateColumn("quantity", gorm.Expr("quantity - ?", mat.Quantity))
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return storage.ErrGardenMaterialNotEnough
			}
		}
		if cfg.AlchemyCost > 0 {
			if err := storage.ApplyPointDeltaInTx(tx, userID, -cfg.AlchemyCost, "garden_alchemy_cost", fmt.Sprintf("炼制【%s】，炉火消耗 %d 积分", gardenPointDescriptionName(cfg.ProductName), cfg.AlchemyCost), "garden_recipe", cfg.Key); err != nil {
				return err
			}
		}
		return GardenGrantInventoryInTx(tx, userID, cfg.ProductName, 1)
	})
	if err != nil {
		return "", err
	}
	return cfg.ProductName, nil
}

func GardenGrantInventoryInTx(tx *gorm.DB, userID int64, itemName string, quantity int) error {
	itemName = strings.TrimSpace(itemName)
	if tx == nil || userID == 0 || itemName == "" || quantity <= 0 {
		return fmt.Errorf("INVALID_INVENTORY_GRANT")
	}
	res := tx.Clauses(InventoryQuantityUpsertClause(quantity)).Create(&model.Inventory{
		UserID:   userID,
		ItemName: itemName,
		Quantity: quantity,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("INVENTORY_GRANT_MISSED")
	}
	return nil
}

func InventoryQuantityUpsertClause(quantity int) clause.OnConflict {
	return clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "item_name"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{
			clause.Eq{Column: clause.Column{Name: "deleted_at"}, Value: nil},
		}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"quantity":   gorm.Expr("quantity + ?", quantity),
			"updated_at": time.Now(),
		}),
	}
}

func gardenUserPlotsWithError(userID int64) ([]model.GardenPlot, error) {
	var plots []model.GardenPlot
	if err := storage.DB.Where("user_id = ?", userID).Order("plot_no asc").Find(&plots).Error; err != nil {
		return plots, err
	}
	return plots, nil
}

func gardenActivePlantingsByPlotWithError(userID int64) (map[uint]model.GardenPlanting, error) {
	var plantings []model.GardenPlanting
	if err := storage.DB.Where("user_id = ? AND status = ?", userID, gardenStatusGrowing).Find(&plantings).Error; err != nil {
		return map[uint]model.GardenPlanting{}, err
	}
	result := make(map[uint]model.GardenPlanting, len(plantings))
	for _, planting := range plantings {
		result[planting.PlotID] = planting
	}
	return result, nil
}

func gardenInventoryByNamesWithError(userID int64, names []string) (map[string]int, error) {
	result := make(map[string]int)
	if len(names) == 0 {
		return result, nil
	}
	var items []model.Inventory
	if err := storage.DB.Where("user_id = ? AND item_name IN ? AND quantity > 0", userID, names).Find(&items).Error; err != nil {
		return result, err
	}
	for _, item := range items {
		result[item.ItemName] = item.Quantity
	}
	return result, nil
}

func gardenInventoryTotalWithError(userID int64, names []string) (int, error) {
	total := 0
	items, err := gardenInventoryByNamesWithError(userID, names)
	if err != nil {
		return 0, err
	}
	for _, qty := range items {
		total += qty
	}
	return total, nil
}

func gardenTodaySeedPurchasesWithError(userID int64, dayKey string) (map[string]int, error) {
	var rows []model.GardenSeedPurchase
	if err := storage.DB.Where("user_id = ? AND day_key = ?", userID, dayKey).Find(&rows).Error; err != nil {
		return map[string]int{}, err
	}
	result := make(map[string]int, len(rows))
	for _, row := range rows {
		result[row.SeedKey] = row.Quantity
	}
	return result, nil
}

func gardenTodayHerbMarketSalesWithError(userID int64, dayKey string) (map[string]int, error) {
	var rows []model.GardenHerbMarketSale
	if err := storage.DB.Where("user_id = ? AND day_key = ?", userID, dayKey).Find(&rows).Error; err != nil {
		return map[string]int{}, err
	}
	result := make(map[string]int, len(rows))
	for _, row := range rows {
		result[row.SeedKey] = row.Quantity
	}
	return result, nil
}

func gardenUnlockedRecipesWithError(userID int64) (map[string]bool, error) {
	var rows []model.GardenRecipeUnlock
	if err := storage.DB.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return map[string]bool{}, err
	}
	result := make(map[string]bool, len(rows))
	for _, row := range rows {
		result[row.RecipeKey] = true
	}
	return result, nil
}

func gardenSeedItemNames() []string {
	names := make([]string, 0, len(GardenSeeds))
	for _, cfg := range GardenSeeds {
		names = append(names, cfg.SeedName)
	}
	sort.Strings(names)
	return names
}

func gardenHerbItemNames() []string {
	names := make([]string, 0, len(GardenSeeds))
	for _, cfg := range GardenSeeds {
		names = append(names, cfg.HerbName)
	}
	sort.Strings(names)
	return names
}

func gardenPillItemNames() []string {
	names := make([]string, 0, len(GardenRecipes))
	for _, cfg := range GardenRecipes {
		names = append(names, cfg.ProductName)
	}
	sort.Strings(names)
	return names
}

func gardenDurationText(d time.Duration) string {
	if d <= 0 {
		return "已成熟"
	}
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%d小时%d分", h, m)
	}
	return fmt.Sprintf("%d分", m)
}

func gardenYieldText(cfg gardenSeedConfig) string {
	if cfg.YieldMin <= 0 || cfg.YieldMax <= 0 {
		return "未知"
	}
	if cfg.YieldMin == cfg.YieldMax {
		return fmt.Sprintf("%d株", cfg.YieldMin)
	}
	return fmt.Sprintf("%d-%d株", cfg.YieldMin, cfg.YieldMax)
}

func gardenErrorCode(err error) string {
	switch {
	case errors.Is(err, storage.ErrPointsNotEnough):
		return "POINTS_NOT_ENOUGH"
	case errors.Is(err, storage.ErrUserNotFound):
		return "USER_NOT_FOUND"
	case errors.Is(err, storage.ErrGardenPlotMax):
		return "GARDEN_PLOT_MAX"
	case errors.Is(err, storage.ErrGardenDailyLimit):
		return "GARDEN_DAILY_LIMIT"
	case errors.Is(err, storage.ErrGardenSeedNotAvailable):
		return "GARDEN_SEED_NOT_AVAILABLE"
	case errors.Is(err, storage.ErrGardenSeedUnknown):
		return "GARDEN_SEED_UNKNOWN"
	case errors.Is(err, storage.ErrGardenPlotNotFound):
		return "GARDEN_PLOT_NOT_FOUND"
	case errors.Is(err, storage.ErrGardenPlotBusy):
		return "GARDEN_PLOT_BUSY"
	case errors.Is(err, storage.ErrGardenNoEmptyPlot):
		return "GARDEN_NO_EMPTY_PLOT"
	case errors.Is(err, storage.ErrGardenSeedNotEnough):
		return "GARDEN_SEED_NOT_ENOUGH"
	case errors.Is(err, storage.ErrGardenNoActivePlant):
		return "GARDEN_NO_ACTIVE_PLANT"
	case errors.Is(err, storage.ErrGardenNotMature):
		return "GARDEN_NOT_MATURE"
	case errors.Is(err, storage.ErrGardenAlreadyHarvested):
		return "GARDEN_ALREADY_HARVESTED"
	case errors.Is(err, storage.ErrGardenNoMaturePlant):
		return "GARDEN_NO_MATURE_PLANT"
	case errors.Is(err, storage.ErrGardenHerbNotSellable):
		return "GARDEN_HERB_NOT_SELLABLE"
	case errors.Is(err, storage.ErrGardenHerbNotEnough):
		return "GARDEN_HERB_NOT_ENOUGH"
	case errors.Is(err, storage.ErrGardenHerbQuantityInvalid):
		return "GARDEN_HERB_QUANTITY_INVALID"
	case errors.Is(err, storage.ErrGardenRecipeUnknown):
		return "GARDEN_RECIPE_UNKNOWN"
	case errors.Is(err, storage.ErrGardenRecipeUnlocked):
		return "GARDEN_RECIPE_UNLOCKED"
	case errors.Is(err, storage.ErrGardenRecipeLocked):
		return "GARDEN_RECIPE_LOCKED"
	case errors.Is(err, storage.ErrGardenMaterialNotEnough):
		return "GARDEN_MATERIAL_NOT_ENOUGH"
	case err != nil:
		return storage.FallbackBusinessErrorCode(err)
	default:
		return ""
	}
}

func GardenActionErrorText(err error) string {
	if err == nil {
		return ""
	}
	if storage.IsUniqueConstraintError(err) {
		return "操作过于频繁，请刷新药园后重试"
	}
	switch gardenErrorCode(err) {
	case "POINTS_NOT_ENOUGH":
		return "积分不足"
	case "USER_NOT_FOUND":
		return "未找到积分账户"
	case "GARDEN_PLOT_MAX":
		return "灵田已达上限"
	case "GARDEN_DAILY_LIMIT":
		return "今日种子限购已满"
	case "GARDEN_SEED_NOT_AVAILABLE":
		return "该种子暂不售卖"
	case "GARDEN_SEED_UNKNOWN":
		return "种子配置不存在"
	case "GARDEN_PLOT_NOT_FOUND":
		return "灵田不存在"
	case "GARDEN_PLOT_BUSY":
		return "这块灵田已有药草"
	case "GARDEN_NO_EMPTY_PLOT":
		return "暂无空闲灵田"
	case "GARDEN_SEED_NOT_ENOUGH":
		return "种子不足"
	case "GARDEN_NO_ACTIVE_PLANT":
		return "这块灵田没有可收获药草"
	case "GARDEN_NOT_MATURE":
		return "药草尚未成熟"
	case "GARDEN_ALREADY_HARVESTED":
		return "药草已被收获"
	case "GARDEN_NO_MATURE_PLANT":
		return "暂无成熟药草"
	case "GARDEN_HERB_NOT_SELLABLE":
		return "该药草暂不可回收"
	case "GARDEN_HERB_NOT_ENOUGH":
		return "药草不足"
	case "GARDEN_HERB_QUANTITY_INVALID":
		return "回收数量异常"
	case "GARDEN_RECIPE_UNKNOWN":
		return "丹方不存在"
	case "GARDEN_RECIPE_UNLOCKED":
		return "丹方已学会"
	case "GARDEN_RECIPE_LOCKED":
		return "尚未参悟丹方"
	case "GARDEN_MATERIAL_NOT_ENOUGH":
		return "炼丹材料不足"
	default:
		log.Printf("⚠️ 药园操作失败: %s", textfmt.FormatPlainError(err))
		return "操作失败，请稍后再试"
	}
}

func StartGardenMaturityNotifier(bot *tgbotapi.BotAPI) {
	go func() {
		notifyGardenMaturePlantings(bot, time.Now())

		ticker := time.NewTicker(gardenMaturityNoticeInterval)
		defer ticker.Stop()
		for now := range ticker.C {
			notifyGardenMaturePlantings(bot, now)
		}
	}()

	log.Println("✅ 药园成熟提醒调度器已启动：每分钟巡检成熟灵田")
}

func notifyGardenMaturePlantings(bot *tgbotapi.BotAPI, now time.Time) {
	if bot == nil || storage.DB == nil {
		return
	}

	var plantings []model.GardenPlanting
	if err := storage.DB.
		Where("status = ? AND mature_notified_at IS NULL AND matures_at <= ?", gardenStatusGrowing, now).
		Order("matures_at ASC, id ASC").
		Limit(gardenMaturityNoticeBatchSize).
		Find(&plantings).Error; err != nil {
		log.Printf("⚠️ 查询药园成熟提醒失败: err=%s", textfmt.FormatPlainError(err))
		return
	}
	if len(plantings) == 0 {
		return
	}

	claimedByUser := make(map[int64][]model.GardenPlanting)
	for _, planting := range plantings {
		claimed, err := claimGardenMaturityNotice(planting.ID, now)
		if err != nil {
			log.Printf("⚠️ 标记药园成熟提醒失败: planting=%d user=%d err=%s", planting.ID, planting.UserID, textfmt.FormatPlainError(err))
			continue
		}
		if !claimed {
			continue
		}
		claimedByUser[planting.UserID] = append(claimedByUser[planting.UserID], planting)
	}

	for userID, rows := range claimedByUser {
		if err := sendGardenMaturityNotice(bot, userID, rows); err != nil {
			log.Printf("⚠️ 药园成熟提醒私聊失败: user=%d count=%d err=%s", userID, len(rows), textfmt.FormatTelegramSendError(err))
		}
	}
}

func claimGardenMaturityNotice(plantingID uint, now time.Time) (bool, error) {
	res := storage.DB.Model(&model.GardenPlanting{}).
		Where("id = ? AND status = ? AND mature_notified_at IS NULL AND matures_at <= ?", plantingID, gardenStatusGrowing, now).
		Update("mature_notified_at", now)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func sendGardenMaturityNotice(bot *tgbotapi.BotAPI, userID int64, plantings []model.GardenPlanting) error {
	msg := tgbotapi.NewMessage(userID, gardenMaturityNoticeText(plantings))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = gardenMaturityNoticeMarkup()
	_, err := bot.Send(telegram.PrepareCopyable(msg))
	return err
}

func gardenMaturityNoticeText(plantings []model.GardenPlanting) string {
	rows := append([]model.GardenPlanting(nil), plantings...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].PlotNo == rows[j].PlotNo {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].PlotNo < rows[j].PlotNo
	})

	var b strings.Builder
	b.WriteString("🌾 **【灵田成熟提醒】**\n\n")
	if len(rows) == 1 {
		p := rows[0]
		b.WriteString(fmt.Sprintf("`%d` 号灵田的 %s 已成熟。\n", p.PlotNo, textfmt.InventoryItemMarkdownName(p.HerbName)))
	} else {
		b.WriteString("以下灵田药草已成熟：\n")
		for _, p := range rows {
			b.WriteString(fmt.Sprintf("- `%d` 号灵田：%s\n", p.PlotNo, textfmt.InventoryItemMarkdownName(p.HerbName)))
		}
	}
	b.WriteString("\n可进入 `药园` 的灵田管理收获。")
	return b.String()
}

func gardenMaturityNoticeMarkup() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("前往灵田", "garden:fields"),
		),
	)
}

func sendGardenScreen(bot *tgbotapi.BotAPI, chatID int64, text string, markup tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = markup
	if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
		log.Printf("发送药园菜单失败: chat=%d err=%s", chatID, textfmt.FormatTelegramSendError(err))
	}
}

func editGardenScreen(bot *tgbotapi.BotAPI, chatID int64, messageID int, text string, markup tgbotapi.InlineKeyboardMarkup) {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &markup
	if _, err := bot.Send(telegram.PrepareCopyable(edit)); err != nil {
		log.Printf("编辑药园菜单失败: chat=%d message=%d err=%s", chatID, messageID, textfmt.FormatTelegramSendError(err))
	}
}

func gardenBackHomeMarkup() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回药园", "garden:home"),
		),
	)
}
