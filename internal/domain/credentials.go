package domain

import (
	"crypto/sha256"
	"encoding/hex"
)

func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func APIKeyPrefix(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}
