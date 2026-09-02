package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	sessionCookieName = "bill_session"
	sessionTTL        = 24 * time.Hour
)

var (
	authUsername string
	authPassword string

	sessionsMu sync.Mutex
	sessions   = map[string]time.Time{}
)

// initAuth 从环境变量（.env / docker-compose env_file）读取登录账号密码，未配置时使用默认值并告警。
func initAuth() {
	authUsername = os.Getenv("BILL_AUTH_USERNAME")
	authPassword = os.Getenv("BILL_AUTH_PASSWORD")
	if authUsername == "" {
		authUsername = "admin"
	}
	if authPassword == "" {
		authPassword = "admin123"
		log.Printf("警告: 未设置 BILL_AUTH_PASSWORD 环境变量，正在使用默认密码，请通过 .env 修改后再对外部署")
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(body.Username), []byte(authUsername)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(body.Password), []byte(authPassword)) == 1
	if !userOK || !passOK {
		httpError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	token := newSessionToken()
	expires := time.Now().Add(sessionTTL)
	sessionsMu.Lock()
	sessions[token] = expires
	sessionsMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		sessionsMu.Lock()
		delete(sessions, c.Value)
		sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": isAuthenticated(r)})
}

func isAuthenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	sessionsMu.Lock()
	expires, ok := sessions[c.Value]
	sessionsMu.Unlock()
	if !ok || time.Now().After(expires) {
		return false
	}
	return true
}

func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAuthenticated(r) {
			httpError(w, http.StatusUnauthorized, "未登录或登录已过期，请重新登录")
			return
		}
		h(w, r)
	}
}

func newSessionToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cleanupSessions 定期清理已过期的登录会话，避免内存无限增长。
func cleanupSessions() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		sessionsMu.Lock()
		for token, expires := range sessions {
			if now.After(expires) {
				delete(sessions, token)
			}
		}
		sessionsMu.Unlock()
	}
}
