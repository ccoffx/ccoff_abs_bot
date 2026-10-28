package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"ccoff-abs-bot/internal/textfmt"
)

type Config struct {
	AppEnv                string
	ABSMode               string
	TestDataDir           string
	TestBotUsername       string
	TgToken               string
	AdminIDs              map[int64]bool
	AbsURL                string
	AbsKey                string
	AbsTemplateID         string
	NoticeGroupID         int64
	BackupGroupID         int64
	BackupEncryptKey      string
	SecurityPepper        string
	NotifyBeforeDays      int
	InviteRequired        bool
	AccountValidDays      int
	AccountGraceDays      int
	DbURL                 string
	WorkerCount           int
	QueueCapacity         int
	DatabaseMaxOpenConns  int
	DatabaseMaxIdleConns  int
	DatabaseBusyTimeoutMS int
	StartupNotifyAdmins   bool
}

var AppConfig *Config

func LoadConfig() {
	_ = godotenv.Load()
	loadConfig(os.LookupEnv)
}

// LoadTestConfig reads only the selected test file. Neither production .env
// values nor application settings exported by the shell can leak into it.
func LoadTestConfig(path string) error {
	values, err := godotenv.Read(path)
	if err != nil {
		return fmt.Errorf("read test config %s: %w", path, err)
	}
	if values["APP_ENV"] != "test" || values["ABS_MODE"] != "mock" {
		return fmt.Errorf("test config requires APP_ENV=test and ABS_MODE=mock")
	}
	loadConfig(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	return nil
}

type envSource struct{ lookup func(string) (string, bool) }

func loadConfig(lookup func(string) (string, bool)) {
	source := envSource{lookup: lookup}
	getEnv := source.getEnv
	getEnvAsInt := source.getEnvAsInt
	getEnvAsInt64 := source.getEnvAsInt64
	getEnvAsBool := source.getEnvAsBool

	AppConfig = &Config{
		AppEnv:                getEnv("APP_ENV", "production"),
		ABSMode:               getEnv("ABS_MODE", "real"),
		TestDataDir:           getEnv("TEST_DATA_DIR", "data-test"),
		TestBotUsername:       strings.TrimPrefix(getEnv("TEST_BOT_USERNAME", ""), "@"),
		TgToken:               getEnv("TELEGRAM_BOT_TOKEN", ""),
		AbsURL:                strings.TrimRight(strings.TrimSpace(getEnv("ABS_API_URL", "")), "/"),
		AbsKey:                getEnv("ABS_API_KEY", ""),
		AbsTemplateID:         getEnv("ABS_TEMPLATE_USER_ID", ""),
		NoticeGroupID:         getEnvAsInt64("NOTICE_GROUP_ID", 0),
		BackupGroupID:         getEnvAsInt64("BACKUP_GROUP_ID", 0),
		BackupEncryptKey:      getEnv("BACKUP_ENCRYPT_KEY", ""),
		SecurityPepper:        getEnv("SECURITY_PEPPER", ""),
		NotifyBeforeDays:      getEnvAsInt("NOTIFY_BEFORE_DAYS", 3),
		InviteRequired:        getEnvAsBool("INVITE_REQUIRED", true),
		AccountValidDays:      getEnvAsInt("ACCOUNT_VALID_DAYS", 30),
		AccountGraceDays:      getEnvAsInt("ACCOUNT_GRACE_DAYS", 7),
		DbURL:                 getEnv("DATABASE_URL", "data/bot_data.db"), // 修正默认路径避免找不到文件
		WorkerCount:           getEnvAsInt("WORKER_COUNT", 30),
		QueueCapacity:         getEnvAsInt("QUEUE_CAPACITY", 10000),
		DatabaseMaxOpenConns:  getEnvAsInt("DATABASE_MAX_OPEN_CONNS", 10),
		DatabaseMaxIdleConns:  getEnvAsInt("DATABASE_MAX_IDLE_CONNS", 10),
		DatabaseBusyTimeoutMS: getEnvAsInt("DATABASE_BUSY_TIMEOUT_MS", 10000),
		StartupNotifyAdmins:   getEnvAsBool("BOT_STARTUP_NOTIFY_ADMINS", false),
	}
	if err := AppConfig.ValidateEnvironment(); err != nil {
		log.Fatal(err)
	}
	if AppConfig.IsTest() {
		AppConfig.AbsURL = "https://abs.mock.invalid"
		AppConfig.AbsKey = "mock"
		AppConfig.AbsTemplateID = "mock-template"
	}

	AppConfig.AdminIDs = make(map[int64]bool)
	adminStr := getEnv("ADMIN_TELEGRAM_IDS", "")
	if adminStr != "" {
		for _, idStr := range strings.Split(adminStr, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64); err == nil {
				AppConfig.AdminIDs[id] = true
			}
		}
	}

	validateConfig(getEnvAsBool("ALLOW_INSECURE_ABS_HTTP", false))
}

func (c *Config) IsTest() bool { return c != nil && c.AppEnv == "test" }

// ValidateEnvironment runs before any database or network access.
func (c *Config) ValidateEnvironment() error {
	if c.AppEnv != "production" && c.AppEnv != "test" {
		return fmt.Errorf("APP_ENV must be production or test")
	}
	if c.ABSMode != "real" && c.ABSMode != "mock" {
		return fmt.Errorf("ABS_MODE must be real or mock")
	}
	if !c.IsTest() {
		if c.ABSMode == "mock" {
			return fmt.Errorf("production forbids ABS_MODE=mock")
		}
		return nil
	}
	if c.ABSMode != "mock" {
		return fmt.Errorf("test requires ABS_MODE=mock")
	}
	if c.TestBotUsername == "" {
		return fmt.Errorf("test requires TEST_BOT_USERNAME (independent test Bot username)")
	}
	if c.NoticeGroupID != 0 || c.BackupGroupID != 0 {
		return fmt.Errorf("test requires NOTICE_GROUP_ID=0 and BACKUP_GROUP_ID=0")
	}
	root, err := filepath.Abs(c.TestDataDir)
	if err != nil {
		return err
	}
	if c.TestDataDir == "" || filepath.Base(root) != "data-test" {
		return fmt.Errorf("TEST_DATA_DIR must end with data-test")
	}
	if strings.ContainsAny(c.DbURL, "?\x00") || strings.HasPrefix(c.DbURL, "file:") {
		return fmt.Errorf("test DATABASE_URL must be a plain file path")
	}
	dbPath, err := filepath.Abs(c.DbURL)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, dbPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("test DATABASE_URL must be inside TEST_DATA_DIR")
	}
	if (c.AbsURL != "" && c.AbsURL != "https://abs.mock.invalid") || (c.AbsKey != "" && c.AbsKey != "mock") || (c.AbsTemplateID != "" && c.AbsTemplateID != "mock-template") {
		return fmt.Errorf("test forbids real ABS configuration; use the dedicated test env file")
	}
	return nil
}

func validateConfig(allowInsecureABS bool) {
	var missing []string

	if strings.TrimSpace(AppConfig.TgToken) == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if strings.TrimSpace(AppConfig.AbsURL) == "" {
		missing = append(missing, "ABS_API_URL")
	}
	if strings.TrimSpace(AppConfig.AbsKey) == "" {
		missing = append(missing, "ABS_API_KEY")
	}
	if strings.TrimSpace(AppConfig.AbsTemplateID) == "" {
		missing = append(missing, "ABS_TEMPLATE_USER_ID")
	}
	if len(AppConfig.AdminIDs) == 0 {
		missing = append(missing, "ADMIN_TELEGRAM_IDS")
	}
	if strings.TrimSpace(AppConfig.BackupEncryptKey) == "" {
		missing = append(missing, "BACKUP_ENCRYPT_KEY")
	}
	if strings.TrimSpace(AppConfig.SecurityPepper) == "" {
		missing = append(missing, "SECURITY_PEPPER")
	}

	if len(missing) > 0 {
		log.Fatalf("❌ 关键配置缺失，禁止启动。请检查: %s", strings.Join(missing, ", "))
	}

	if !isValidAbsAPIURL(AppConfig.AbsURL) {
		log.Fatalf("❌ ABS_API_URL 格式错误，必须是 http:// 或 https:// 开头的有效基础 URL，且不能包含空白、控制字符、URL 账号密码信息、查询参数或 fragment，当前值: url=%s", textfmt.FormatPlainValue(AppConfig.AbsURL))
	}

	if isAbsAPIHTTPURL(AppConfig.AbsURL) {
		if !allowInsecureABS {
			log.Fatalf("❌ 生产环境禁止使用明文 HTTP ABS_API_URL，请改为 https://。如仅本地测试，请显式设置 ALLOW_INSECURE_ABS_HTTP=true")
		}
		log.Printf("⚠️ 当前允许使用明文 HTTP ABS_API_URL，仅建议本地测试环境使用")
	}

	if len([]byte(AppConfig.BackupEncryptKey)) < 32 {
		log.Fatalf("❌ BACKUP_ENCRYPT_KEY 太短，建议至少 32 字节，最好 64 字符以上")
	}

	if len([]byte(AppConfig.SecurityPepper)) < 32 {
		log.Fatalf("❌ SECURITY_PEPPER 太短，建议至少 32 字节，最好 64 字符以上")
	}

	if AppConfig.BackupEncryptKey == AppConfig.SecurityPepper {
		log.Printf("🔐 SECURITY_PEPPER 指纹: %s", secretFingerprint(AppConfig.SecurityPepper))
		log.Printf("🔐 BACKUP_ENCRYPT_KEY 指纹: %s", secretFingerprint(AppConfig.BackupEncryptKey))
		log.Fatalf("❌ BACKUP_ENCRYPT_KEY 和 SECURITY_PEPPER 不能相同，请分别设置两个不同密钥")
	}

	if AppConfig.AccountValidDays < 0 {
		log.Fatalf("❌ ACCOUNT_VALID_DAYS 不能为负数")
	}

	if AppConfig.AccountGraceDays < 0 {
		log.Fatalf("❌ ACCOUNT_GRACE_DAYS 不能为负数")
	}

	if AppConfig.NotifyBeforeDays < 0 {
		log.Fatalf("❌ NOTIFY_BEFORE_DAYS 不能为负数")
	}

	if AppConfig.WorkerCount < 1 {
		log.Printf("⚠️ WORKER_COUNT 小于 1，已自动修正为 1")
		AppConfig.WorkerCount = 1
	}
	if AppConfig.WorkerCount > 100 {
		log.Printf("⚠️ WORKER_COUNT 过大，已自动限制为 100")
		AppConfig.WorkerCount = 100
	}

	if AppConfig.QueueCapacity < 100 {
		log.Printf("⚠️ QUEUE_CAPACITY 过小，已自动修正为 100")
		AppConfig.QueueCapacity = 100
	}
	if AppConfig.QueueCapacity > 100000 {
		log.Printf("⚠️ QUEUE_CAPACITY 过大，已自动限制为 100000")
		AppConfig.QueueCapacity = 100000
	}
	if AppConfig.DatabaseMaxOpenConns < 1 {
		log.Printf("⚠️ DATABASE_MAX_OPEN_CONNS 小于 1，已自动修正为 1")
		AppConfig.DatabaseMaxOpenConns = 1
	}
	if AppConfig.DatabaseMaxOpenConns > 20 {
		log.Printf("⚠️ DATABASE_MAX_OPEN_CONNS 过大，SQLite 场景已自动限制为 20")
		AppConfig.DatabaseMaxOpenConns = 20
	}

	if AppConfig.DatabaseMaxIdleConns < 1 {
		log.Printf("⚠️ DATABASE_MAX_IDLE_CONNS 小于 1，已自动修正为 1")
		AppConfig.DatabaseMaxIdleConns = 1
	}
	if AppConfig.DatabaseMaxIdleConns > AppConfig.DatabaseMaxOpenConns {
		log.Printf("⚠️ DATABASE_MAX_IDLE_CONNS 大于 DATABASE_MAX_OPEN_CONNS，已自动修正")
		AppConfig.DatabaseMaxIdleConns = AppConfig.DatabaseMaxOpenConns
	}

	if AppConfig.DatabaseBusyTimeoutMS < 1000 {
		log.Printf("⚠️ DATABASE_BUSY_TIMEOUT_MS 过小，已自动修正为 1000")
		AppConfig.DatabaseBusyTimeoutMS = 1000
	}
	if AppConfig.DatabaseBusyTimeoutMS > 60000 {
		log.Printf("⚠️ DATABASE_BUSY_TIMEOUT_MS 过大，已自动限制为 60000")
		AppConfig.DatabaseBusyTimeoutMS = 60000
	}

}

func isValidAbsAPIURL(rawURL string) bool {
	_, err := parseAbsAPIURL(rawURL)
	return err == nil
}

func isAbsAPIHTTPURL(rawURL string) bool {
	parsed, err := parseAbsAPIURL(rawURL)
	return err == nil && parsed.Scheme == "http"
}

func parseAbsAPIURL(rawURL string) (*url.URL, error) {
	if rawURL == "" || rawURL != strings.TrimSpace(rawURL) || strings.ContainsAny(rawURL, " \r\n\t") {
		return nil, url.InvalidHostError(rawURL)
	}
	for _, r := range rawURL {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			return nil, url.InvalidHostError(rawURL)
		}
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed == nil {
		return nil, url.InvalidHostError(rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, url.InvalidHostError(rawURL)
	}
	if parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, url.InvalidHostError(rawURL)
	}
	return parsed, nil
}

func secretFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	encoded := hex.EncodeToString(sum[:])
	if len(encoded) > 12 {
		return encoded[:12]
	}
	return encoded
}

func (s envSource) getEnv(key, defaultVal string) string {
	if val, exists := s.lookup(key); exists {
		return val
	}
	return defaultVal
}

func (s envSource) getEnvAsInt(key string, defaultVal int) int {
	if val, err := strconv.Atoi(s.getEnv(key, "")); err == nil {
		return val
	}
	return defaultVal
}

func (s envSource) getEnvAsInt64(key string, defaultVal int64) int64 {
	if val, err := strconv.ParseInt(s.getEnv(key, ""), 10, 64); err == nil {
		return val
	}
	return defaultVal
}

func (s envSource) getEnvAsBool(key string, defaultVal bool) bool {
	if val, exists := s.lookup(key); exists {
		return strings.ToLower(val) == "true"
	}
	return defaultVal
}
