package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"ccoff-abs-bot/internal/config"
)

const sensitiveHashPrefix = "hmac-sha256$"

func GetSensitivePepper() string {
	if config.AppConfig == nil {
		return ""
	}

	return strings.TrimSpace(config.AppConfig.SecurityPepper)
}

func HashSensitiveToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	pepper := GetSensitivePepper()
	if pepper == "" {
		return ""
	}

	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(raw))
	sum := mac.Sum(nil)

	return sensitiveHashPrefix + base64.RawURLEncoding.EncodeToString(sum)
}

func isHashedSensitiveToken(stored string) bool {
	return strings.HasPrefix(stored, sensitiveHashPrefix)
}

func VerifySensitiveToken(raw string, stored string) bool {
	raw = strings.TrimSpace(raw)
	stored = strings.TrimSpace(stored)

	if raw == "" || stored == "" {
		return false
	}

	if !isHashedSensitiveToken(stored) {
		return false
	}
	expected := HashSensitiveToken(raw)
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(stored)) == 1
}

func MaskSecret(raw string) string {
	raw = strings.TrimSpace(raw)
	runes := []rune(raw)

	if len(runes) <= 4 {
		return "****"
	}

	if len(runes) <= 10 {
		return string(runes[:2]) + "****" + string(runes[len(runes)-2:])
	}

	return string(runes[:4]) + "****" + string(runes[len(runes)-4:])
}
