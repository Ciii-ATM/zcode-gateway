package main

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminCookieName = "zcode_admin"
	adminSessionTTL = 7 * 24 * 3600
)

func (s *Server) initAdminPassword() {
	if configured := strings.TrimSpace(os.Getenv("ZCODE_ADMIN_PASSWORD")); configured != "" {
		s.adminPass = configured
		s.adminGenerated = false
		return
	}
	if hash := s.store.Settings().AdminPasswordHash; hash != "" {
		s.adminHash = hash
		return
	}
	token := make([]byte, 12)
	if _, err := rand.Read(token); err != nil {
		token = []byte("zcode-admin")
	}
	password := base64.RawURLEncoding.EncodeToString(token)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err == nil {
		s.store.UpdateSettings(func(st *Settings) {
			st.AdminPasswordHash = string(hash)
		})
		s.adminHash = string(hash)
	}
	s.adminPass = password
	s.adminGenerated = true
}

func (s *Server) passwordMatches(candidate string) bool {
	if s.adminPass != "" {
		return hmac.Equal([]byte(candidate), []byte(s.adminPass))
	}
	if s.adminHash != "" {
		return bcrypt.CompareHashAndPassword([]byte(s.adminHash), []byte(candidate)) == nil
	}
	return false
}

func (s *Server) newAdminSession(w http.ResponseWriter) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	now := time.Now().Unix()
	if s.store.db != nil {
		if err := s.store.db.CreateAdminSession(token, now, now+adminSessionTTL); err != nil {
			return "", err
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   adminSessionTTL,
	})
	return token, nil
}

func (s *Server) adminAuthed(r *http.Request) bool {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	if s.store.db == nil {
		return false
	}
	return s.store.db.AdminSessionValid(cookie.Value, time.Now().Unix())
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	if !s.passwordMatches(strings.TrimSpace(body.Password)) {
		writeError(w, 401, "密码错误")
		return
	}
	if _, err := s.newAdminSession(w); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminCookieName); err == nil && s.store.db != nil {
		s.store.db.DeleteAdminSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"authenticated": s.adminAuthed(r)})
}

// requireAdmin guards every panel API and the SPA shell.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuthed(r) {
			writeError(w, 401, "not authenticated")
			return
		}
		next(w, r)
	}
}
