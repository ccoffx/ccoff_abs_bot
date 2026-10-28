package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeConfigIsolatedFromProductionEnvironment(t *testing.T) {
	previous := AppConfig
	t.Cleanup(func() { AppConfig = previous })
	for key, value := range map[string]string{
		"APP_ENV": "production", "ABS_MODE": "real", "TELEGRAM_BOT_TOKEN": "production-token",
		"ABS_API_URL": "https://real.example", "ABS_API_KEY": "real-key", "ABS_TEMPLATE_USER_ID": "real-template",
		"DATABASE_URL": "data/bot_data.db", "NOTICE_GROUP_ID": "-100", "BACKUP_GROUP_ID": "-200", "GITHUB_API_TOKEN": "real-github-token",
	} {
		t.Setenv(key, value)
	}
	path := filepath.Join(t.TempDir(), ".env.test")
	data := "APP_ENV=test\nABS_MODE=mock\nTELEGRAM_BOT_TOKEN=123:test\nTEST_BOT_USERNAME=test_bot\nADMIN_TELEGRAM_IDS=42\nTEST_DATA_DIR=data-test\nDATABASE_URL=data-test/bot_data.db\nINVITE_REQUIRED=false\nWORKER_COUNT=4\nBACKUP_ENCRYPT_KEY=" + strings.Repeat("b", 64) + "\nSECURITY_PEPPER=" + strings.Repeat("s", 64) + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadTestConfig(path); err != nil {
		t.Fatal(err)
	}
	c := AppConfig
	if !c.IsTest() || c.ABSMode != "mock" || c.TgToken != "123:test" || c.AbsURL != "https://abs.mock.invalid" || c.AbsKey != "mock" || c.AbsTemplateID != "mock-template" || c.DbURL != "data-test/bot_data.db" || c.NoticeGroupID != 0 || c.BackupGroupID != 0 || c.GithubAPIToken != "" || c.InviteRequired || c.WorkerCount != 4 || !c.AdminIDs[42] {
		t.Fatal("native test config inherited production values or lost test settings")
	}
	if os.Getenv("TELEGRAM_BOT_TOKEN") != "production-token" {
		t.Fatal("test loader mutated process environment")
	}
}

func TestNativeConfigRequiresExplicitTestFile(t *testing.T) {
	previous := AppConfig
	t.Cleanup(func() { AppConfig = previous })
	path := filepath.Join(t.TempDir(), ".env.test")
	if err := LoadTestConfig(path); err == nil {
		t.Fatal("missing test file accepted")
	}
	for _, data := range []string{"APP_ENV=production\nABS_MODE=real\n", "APP_ENV=test\nABS_MODE=real\n", "APP_ENV=test\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := LoadTestConfig(path); err == nil {
			t.Fatal("non-mock test config accepted")
		}
		if AppConfig != previous {
			t.Fatal("invalid test file changed active config")
		}
	}
}
