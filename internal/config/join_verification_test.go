package config

import "testing"

func TestJoinVerificationConfiguration(t *testing.T) {
	valid := Config{AppEnv: "production", ABSMode: "real", NoticeGroupID: -100, JoinVerifyEnabled: true, JoinVerifyChannelID: -200, JoinVerifyChannelURL: "https://t.me/+private_invite"}
	if err := valid.ValidateEnvironment(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.NoticeGroupID = 0 }, func(c *Config) { c.JoinVerifyChannelID = 0 }, func(c *Config) { c.JoinVerifyChannelID = c.NoticeGroupID },
		func(c *Config) { c.JoinVerifyChannelURL = "http://t.me/channel" }, func(c *Config) { c.JoinVerifyChannelURL = "https://evil.example/channel" },
		func(c *Config) { c.JoinVerifyChannelURL = "https://t.me/" }, func(c *Config) { c.JoinVerifyChannelURL = "https://user@t.me/channel" },
		func(c *Config) { c.AppEnv = "test" },
	} {
		c := valid
		mutate(&c)
		if c.ValidateEnvironment() == nil {
			t.Fatalf("unsafe join verification config accepted: %+v", c)
		}
	}
}
