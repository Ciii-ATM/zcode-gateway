package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu          sync.RWMutex
	modelMu     sync.RWMutex
	dir         string
	master      []byte
	accounts    []*Account
	settings    Settings
	db          *DB
	modelStates map[string]ModelState
}

type storedFile struct {
	Version  int             `json:"version"`
	Accounts []storedAccount `json:"accounts"`
	Settings Settings        `json:"settings"`
}

type storedAccount struct {
	Account
	Secrets map[string]string `json:"secrets"`
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		dir:      dir,
		settings: Settings{Fingerprint: true, StoreBodies: true, QuotaIntervalSec: 30, PromptMode: "append", UAProfile: "zcode"},
	}
	if err := s.loadMasterKey(); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	db, err := OpenDB(s.dirPath("gateway.db"))
	if err != nil {
		return nil, err
	}
	s.db = db
	if err := ensureProfileColumns(db); err != nil {
		return nil, err
	}
	if err := ensureModelStateColumns(db); err != nil {
		return nil, err
	}
	_ = db.ImportJSONL(s.dirPath("logs.jsonl"))
	if len(s.accounts) == 0 {
		if err := s.ImportDesktop(); err != nil {
			// Import is best-effort; a fresh install may have no desktop login.
			_ = err
		}
	}
	s.attachProfiles()
	if err := s.loadModelStates(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *Store) dirPath(name string) string {
	return filepath.Join(s.dir, name)
}

func (s *Store) loadMasterKey() error {
	path := s.dirPath("master.key")
	data, err := os.ReadFile(path)
	if err == nil && len(data) == 32 {
		s.master = data
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return err
	}
	s.master = key
	return nil
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Store) encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	block, err := aes.NewCipher(s.master)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), nil)
	tag := sealed[len(sealed)-gcm.Overhead():]
	data := sealed[:len(sealed)-gcm.Overhead()]
	return "enc:v1:" + b64url(iv) + "." + b64url(tag) + "." + b64url(data), nil
}

func (s *Store) decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, "enc:v1:") {
		return value, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, "enc:v1:"), ".")
	if len(parts) != 3 {
		return "", errors.New("invalid ciphertext")
	}
	iv, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(s.master)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	sealed := append(append([]byte{}, data...), tag...)
	plain, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *Store) load() error {
	path := s.dirPath("accounts.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var file storedFile
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}
	if file.Settings.QuotaIntervalSec <= 0 {
		file.Settings.QuotaIntervalSec = 30
	}
	if file.Settings.PromptMode == "" {
		file.Settings.PromptMode = "append"
	}
	if file.Settings.UAProfile == "" {
		file.Settings.UAProfile = "zcode"
	}
	if !file.Settings.FingerprintSet {
		// Older files predate the field; fingerprinting is the safe default.
		file.Settings.Fingerprint = true
	}
	s.settings = file.Settings
	s.accounts = nil
	for _, sa := range file.Accounts {
		acc := sa.Account
		for key, value := range sa.Secrets {
			plain, err := s.decrypt(value)
			if err != nil {
				continue
			}
			switch key {
			case "zcodeJwt":
				acc.ZcodeJWT = plain
			case "accessToken":
				acc.AccessToken = plain
			case "projectToken":
				acc.ProjectToken = plain
			}
		}
		if acc.Models == nil {
			acc.Models = map[string]*UsageTotals{}
		}
		s.accounts = append(s.accounts, &acc)
	}
	sort.SliceStable(s.accounts, func(i, j int) bool {
		if s.accounts[i].Priority != s.accounts[j].Priority {
			return s.accounts[i].Priority < s.accounts[j].Priority
		}
		return s.accounts[i].CreatedAt < s.accounts[j].CreatedAt
	})
	s.normalizePrioritiesLocked()
	return nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	s.settings.FingerprintSet = true
	file := storedFile{Version: 1, Settings: s.settings}
	for _, acc := range s.accounts {
		sa := storedAccount{Account: *acc, Secrets: map[string]string{}}
		if acc.ZcodeJWT != "" {
			enc, err := s.encrypt(acc.ZcodeJWT)
			if err != nil {
				return err
			}
			sa.Secrets["zcodeJwt"] = enc
		}
		if acc.AccessToken != "" {
			enc, err := s.encrypt(acc.AccessToken)
			if err != nil {
				return err
			}
			sa.Secrets["accessToken"] = enc
		}
		if acc.ProjectToken != "" {
			enc, err := s.encrypt(acc.ProjectToken)
			if err != nil {
				return err
			}
			sa.Secrets["projectToken"] = enc
		}
		sa.ZcodeJWT = ""
		sa.AccessToken = ""
		sa.ProjectToken = ""
		file.Accounts = append(file.Accounts, sa)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.dirPath("accounts.json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.dirPath("accounts.json"))
}

func (s *Store) appendLog(e *LogEntry) {
	if s.db != nil {
		_ = s.db.InsertLog(e)
	}
}
func (s *Store) Accounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, len(s.accounts))
	copy(out, s.accounts)
	return out
}

func (s *Store) FindByUser(provider, userID string) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, acc := range s.accounts {
		if acc.Provider == provider && acc.User.ID == userID {
			copy := *acc
			return &copy
		}
	}
	return nil
}

func (s *Store) GetAccount(id string) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, acc := range s.accounts {
		if acc.ID == id {
			copy := *acc
			return &copy
		}
	}
	return nil
}

func (s *Store) sortedLocked() []*Account {
	out := make([]*Account, len(s.accounts))
	copy(out, s.accounts)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out
}

func (s *Store) normalizePrioritiesLocked() {
	for i, acc := range s.accounts {
		acc.Priority = i + 1
	}
}

func (s *Store) PickAccount(now time.Time) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	sorted := s.sortedLocked()
	for _, acc := range sorted {
		if acc.Available(now) {
			s.settings.ActiveAccountID = acc.ID
			copy := *acc
			return &copy
		}
	}
	return nil
}

func (s *Store) SetActive(id string) {
	s.mu.Lock()
	s.settings.ActiveAccountID = id
	s.mu.Unlock()
}

func (s *Store) MarkExhausted(id string, until time.Time, reason string) {
	s.mu.Lock()
	for _, acc := range s.accounts {
		if acc.ID == id {
			acc.ExhaustedUntil = until.Unix()
			acc.LastError = reason
			acc.LastErrorAt = time.Now().Unix()
			if s.settings.ActiveAccountID == id {
				s.settings.ActiveAccountID = ""
			}
			break
		}
	}
	s.mu.Unlock()
	_ = s.Save()
}

func (s *Store) UpdateAccount(id string, fn func(*Account)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, acc := range s.accounts {
		if acc.ID == id {
			fn(acc)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

func (s *Store) UpdateAccountMemory(id string, fn func(*Account)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, acc := range s.accounts {
		if acc.ID == id {
			fn(acc)
			return true
		}
	}
	return false
}

func (s *Store) AddAccount(acc *Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc.ID == "" {
		acc.ID = randomID()
	}
	if acc.CreatedAt == 0 {
		acc.CreatedAt = time.Now().Unix()
	}
	if acc.Priority == 0 {
		acc.Priority = len(s.accounts) + 1
	}
	if acc.Models == nil {
		acc.Models = map[string]*UsageTotals{}
	}
	profile := s.EnsureProfile(acc)
	// Persist even when the caller supplied the profile (OAuth flow), so the
	// identity used at login is the one stored for later traffic.
	_ = s.saveProfile(acc.ID, *profile, acc.Concurrency, acc.LastActivationDate)
	s.accounts = append(s.accounts, acc)
	sort.SliceStable(s.accounts, func(i, j int) bool {
		if s.accounts[i].Priority != s.accounts[j].Priority {
			return s.accounts[i].Priority < s.accounts[j].Priority
		}
		return s.accounts[i].CreatedAt < s.accounts[j].CreatedAt
	})
	s.normalizePrioritiesLocked()
	if s.settings.ActiveAccountID == "" {
		s.settings.ActiveAccountID = acc.ID
	}
	return s.saveLocked()
}

func (s *Store) DeleteAccount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.accounts[:0]
	for _, acc := range s.accounts {
		if acc.ID != id {
			out = append(out, acc)
		}
	}
	s.accounts = out
	if s.settings.ActiveAccountID == id {
		s.settings.ActiveAccountID = ""
	}
	if s.db != nil {
		s.db.DeleteAccountProfile(id)
		s.db.UnbindAccount(id)
	}
	_ = s.saveLocked()
}

func (s *Store) Logs(limit int, status string, accountID string) []*LogEntry {
	if s.db == nil {
		return nil
	}
	out, err := s.db.Logs(limit, status, accountID)
	if err != nil {
		return nil
	}
	for _, e := range out {
		e.RequestBody = ""
		e.ResponseBody = ""
	}
	return out
}

func (s *Store) GetLog(id string) *LogEntry {
	if s.db == nil {
		return nil
	}
	e, err := s.db.GetLog(id)
	if err != nil {
		return nil
	}
	return e
}

func (s *Store) TodayTotals() (requests, total int, u Usage) {
	if s.db == nil {
		return 0, 0, Usage{}
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	return s.db.TodayTotals(start)
}

func (s *Store) AccountTodayStats() map[string]*TodayStats {
	if s.db == nil {
		return map[string]*TodayStats{}
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	return s.db.AccountStats(start)
}
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

func (s *Store) UpdateSettings(fn func(*Settings)) {
	s.mu.Lock()
	fn(&s.settings)
	_ = s.saveLocked()
	s.mu.Unlock()
}

func (s *Store) Summaries() []*AccountSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AccountSummary, 0, len(s.accounts))
	today := s.AccountTodayStats()
	now := time.Now()
	for _, acc := range s.sortedLocked() {
		status := "可用"
		if !acc.Enabled {
			status = "已禁用"
		} else if acc.Quota.Granted > 0 && acc.Quota.Remaining <= 0 {
			status = "额度耗尽"
		} else if acc.ExhaustedUntil > now.Unix() {
			status = "冷却中"
		}
		stats := today[acc.ID]
		if stats == nil {
			stats = &TodayStats{}
		}
		var platform, osVersion, screen, deviceMid string
		if acc.Profile != nil {
			platform = acc.Profile.Platform + "-" + acc.Profile.Arch
			osVersion = acc.Profile.OSVersion
			screen = acc.Profile.Screen
			deviceMid = acc.Profile.DeviceMid
		}
		lastUsed := acc.LastUsedAt
		if stats.LastUsed > lastUsed {
			lastUsed = stats.LastUsed
		}
		out = append(out, &AccountSummary{
			ID:             acc.ID,
			Label:          acc.Label,
			Provider:       acc.Provider,
			Mode:           acc.Mode,
			Priority:       acc.Priority,
			Enabled:        acc.Enabled,
			Status:         status,
			Concurrency:    clampConcurrency(acc.Concurrency),
			ModelStates:    s.ModelStatesFor(acc.ID),
			User:           acc.User,
			Quota:          acc.Quota,
			LastError:      acc.LastError,
			ExhaustedUntil: acc.ExhaustedUntil,
			LastUsedAt:     lastUsed,
			CreatedAt:      acc.CreatedAt,
			Usage:          acc.Usage,
			Today:          *stats,
			HasProject:     acc.Project != nil,
			Platform:       platform,
			OSVersion:      osVersion,
			Screen:         screen,
			DeviceMid:      deviceMid,
		})
	}
	return out
}

func (s *Store) AccountsForRefresh() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, acc := range s.accounts {
		out = append(out, acc)
	}
	return out
}

// ---------- desktop credential import ----------

func desktopKeys() [][]byte {
	if secret := os.Getenv("ZCODE_CREDENTIAL_SECRET"); secret != "" {
		sum := sha256.Sum256([]byte(secret))
		return [][]byte{sum[:]}
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32" // Node's process.platform value used by ZCode.
	}
	home, _ := os.UserHomeDir()
	names := []string{}
	if u, err := user.Current(); err == nil {
		names = append(names, u.Username)
		if i := strings.LastIndexAny(u.Username, `\/`); i >= 0 {
			names = append(names, u.Username[i+1:])
		}
	}
	if v := os.Getenv("USERNAME"); v != "" {
		names = append(names, v)
	}
	seen := map[string]bool{}
	var keys [][]byte
	for _, name := range names {
		secret := fmt.Sprintf("zcode-credential-fallback:%s:%s:%s", platform, home, name)
		if seen[secret] {
			continue
		}
		seen[secret] = true
		sum := sha256.Sum256([]byte(secret))
		keys = append(keys, sum[:])
	}
	return keys
}

func decryptDesktop(value string, keys [][]byte) (string, error) {
	if !strings.HasPrefix(value, "enc:v1:") {
		return value, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, "enc:v1:"), ".")
	if len(parts) != 3 {
		return "", errors.New("invalid desktop ciphertext")
	}
	iv, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", err
	}
	var lastErr error
	for _, key := range keys {
		block, err := aes.NewCipher(key)
		if err != nil {
			lastErr = err
			continue
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			lastErr = err
			continue
		}
		sealed := append(append([]byte{}, data...), tag...)
		plain, err := gcm.Open(nil, iv, sealed, nil)
		if err == nil {
			return string(plain), nil
		}
		lastErr = err
	}
	return "", lastErr
}

func (s *Store) ImportDesktop() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".zcode", "v2", "credentials.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file map[string]string
	if err := json.Unmarshal(raw, &file); err != nil {
		return err
	}
	keys := desktopKeys()
	decryptAny := func(name string) (string, error) {
		value, ok := file[name]
		if !ok {
			return "", os.ErrNotExist
		}
		return decryptDesktop(value, keys)
	}
	zcodeJWT, err := decryptAny("zcodejwttoken")
	if err != nil {
		return err
	}
	accessToken, _ := decryptAny("oauth:zai:access_token")
	userInfoRaw, _ := decryptAny("oauth:zai:user_info")
	providerRaw, _ := decryptAny("oauth:active_provider")
	provider := strings.TrimSpace(providerRaw)
	if provider == "" {
		provider = "zai"
	}
	var userInfo UserInfo
	if userInfoRaw != "" {
		var parsed struct {
			UserID string `json:"user_id"`
			Email  string `json:"email"`
			Name   string `json:"name"`
			Avatar string `json:"avatar"`
		}
		if json.Unmarshal([]byte(userInfoRaw), &parsed) == nil {
			userInfo = UserInfo{ID: parsed.UserID, Email: parsed.Email, Name: parsed.Name, Avatar: parsed.Avatar}
		}
	}
	if userInfo.ID == "" {
		userInfo.ID = "imported-" + randomID()
	}
	var project *ProjectLocation
	for name, value := range file {
		const prefix = "project-key-location:v1:"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		decodedName, err := urlDecode(name[len(prefix):])
		if err != nil {
			continue
		}
		var meta []any
		if json.Unmarshal([]byte(decodedName), &meta) != nil || len(meta) < 6 {
			continue
		}
		plain, err := decryptDesktop(value, keys)
		if err != nil {
			continue
		}
		var detail struct {
			OrganizationID string `json:"organizationId"`
			ProjectID      string `json:"projectId"`
			APIKeyID       string `json:"apiKeyId"`
		}
		if json.Unmarshal([]byte(plain), &detail) != nil {
			continue
		}
		origin, _ := meta[0].(string)
		family, _ := meta[1].(string)
		project = &ProjectLocation{
			OrganizationID: detail.OrganizationID,
			ProjectID:      detail.ProjectID,
			APIKeyID:       detail.APIKeyID,
			Origin:         origin,
			Family:         family,
		}
		break
	}
	label := userInfo.Name
	if label == "" {
		label = userInfo.Email
	}
	if label == "" {
		label = "本机账号"
	}
	if existing := s.FindByUser(provider, userInfo.ID); existing != nil {
		s.UpdateAccount(existing.ID, func(a *Account) {
			a.ZcodeJWT = zcodeJWT
			a.AccessToken = accessToken
			if project != nil {
				a.Project = project
			}
			a.LastError = ""
			a.ExhaustedUntil = 0
		})
		return nil
	}
	acc := &Account{
		Label:       label,
		Provider:    provider,
		Mode:        "start-plan",
		User:        userInfo,
		Project:     project,
		Priority:    len(s.accounts) + 1,
		Enabled:     true,
		CreatedAt:   time.Now().Unix(),
		ZcodeJWT:    zcodeJWT,
		AccessToken: accessToken,
		Models:      map[string]*UsageTotals{},
	}
	return s.AddAccount(acc)
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b64url(b)
}

func urlDecode(s string) (string, error) {
	// The credential key uses percent-encoding, not plus-as-space.
	return url.PathUnescape(s)
}
