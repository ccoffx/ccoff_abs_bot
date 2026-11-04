package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

func secretReceiptCipher(domain string) (cipher.AEAD, error) {
	pepper := GetSensitivePepper()
	if pepper == "" {
		return nil, fmt.Errorf("SECURITY_PEPPER_NOT_CONFIGURED")
	}
	key := sha256.Sum256([]byte(domain + ":" + pepper))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func EncryptSecretReceipt(domain, identity, plain string) (string, error) {
	aead, err := secretReceiptCipher(domain)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return "gcm$" + base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plain), []byte(identity))), nil
}

func DecryptSecretReceipt(domain, identity, encrypted string) (string, error) {
	if !strings.HasPrefix(encrypted, "gcm$") {
		return "", fmt.Errorf("INVALID_SECRET_RECEIPT")
	}
	aead, err := secretReceiptCipher(domain)
	if err != nil {
		return "", err
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encrypted, "gcm$"))
	if err != nil {
		return "", err
	}
	if len(payload) < aead.NonceSize()+aead.Overhead() {
		return "", fmt.Errorf("INVALID_SECRET_RECEIPT_PAYLOAD")
	}
	plain, err := aead.Open(nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], []byte(identity))
	return string(plain), err
}
