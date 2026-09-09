package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const userContextHeader = "X-Deeix-User-Context"

type userContext struct {
	UserID    uint64 `json:"user_id"`
	ExpiresAt int64  `json:"exp"`
}

type TenantResolver interface {
	ResolveTenant(http.Header) (string, error)
}

type DeeixResolver struct {
	Secret string
	Now    func() time.Time
}

func (r DeeixResolver) ResolveTenant(headers http.Header) (string, error) {
	token := strings.TrimSpace(headers.Get(userContextHeader))
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] == "" || parts[2] == "" {
		return "", errors.New("missing or malformed signed user context")
	}
	mac := hmac.New(sha256.New, []byte(r.Secret))
	_, _ = mac.Write([]byte(parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(mac.Sum(nil), sig) {
		return "", errors.New("invalid signed user context")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid signed user context")
	}
	var payload userContext
	if json.Unmarshal(raw, &payload) != nil || payload.UserID == 0 || payload.ExpiresAt <= r.now().Unix() {
		return "", errors.New("expired or invalid signed user context")
	}
	return strconv.FormatUint(payload.UserID, 10), nil
}

func (r DeeixResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func requireBearer(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || !hmac.Equal([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(token)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tenantFromRequest(resolver TenantResolver, headers http.Header) (string, error) {
	tenant, err := resolver.ResolveTenant(headers)
	if err != nil {
		return "", fmt.Errorf("authorization failed: %w", err)
	}
	return tenant, nil
}
