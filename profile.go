package main

import (
	"database/sql"
	"math/rand/v2"
	"strings"
	"time"
)

type profileRow struct {
	Profile            DeviceProfile
	Concurrency        int
	LastActivationDate string
	Found              bool
}

func (d *DB) LoadAccountProfile(accountID string) (profileRow, error) {
	var row profileRow
	var created, updated int64
	err := d.sql.QueryRow(`SELECT platform, arch, os_version, language, timezone, screen, device_mid, concurrency, COALESCE(last_activation_date,''), created_at, updated_at FROM account_profiles WHERE account_id = ?`, accountID).
		Scan(&row.Profile.Platform, &row.Profile.Arch, &row.Profile.OSVersion, &row.Profile.Language, &row.Profile.Timezone, &row.Profile.Screen, &row.Profile.DeviceMid, &row.Concurrency, &row.LastActivationDate, &created, &updated)
	if err == nil {
		row.Found = true
	}
	return row, err
}

func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > 5 {
		return 5
	}
	return n
}

func (s *Store) saveProfile(accountID string, p DeviceProfile, concurrency int, activation string) error {
	if s.db == nil {
		return nil
	}
	now := time.Now().Unix()
	_, err := s.db.sql.Exec(`INSERT INTO account_profiles
(account_id, platform, arch, os_version, language, timezone, screen, device_mid, concurrency, last_activation_date, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(account_id) DO UPDATE SET
platform=excluded.platform, arch=excluded.arch, os_version=excluded.os_version,
language=excluded.language, timezone=excluded.timezone, screen=excluded.screen,
device_mid=excluded.device_mid, concurrency=excluded.concurrency,
last_activation_date=excluded.last_activation_date, updated_at=excluded.updated_at`,
		accountID, p.Platform, p.Arch, p.OSVersion, p.Language, p.Timezone, p.Screen, p.DeviceMid, clampConcurrency(concurrency), activation, now, now)
	return err
}

func (s *Store) loadProfile(accountID string) (profileRow, error) {
	if s.db == nil {
		return profileRow{}, sql.ErrNoRows
	}
	return s.db.LoadAccountProfile(accountID)
}

// Device profiles are generated locally, per account: a coherent desktop SKU
// (platform + arch + os build + screen chosen together), a realistic
// language/timezone pair and a fresh UUIDv4 device id. Nothing is read from the
// host machine, so moving the gateway never changes an account's identity.
// Profiles without a low-DPI screen: the pool only carries Retina/1440p+ SKUs.

type profileSKU struct {
	Platform  string
	Arch      string
	OSVersion string
	Screen    string
	Weight    int
}

var profileSKUs = []profileSKU{
	// Apple silicon. Values are Electron DIPs; only 14"/16" MacBook Pro class
	// and larger displays are used, no small/low-DPI panels.
	{Platform: "darwin", Arch: "arm64", OSVersion: "24.5.0", Screen: "1728x1117", Weight: 10},
	{Platform: "darwin", Arch: "arm64", OSVersion: "24.6.0", Screen: "1728x1117", Weight: 10},
	{Platform: "darwin", Arch: "arm64", OSVersion: "25.5.0", Screen: "1728x1117", Weight: 7},
	{Platform: "darwin", Arch: "arm64", OSVersion: "25.5.0", Screen: "2056x1329", Weight: 6},
	{Platform: "darwin", Arch: "arm64", OSVersion: "24.5.0", Screen: "2560x1440", Weight: 4},
	// Windows laptops/desktops with 1440p or better panels.
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.22631", Screen: "2560x1440", Weight: 9},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.26100", Screen: "2560x1440", Weight: 7},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.22631", Screen: "2560x1600", Weight: 6},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.26100", Screen: "2880x1800", Weight: 5},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.22621", Screen: "3200x1800", Weight: 4},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.26100", Screen: "3840x2160", Weight: 4},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.22631", Screen: "2256x1504", Weight: 4},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.26200", Screen: "2496x1664", Weight: 4},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.22631", Screen: "3440x1440", Weight: 3},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.19045", Screen: "2560x1440", Weight: 2},
	{Platform: "win32", Arch: "x64", OSVersion: "10.0.22000", Screen: "3000x2000", Weight: 2},
}

type profileLocale struct {
	Language string
	Timezone string
	Weight   int
}

var profileLocales = []profileLocale{
	{Language: "zh-CN", Timezone: "Asia/Shanghai", Weight: 60},
	{Language: "en-US", Timezone: "America/New_York", Weight: 8},
	{Language: "en-US", Timezone: "America/Los_Angeles", Weight: 7},
	{Language: "en-GB", Timezone: "Europe/London", Weight: 5},
	{Language: "ja-JP", Timezone: "Asia/Tokyo", Weight: 5},
	{Language: "ko-KR", Timezone: "Asia/Seoul", Weight: 4},
	{Language: "de-DE", Timezone: "Europe/Berlin", Weight: 4},
	{Language: "en-SG", Timezone: "Asia/Singapore", Weight: 4},
}

var (
	profileSKUPool       []profileSKU
	profileSKUByPlatform = map[string][]profileSKU{}
	profileLocalePool    []profileLocale
	profileSKUCombos     = map[[4]string]bool{}
	profileLocalePairs   = map[[2]string]bool{}
)

func init() {
	for _, sku := range profileSKUs {
		profileSKUCombos[[4]string{sku.Platform, sku.Arch, sku.OSVersion, sku.Screen}] = true
		for range sku.Weight {
			profileSKUPool = append(profileSKUPool, sku)
			profileSKUByPlatform[sku.Platform] = append(profileSKUByPlatform[sku.Platform], sku)
		}
	}
	for _, locale := range profileLocales {
		profileLocalePairs[[2]string{locale.Language, locale.Timezone}] = true
		for range locale.Weight {
			profileLocalePool = append(profileLocalePool, locale)
		}
	}
}

// isGeneratedProfile reports whether a stored profile already matches the
// current desktop SKU pool (coherent combo + real locale pair).
func isGeneratedProfile(p DeviceProfile) bool {
	combo := [4]string{p.Platform, p.Arch, p.OSVersion, p.Screen}
	pair := [2]string{p.Language, p.Timezone}
	return profileSKUCombos[combo] && profileLocalePairs[pair]
}

func pickSKU() profileSKU {
	return profileSKUPool[rand.IntN(len(profileSKUPool))]
}

// pickSKUFor keeps the platform of a profile that is being repaired, so a
// migration does not turn a Windows device into a Mac (only host-cloned linux
// profiles have no matching pool and get a full re-roll).
func pickSKUFor(platform string) profileSKU {
	pool := profileSKUByPlatform[platform]
	if len(pool) == 0 {
		return pickSKU()
	}
	return pool[rand.IntN(len(pool))]
}

func pickLocale() profileLocale {
	return profileLocalePool[rand.IntN(len(profileLocalePool))]
}

func generateProfile() DeviceProfile {
	sku := pickSKU()
	locale := pickLocale()
	return DeviceProfile{
		Platform:  sku.Platform,
		Arch:      sku.Arch,
		OSVersion: sku.OSVersion,
		Screen:    sku.Screen,
		Language:  locale.Language,
		Timezone:  locale.Timezone,
		DeviceMid: randomUUID(),
	}
}

// upgradeProfile replaces a legacy profile (host-cloned, low-DPI or otherwise
// outside the SKU pool) with a generated one while keeping the device_mid, so
// the account's identity stays stable for the upstream.
func upgradeProfile(p DeviceProfile) (DeviceProfile, bool) {
	if isGeneratedProfile(p) {
		return p, false
	}
	sku := pickSKUFor(p.Platform)
	locale := pickLocale()
	p.Platform = sku.Platform
	p.Arch = sku.Arch
	p.OSVersion = sku.OSVersion
	p.Screen = sku.Screen
	p.Language = locale.Language
	p.Timezone = locale.Timezone
	if p.DeviceMid == "" {
		p.DeviceMid = randomUUID()
	}
	return p, true
}

// uniqueDeviceMid returns a device id no stored account profile uses.
func (s *Store) uniqueDeviceMid() string {
	for range 8 {
		mid := randomUUID()
		if s.db == nil {
			return mid
		}
		var one int
		if err := s.db.sql.QueryRow(`SELECT 1 FROM account_profiles WHERE device_mid = ? LIMIT 1`, mid).Scan(&one); err != nil {
			return mid
		}
	}
	return randomUUID()
}

func (s *Store) EnsureProfile(acc *Account) *DeviceProfile {
	if acc == nil {
		p := generateProfile()
		return &p
	}
	if p := acc.Profile; p != nil && p.Platform != "" && p.Arch != "" && p.OSVersion != "" && p.DeviceMid != "" {
		if acc.Concurrency == 0 {
			acc.Concurrency = 1
		}
		return p
	}
	if row, err := s.loadProfile(acc.ID); err == nil && row.Found {
		p := row.Profile
		if upgraded, changed := upgradeProfile(p); changed {
			p = upgraded
			_ = s.saveProfile(acc.ID, p, row.Concurrency, row.LastActivationDate)
		}
		acc.Profile = &p
		acc.DeviceMid = p.DeviceMid
		acc.Concurrency = clampConcurrency(row.Concurrency)
		acc.LastActivationDate = row.LastActivationDate
		return &p
	}
	p := generateProfile()
	p.DeviceMid = s.uniqueDeviceMid()
	acc.Profile = &p
	acc.DeviceMid = p.DeviceMid
	acc.Concurrency = 1
	_ = s.saveProfile(acc.ID, p, 1, "")
	return &p
}

// RotateProfile gives an account a brand-new device identity: a fresh desktop
// SKU, a real locale pair and a new UUIDv4 device_mid. The old identity is
// replaced in memory and on disk.
func (s *Store) RotateProfile(accountID string) *DeviceProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, acc := range s.accounts {
		if acc.ID != accountID {
			continue
		}
		p := generateProfile()
		p.DeviceMid = s.uniqueDeviceMid()
		acc.Profile = &p
		acc.DeviceMid = p.DeviceMid
		_ = s.saveProfile(accountID, p, acc.Concurrency, acc.LastActivationDate)
		return &p
	}
	return nil
}

// ProfileOf returns the account's profile without generating or saving one.
func (s *Store) ProfileOf(acc *Account) DeviceProfile {
	if acc != nil && acc.Profile != nil {
		return *acc.Profile
	}
	if acc != nil && s.db != nil {
		if row, err := s.loadProfile(acc.ID); err == nil && row.Found {
			return row.Profile
		}
	}
	return DeviceProfile{}
}

func (s *Store) SetConcurrency(accountID string, n int) bool {
	n = clampConcurrency(n)
	ok := s.UpdateAccountMemory(accountID, func(a *Account) {
		a.Concurrency = n
		if a.Profile != nil {
			_ = s.saveProfile(accountID, *a.Profile, n, a.LastActivationDate)
		}
	})
	if ok {
		_ = s.UpdateAccount(accountID, func(a *Account) {
			a.Concurrency = n
		})
	}
	return ok
}

func (s *Store) SetActivationDate(accountID, date string) {
	s.UpdateAccountMemory(accountID, func(a *Account) {
		a.LastActivationDate = date
		if a.Profile != nil {
			_ = s.saveProfile(accountID, *a.Profile, a.Concurrency, date)
		}
	})
}

func osCategoryFor(platform string) string {
	switch platform {
	case "darwin", "macos":
		return "macos"
	case "win32", "windows":
		return "windows"
	default:
		return "linux"
	}
}

// identityHeaders maps a device profile to the ZCode client identity headers.
// Every value comes from the stored profile; nothing is read from the host.
func identityHeaders(p DeviceProfile) map[string]string {
	return map[string]string{
		"User-Agent":          "ZCode/" + appVersion,
		"HTTP-Referer":        "https://zcode.z.ai",
		"X-Title":             "Z Code@electron",
		"X-ZCode-App-Version": appVersion,
		"X-Platform":          p.Platform + "-" + p.Arch,
		"X-Release-Channel":   envOr("ZCODE_RELEASE_CHANNEL", "production"),
		"X-ZCode-Agent":       "glm",
		"X-Client-Language":   p.Language,
		"X-Client-Timezone":   p.Timezone,
		"X-Os-Category":       osCategoryFor(p.Platform),
		"X-Os-Version":        p.OSVersion,
		"X-Device-Mid":        p.DeviceMid,
	}
}

// profileHeaders is the billing/plan family header set. It keeps the "stable"
// release channel the billing API expects and drops the agent marker.
func (s *Server) profileHeaders(acc *Account) map[string]string {
	h := identityHeaders(*s.store.EnsureProfile(acc))
	delete(h, "X-ZCode-Agent")
	h["X-Release-Channel"] = "stable"
	h["x-request-id"] = randomUUID()
	return h
}

func ensureProfileColumns(db *DB) error {
	// account_profiles is created in OpenDB. This function is the migration
	// hook for future columns; keep it additive and idempotent.
	_, err := db.sql.Exec(`CREATE TABLE IF NOT EXISTS account_profiles (
  account_id TEXT PRIMARY KEY,
  platform TEXT NOT NULL,
  arch TEXT NOT NULL,
  os_version TEXT NOT NULL,
  language TEXT NOT NULL,
  timezone TEXT NOT NULL,
  screen TEXT NOT NULL,
  device_mid TEXT NOT NULL,
  concurrency INTEGER NOT NULL DEFAULT 1,
  last_activation_date TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`)
	return err
}

func (d *DB) DeleteAccountProfile(accountID string) {
	_, _ = d.sql.Exec(`DELETE FROM account_profiles WHERE account_id = ?`, accountID)
}

func (s *Store) attachProfiles() {
	for _, acc := range s.accounts {
		s.EnsureProfile(acc)
	}
}

func normalizeConcurrency(n int) int { return clampConcurrency(n) }

var _ = strings.TrimSpace
