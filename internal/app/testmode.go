package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/buildinfo"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

func prepareTestDirectory() error {
	c := config.AppConfig
	if !c.IsTest() {
		return nil
	}
	if err := os.MkdirAll(c.TestDataDir, 0700); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(c.TestDataDir)
	if err != nil {
		return err
	}
	expected, err := filepath.Abs(c.TestDataDir)
	if err != nil {
		return err
	}
	actual, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("test data directory must not be a symlink")
	}
	marker := filepath.Join(root, ".test-environment")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("refusing unmarked, nonempty test directory")
		}
		if err := os.WriteFile(marker, []byte("ccoff-abs-bot test data only\n"), 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	info, err := os.Lstat(c.DbURL)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("test database must not be a symlink")
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(c.DbURL))
	if err != nil {
		return err
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return err
	}
	if parent != actual {
		return fmt.Errorf("test database must be directly inside TEST_DATA_DIR")
	}
	return nil
}

func testMessageAllowed(msg *tgbotapi.Message) bool {
	return msg != nil && msg.From != nil && msg.Chat != nil && msg.Chat.IsPrivate() && msg.Chat.ID == msg.From.ID && config.AppConfig.AdminIDs[msg.From.ID]
}

func testUpdateAllowed(update tgbotapi.Update) bool {
	if !config.AppConfig.IsTest() {
		return true
	}
	if update.Message != nil {
		return testMessageAllowed(update.Message)
	}
	cb := update.CallbackQuery
	return cb != nil && cb.From != nil && cb.Message != nil && cb.Message.Chat != nil && cb.Message.Chat.IsPrivate() && cb.Message.Chat.ID == cb.From.ID && config.AppConfig.AdminIDs[cb.From.ID]
}

// Guard the HTTP boundary as well as incoming updates, including background sends.
func guardTestTelegramRequest(req *http.Request) error {
	if !config.AppConfig.IsTest() {
		return nil
	}
	var body []byte
	var err error
	if req.Body != nil {
		body, err = io.ReadAll(io.LimitReader(req.Body, (8<<20)+1))
		_ = req.Body.Close()
	}
	if err != nil {
		return err
	}
	if len(body) > 8<<20 {
		return fmt.Errorf("test Telegram request too large")
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	copyReq := req.Clone(req.Context())
	copyReq.Body = io.NopCloser(bytes.NewReader(body))
	if strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/") {
		if err := copyReq.ParseMultipartForm(1 << 20); err != nil {
			return err
		}
		defer copyReq.MultipartForm.RemoveAll()
	} else {
		if len(body) > 0 && !strings.HasPrefix(req.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			return fmt.Errorf("test Telegram request encoding unsupported")
		}
		if err := copyReq.ParseForm(); err != nil {
			return err
		}
	}
	for _, key := range []string{"chat_id", "from_chat_id"} {
		for _, value := range copyReq.Form[key] {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 || !config.AppConfig.AdminIDs[id] {
				return fmt.Errorf("test mode forbids Telegram destination")
			}
		}
	}
	return nil
}

func handleTestCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) bool {
	if !config.AppConfig.IsTest() || !testMessageAllowed(msg) {
		return false
	}
	args := strings.Fields(msg.Text)
	if len(args) == 0 || args[0] != "/test" {
		return false
	}
	reply := func(s string) {
		telegram.SendPlainText(bot, msg.Chat.ID, "🧪 测试环境 "+buildinfo.Version+"\n"+s)
	}
	if abs.DefaultClient == nil || abs.DefaultClient.HttpClient == nil {
		reply("模拟 ABS 未初始化")
		return true
	}
	mock, ok := abs.DefaultClient.HttpClient.Transport.(*abs.MockTransport)
	if !ok {
		reply("模拟 ABS 未初始化")
		return true
	}
	var err error
	switch {
	case len(args) == 2 && args[1] == "seed":
		var current model.User
		err = storage.DB.Where("telegram_id = ?", msg.From.ID).First(&current).Error
		if err == nil {
			reply("已有本地档案，保持原样。需要重新开户测试时，请通过测试 Bot 注销，或使用本地 reset 命令重置全部测试数据。")
			return true
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		var id string
		id, err = mock.SeedUser(msg.From.ID)
		if err != nil {
			break
		}
		expiry := time.Now().AddDate(0, 0, 30)
		user := model.User{TelegramID: msg.From.ID, Username: fmt.Sprintf("demo%d", msg.From.ID), AbsUserID: id, SecurityCode: security.HashSensitiveToken("TestSecurity123!"), Points: 1000, Status: "active", AccountType: "formal", ExpireAt: &expiry}
		err = storage.DB.Create(&user).Error
		if err == nil {
			reply("已创建你的测试档案：1000 积分、30 天有效期。\n用户名：" + user.Username + "\n模拟 ABS 密码：TestPassword123!\n安全码：TestSecurity123!\n以上均为测试专用。")
			return true
		}
	case len(args) == 4 && args[1] == "listening":
		var user model.User
		err = storage.DB.Where("telegram_id = ?", msg.From.ID).First(&user).Error
		if err != nil {
			break
		}
		var hours float64
		hours, err = strconv.ParseFloat(args[3], 64)
		if err == nil {
			err = mock.SetListening(user.AbsUserID, args[2], hours)
		}
		if err == nil {
			reply("已设置模拟听书时长，可打开听书报告刷新。")
			return true
		}
	case len(args) == 3 && args[1] == "failure":
		err = mock.SetFailure(args[2])
		if err == nil {
			reply("模拟 ABS 故障模式：" + args[2])
			return true
		}
	default:
		reply("/test seed — 创建测试档案（已有则保留）\n/test listening YYYY-MM-DD 小时 — 设置某日听书时长\n/test failure off|timeout|500|invalid-json — 模拟故障\n全部重置：在 Mac 执行 ./scripts/test-bot.sh reset --confirm，仅清理测试数据。")
		return true
	}
	reply(fmt.Sprintf("操作失败：%v", err))
	return true
}
