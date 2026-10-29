package security

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"math/big"
	"time"

	"ccoff-abs-bot/internal/textfmt"
)

func RandomIntRange(min int, max int) int {
	if max <= min {
		return min
	}

	nBig, err := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	if err != nil {
		return min
	}

	return int(nBig.Int64()) + min
}

func GenerateRandomCode(length int) string {
	if length <= 0 {
		length = 16
	}

	bytes := make([]byte, (length+1)/2)
	// 熵源故障必须 fail-closed：本函数的产物包含邀请码/续期卡/卡密等安全令牌，
	// 绝不能静默退化为时间戳等可预测值；重试后仍失败则中止本次生成（由各调用点的 recover 兜底）。
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if _, err = rand.Read(bytes); err == nil {
			code := hex.EncodeToString(bytes)
			if len(code) > length {
				code = code[:length]
			}
			return code
		}
		time.Sleep(10 * time.Millisecond)
	}
	log.Printf("❌ crypto/rand 不可用，已中止安全码生成（fail-closed）: err=%s", textfmt.FormatPlainError(err))
	panic("crypto/rand unavailable; refusing to generate predictable security token")
}
