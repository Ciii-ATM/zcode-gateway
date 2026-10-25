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

func generateProfile() DeviceProfile {
	winProfiles := []DeviceProfile{
		{Platform: "win32", Arch: "x64", OSVersion: "10.0.22631", Language: "zh-CN", Timezone: "Asia/Shanghai", Screen: "1920x1080"},
		{Platform: "win32", Arch: "x64", OSVersion: "10.0.26100", Language: "zh-CN", Timezone: "Asia/Shanghai", Screen: "1920x1080"},
		{Platform: "win32", Arch: "x64", OSVersion: "10.0.26100", Language: "zh-CN", Timezone: "Asia/Shanghai", Screen: "2560x1440"},
		{Platform: "win32", Arch: "x64", OSVersion: "10.0.22621", Language: "zh-CN", Timezone: "Asia/Shanghai", Screen: "2560x1440"},
		{Platform: "win32", Arch: "x64", OSVersion: "10.0.19045", Language: "zh-CN", Timezone: "Asia/Shanghai", Screen: "1920x1080"},
		{Platform: "win32", Arch: "x64", OSVersion: "10.0.22631", Language: "en-US", Timezone: "America/New_York", Screen: "1920x1080"},
		{Platform: "win32", Arch: "x64", OSVersion: "10.0.26100", Language: "en-US", Timezone: "America/Los_Angeles", Screen: "2560x1440"},
	}
	macProfiles := []DeviceProfile{
		{Platform: "darwin", Arch: "arm64", OSVersion: "24.6.0", Language: "zh-CN", Timezone: "Asia/Shanghai", Screen: "1512x982"},
		{Platform: "darwin", Arch: "arm64", OSVersion: "25.5.0", Language: "en-US", Timezone: "America/New_York", Screen: "1728x1117"},
	}
	// 80% Windows desktop, 20% Apple silicon.
	var p DeviceProfile
	if rand.IntN(100) < 80 {
		p = winProfiles[rand.IntN(len(winProfiles))]
	} else {
		p = macProfiles[rand.IntN(len(macProfiles))]
	}
	p.DeviceMid = randomUUID()
	return p
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
		acc.Profile = &p
		acc.DeviceMid = p.DeviceMid
		acc.Concurrency = clampConcurrency(row.Concurrency)
		acc.LastActivationDate = row.LastActivationDate
		return &p
	}
	p := generateProfile()
	acc.Profile = &p
	acc.DeviceMid = p.DeviceMid
	acc.Concurrency = 1
	_ = s.saveProfile(acc.ID, p, 1, "")
	return &p
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

func (s *Server) profileHeaders(acc *Account) map[string]string {
	p := s.store.EnsureProfile(acc)
	osCategory := "linux"
	if p.Platform == "darwin" || p.Platform == "macos" {
		osCategory = "macos"
	} else if p.Platform == "win32" || p.Platform == "windows" {
		osCategory = "windows"
	}
	return map[string]string{
		"User-Agent":          "ZCode/" + appVersion,
		"HTTP-Referer":        "https://zcode.z.ai",
		"X-Title":             "Z Code@electron",
		"X-ZCode-App-Version": appVersion,
		"X-Platform":          p.Platform + "-" + p.Arch,
		"X-Release-Channel":   "stable",
		"X-Client-Language":   p.Language,
		"X-Client-Timezone":   p.Timezone,
		"X-Os-Category":       osCategory,
		"X-Os-Version":        p.OSVersion,
		"X-Device-Mid":        p.DeviceMid,
		"x-request-id":        randomUUID(),
	}
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
