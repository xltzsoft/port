// Package auth 实现登录鉴权: HMAC-SHA256 + 时间窗,防重放。
// token 永不上网,客户端只发送 HMAC(token, "clientID|ts")。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// HmacLogin 计算登录签名: HMAC-SHA256(token, "clientID|ts")。
func HmacLogin(token, clientID string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = fmt.Fprintf(mac, "%s|%d", clientID, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyLogin 校验签名与时间窗(|now-ts| <= window),防重放。
func VerifyLogin(token, clientID string, ts int64, want string, window time.Duration) bool {
	if want == "" {
		return false
	}
	diff := time.Since(time.Unix(ts, 0))
	if diff < -window || diff > window {
		return false
	}
	got := HmacLogin(token, clientID, ts)
	return hmac.Equal([]byte(got), []byte(want))
}
