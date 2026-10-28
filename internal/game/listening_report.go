package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func GetPersonalReport(c *abs.AbsClient, absUserID string) string {
	// ==========================================
	// 1. 获取防作弊的真实听书时长与每日明细 (来自统计接口)
	// ==========================================
	statsEndpoint := abs.AbsUserListeningStatsPath(absUserID)
	statsBody, statsCode, statsErr := c.SendRequest("GET", statsEndpoint, nil)
	if statsErr != nil {
		log.Printf("⚠️ 听书报告 ABS 统计读取失败: abs=%s err=%s", textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(statsErr))
		return "❌ 听书统计暂时读取失败，请稍后再试。"
	}
	if statsCode != 200 {
		log.Printf("⚠️ 听书报告 ABS 统计状态异常: abs=%s status=%d", textfmt.FormatPlainValue(absUserID), statsCode)
		return "❌ 听书统计暂时读取失败，请稍后再试。"
	}

	var rawTotalSeconds float64 = 0
	var effectiveTotalHours float64 = 0 // 经过天道法则压制后的总修为时长
	var todayEffectiveHours float64 = 0
	var todayRawHours float64 = 0
	var statsDays map[string]float64

	// 精准映射 ABS 的统计数据结构
	var stats struct {
		TotalTime float64            `json:"totalTime"`
		Days      map[string]float64 `json:"days"`
	}

	if err := json.Unmarshal(statsBody, &stats); err != nil {
		log.Printf("⚠️ 听书报告 ABS 统计解析失败: abs=%s err=%s", textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(err))
		return "❌ 听书统计暂时读取失败，请稍后再试。"
	}

	// 累计实际时长优先使用 ABS 官方 totalTime；未提供累计值时汇总官方 days。
	rawTotalSeconds = abs.AuthoritativeABSListeningTotalSeconds(stats.TotalTime, stats.Days)

	statsDays = stats.Days
	effectiveTotalHours = calculateEffectiveCultivationHoursFromABSDays(stats.Days)
	todaySeconds := abs.PositiveListeningSeconds(stats.Days[SectDayKey(time.Now())])
	todayRawHours = todaySeconds / 3600.0
	todayEffectiveHours = calculateSectEffectiveHoursFromSeconds(todaySeconds)

	if rawTotalSeconds <= 0 {
		return "💤 暂无您的有效收听记录，去听本书再来吧！"
	}

	// ==========================================
	// 2. 获取书籍完成状态 (保持原逻辑不变)
	// ==========================================
	userEndpoint := abs.AbsUserPath(absUserID)
	userBody, userCode, userErr := c.SendRequest("GET", userEndpoint, nil)

	finishedCount := 0
	listeningCount := 0
	finishedCountText := "读取失败"
	listeningCountText := "读取失败"

	if userErr != nil {
		log.Printf("⚠️ 听书报告 ABS 书籍进度读取失败: abs=%s err=%s", textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(userErr))
	} else if userCode != 200 {
		log.Printf("⚠️ 听书报告 ABS 书籍进度状态异常: abs=%s status=%d", textfmt.FormatPlainValue(absUserID), userCode)
	} else {
		var userData struct {
			MediaProgress []struct {
				IsFinished bool `json:"isFinished"`
			} `json:"mediaProgress"`
		}
		if err := json.Unmarshal(userBody, &userData); err != nil {
			log.Printf("⚠️ 听书报告 ABS 书籍进度解析失败: abs=%s err=%s", textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(err))
		} else {
			for _, mp := range userData.MediaProgress {
				if mp.IsFinished {
					finishedCount++
				} else {
					listeningCount++
				}
			}
			finishedCountText = strconv.Itoa(finishedCount)
			listeningCountText = strconv.Itoa(listeningCount)
		}
	}

	// 👇 --- 核心新增：注入经过折算的斗气系统 --- 👇
	var u model.User
	if err := storage.DB.Where("abs_user_id = ?", absUserID).First(&u).Error; err == nil {
		now := time.Now()
		if statsDays == nil {
			log.Printf("⚠️ ABS 听书统计缺少 days 明细，跳过净修为同步: user=%d abs=%s", u.TelegramID, textfmt.FormatPlainValue(absUserID))
			if cul := GetOrCreateCultivation(u.TelegramID); cul != nil {
				effectiveTotalHours = cul.TotalAudioTime
			}
		} else {
			if err := recordDailyListeningStatsFromABSDays(u.TelegramID, absUserID, statsDays, now); err == nil {
				if syncedEffectiveTotalHours, ok := syncCultivationFromDailyListeningStatsAt(u.TelegramID, now); ok {
					effectiveTotalHours = syncedEffectiveTotalHours
				} else if cul := GetOrCreateCultivation(u.TelegramID); cul != nil {
					effectiveTotalHours = cul.TotalAudioTime
				}
				if todayStat, ok := getTodayDailyListeningStat(u.TelegramID, now); ok {
					todayRawHours = abs.PositiveListeningSeconds(todayStat.RawSeconds) / 3600.0
					todayEffectiveHours = todayStat.EffectiveHours + ActiveSectCaveRetreatBonusHours(u.TelegramID, now)
				}
			} else {
				log.Printf("⚠️ 听书报告每日统计写入失败，使用本次 ABS 数据降级展示: user=%d abs=%s err=%s",
					u.TelegramID, textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(err))
			}
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("⚠️ 听书报告本地用户读取失败，跳过净修为同步: abs=%s err=%s", textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(err))
	}
	// 👆 --------------------------------------- 👆

	// ==========================================
	// 3. 严格按照专属排版输出 (展示真假时长的差异)
	// ==========================================
	rawHours := rawTotalSeconds / 3600.0

	// 如果由于天道压制导致折算时长明显小于真实时长，我们在文案上给予反馈
	suppressNotice := ""
	if rawHours-effectiveTotalHours > 1.0 {
		suppressNotice = "\n*(⚠️ 天道法则感应：您的部分修炼涉嫌强行吸收灵气，转化率已受压制)*"
	}

	return fmt.Sprintf("📊 你的专属听书战绩\n\n🎧 累计实际听书: %.2f 小时\n🕒 今日实际听书: %.2f 小时\n✨ 净修炼时长: %.2f 小时\n🌅 今日净修为: %.2f 小时%s\n📚 已经听完书籍: %s 本\n📖 当前正在收听: %s 本",
		rawHours, todayRawHours, effectiveTotalHours, todayEffectiveHours, suppressNotice, finishedCountText, listeningCountText)
}
