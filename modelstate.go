package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	modelCooldownStage1 = time.Minute
	modelCooldownStage2 = 10 * time.Minute
	modelFailureLimit   = 3
)

func modelKey(accountID, model string) string {
	return accountID + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

func (d *DB) LoadModelStates() (map[string]ModelState, error) {
	rows, err := d.sql.Query(`SELECT account_id, model, streak, stage, cooldown_until, disabled, COALESCE(last_error,''), updated_at FROM model_states`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ModelState{}
	for rows.Next() {
		var state ModelState
		var disabled int
		if err := rows.Scan(&state.AccountID, &state.Model, &state.Streak, &state.Stage, &state.CooldownUntil, &disabled, &state.LastError, &state.UpdatedAt); err != nil {
			return nil, err
		}
		state.Disabled = disabled == 1
		out[modelKey(state.AccountID, state.Model)] = state
	}
	return out, rows.Err()
}

func (s *Store) loadModelStates() error {
	if s.db == nil {
		s.modelStates = map[string]ModelState{}
		return nil
	}
	states, err := s.db.LoadModelStates()
	if err != nil {
		return err
	}
	s.modelMu.Lock()
	s.modelStates = states
	s.modelMu.Unlock()
	return nil
}

func (s *Store) saveModelState(state ModelState) {
	if s.db == nil {
		return
	}
	disabled := 0
	if state.Disabled {
		disabled = 1
	}
	_, _ = s.db.sql.Exec(`INSERT INTO model_states
(account_id, model, streak, stage, cooldown_until, disabled, last_error, updated_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(account_id, model) DO UPDATE SET
streak=excluded.streak, stage=excluded.stage, cooldown_until=excluded.cooldown_until,
disabled=excluded.disabled, last_error=excluded.last_error, updated_at=excluded.updated_at`,
		state.AccountID, state.Model, state.Streak, state.Stage, state.CooldownUntil, disabled, state.LastError, state.UpdatedAt)
}

func (s *Store) RecordModelFailure(accountID, model, reason string) ModelState {
	return s.recordModelFailureAt(accountID, model, reason, time.Now().Unix())
}

func (s *Store) recordModelFailureAt(accountID, model, reason string, now int64) ModelState {
	model = strings.TrimSpace(model)
	state := ModelState{AccountID: accountID, Model: model, LastError: reason, UpdatedAt: now}
	key := modelKey(accountID, model)
	s.modelMu.Lock()
	if existing, ok := s.modelStates[key]; ok {
		state = existing
	}
	if state.Disabled {
		s.modelMu.Unlock()
		return state
	}
	state.Streak++
	if state.Streak >= modelFailureLimit {
		state.Streak = 0
		state.Stage++
		switch state.Stage {
		case 1:
			state.CooldownUntil = now + int64(modelCooldownStage1.Seconds())
		case 2:
			state.CooldownUntil = now + int64(modelCooldownStage2.Seconds())
		default:
			state.Disabled = true
			state.CooldownUntil = 0
		}
	}
	state.LastError = reason
	state.UpdatedAt = now
	s.modelStates[key] = state
	s.modelMu.Unlock()
	s.saveModelState(state)
	return state
}

func (s *Store) ModelHealthy(accountID, model string, now int64) bool {
	if strings.TrimSpace(model) == "" {
		return true
	}
	s.modelMu.RLock()
	state, ok := s.modelStates[modelKey(accountID, model)]
	s.modelMu.RUnlock()
	if !ok {
		return true
	}
	return !state.Disabled && state.CooldownUntil <= now
}

func (s *Store) ModelStateFor(accountID, model string) (ModelState, bool) {
	s.modelMu.RLock()
	state, ok := s.modelStates[modelKey(accountID, model)]
	s.modelMu.RUnlock()
	return state, ok
}

func (s *Store) ResetModelState(accountID, model string) {
	key := modelKey(accountID, model)
	s.modelMu.Lock()
	delete(s.modelStates, key)
	s.modelMu.Unlock()
	if s.db != nil {
		_, _ = s.db.sql.Exec(`DELETE FROM model_states WHERE account_id = ? AND lower(model) = lower(?)`, accountID, model)
	}
}

func (s *Store) ResetAccountModelStates(accountID string) {
	s.modelMu.Lock()
	for key, state := range s.modelStates {
		if state.AccountID == accountID {
			delete(s.modelStates, key)
		}
	}
	s.modelMu.Unlock()
	if s.db != nil {
		_, _ = s.db.sql.Exec(`DELETE FROM model_states WHERE account_id = ?`, accountID)
	}
}

func (s *Store) ModelStatesFor(accountID string) []ModelState {
	s.modelMu.RLock()
	out := make([]ModelState, 0)
	for _, state := range s.modelStates {
		if state.AccountID == accountID && (state.Disabled || state.Stage > 0 || state.Streak > 0 || state.CooldownUntil > 0) {
			out = append(out, state)
		}
	}
	s.modelMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func ensureModelStateColumns(db *DB) error {
	if _, err := db.sql.Exec(`CREATE TABLE IF NOT EXISTS model_states (
  account_id TEXT NOT NULL,
  model TEXT NOT NULL,
  streak INTEGER NOT NULL DEFAULT 0,
  stage INTEGER NOT NULL DEFAULT 0,
  cooldown_until INTEGER NOT NULL DEFAULT 0,
  disabled INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, model)
)`); err != nil {
		return err
	}
	rows, err := db.sql.Query(`PRAGMA table_info(model_states)`)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	columns := []struct{ name, ddl string }{
		{"streak", "INTEGER NOT NULL DEFAULT 0"},
		{"stage", "INTEGER NOT NULL DEFAULT 0"},
		{"cooldown_until", "INTEGER NOT NULL DEFAULT 0"},
		{"disabled", "INTEGER NOT NULL DEFAULT 0"},
		{"last_error", "TEXT"},
		{"updated_at", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if existing[column.name] {
			continue
		}
		if _, err := db.sql.Exec(fmt.Sprintf("ALTER TABLE model_states ADD COLUMN %s %s", column.name, column.ddl)); err != nil {
			return err
		}
	}
	return nil
}
