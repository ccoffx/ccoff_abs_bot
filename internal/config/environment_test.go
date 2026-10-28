package config

import (
	"path/filepath"
	"testing"
)

func TestEnvironmentIsolation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data-test")
	valid := Config{AppEnv: "test", ABSMode: "mock", TestDataDir: root, DbURL: filepath.Join(root, "bot.db"), TestBotUsername: "sandbox_bot"}
	if err := valid.ValidateEnvironment(); err != nil {
		t.Fatal(err)
	}
	cases := []func(*Config){func(c *Config) { c.AppEnv = "production" }, func(c *Config) { c.ABSMode = "real" }, func(c *Config) { c.NoticeGroupID = -100 }, func(c *Config) { c.BackupGroupID = -100 }, func(c *Config) { c.DbURL = "data/bot_data.db" }, func(c *Config) { c.AbsURL = "https://real.example" }, func(c *Config) { c.AbsKey = "production-key" }, func(c *Config) { c.TestBotUsername = "" }, func(c *Config) { c.DbURL = "file:" + c.DbURL }, func(c *Config) { c.AppEnv = "staging" }}
	for i, change := range cases {
		c := valid
		change(&c)
		if c.ValidateEnvironment() == nil {
			t.Errorf("unsafe configuration %d accepted", i)
		}
	}
	if err := (&Config{AppEnv: "production", ABSMode: "real"}).ValidateEnvironment(); err != nil {
		t.Fatal(err)
	}
}
