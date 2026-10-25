package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type accountSlot struct {
	id       string
	mu       sync.Mutex
	limit    int
	inFlight int
	notify   chan struct{}
}

type sessionLock struct {
	mu   sync.Mutex
	refs int
}

type Pool struct {
	store     *Store
	mu        sync.Mutex
	slots     map[string]*accountSlot
	sessions  map[string]*sessionLock
	wake      chan struct{}
	stickyTTL int64
	maxHold   time.Duration
	inFlight  int
	forced    int64
}

func NewPool(store *Store) *Pool {
	ttl := int64(1800)
	if v := envOr("ZCODE_STICKY_TTL_SEC", ""); v != "" {
		if n, err := time.ParseDuration(v + "s"); err == nil {
			ttl = int64(n.Seconds())
		}
	}
	maxHold := 600 * time.Second
	if n, err := time.ParseDuration(envOr("ZCODE_MAX_HOLD_SEC", "600") + "s"); err == nil && n > 0 {
		maxHold = n
	}
	return &Pool{
		store:     store,
		slots:     map[string]*accountSlot{},
		sessions:  map[string]*sessionLock{},
		wake:      make(chan struct{}),
		stickyTTL: ttl,
		maxHold:   maxHold,
	}
}

func (p *Pool) slotFor(acc *Account) *accountSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if slot, ok := p.slots[acc.ID]; ok {
		return slot
	}
	slot := &accountSlot{id: acc.ID, limit: 1, notify: make(chan struct{})}
	p.slots[acc.ID] = slot
	return slot
}

func (p *Pool) signal() {
	p.mu.Lock()
	close(p.wake)
	p.wake = make(chan struct{})
	p.mu.Unlock()
}

func (p *Pool) healthy(acc *Account, now int64) bool {
	if acc == nil || !acc.Enabled || acc.ExhaustedUntil > now {
		return false
	}
	if acc.Quota.Granted > 0 && acc.Quota.Remaining <= 0 {
		return false
	}
	return true
}

func slotLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > 5 {
		return 5
	}
	return n
}

func (s *accountSlot) available(limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight < slotLimit(limit)
}

// holdSlot takes one of the account's concurrency slots. The returned release
// is idempotent and is also called by a watchdog after maxHold, so a stalled or
// abandoned request can never keep the account busy forever.
func (p *Pool) holdSlot(ctx context.Context, slot *accountSlot, limit int) (func(), error) {
	slot.mu.Lock()
	slot.limit = slotLimit(limit)
	for slot.inFlight >= slot.limit {
		notify := slot.notify
		slot.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		slot.mu.Lock()
	}
	slot.inFlight++
	slot.mu.Unlock()

	var once sync.Once
	var watchdog *time.Timer
	release := func() {
		once.Do(func() {
			watchdog.Stop()
			slot.mu.Lock()
			slot.inFlight--
			close(slot.notify)
			slot.notify = make(chan struct{})
			slot.mu.Unlock()
			p.mu.Lock()
			p.inFlight--
			p.mu.Unlock()
			p.signal()
		})
	}
	watchdog = time.AfterFunc(p.maxHold, func() {
		p.mu.Lock()
		p.forced++
		p.mu.Unlock()
		release()
	})

	p.mu.Lock()
	p.inFlight++
	p.mu.Unlock()
	return release, nil
}

// Acquire returns an available account and a release function. It enforces
// one in-flight request per account and sticky session routing.
func (p *Pool) Acquire(ctx context.Context, sessionKey string) (*Account, func(), error) {
	return p.AcquireExcludingModel(ctx, sessionKey, nil, "")
}

func (p *Pool) AcquireForModel(ctx context.Context, sessionKey, model string) (*Account, func(), error) {
	return p.AcquireExcludingModel(ctx, sessionKey, nil, model)
}

// AcquireExcluding behaves like Acquire but skips accounts already tried by
// the current request. This lets a rate-limited account be retried on a later
// request without cooling it inside one retry loop.
func (p *Pool) AcquireExcluding(ctx context.Context, sessionKey string, exclude map[string]bool) (*Account, func(), error) {
	return p.AcquireExcludingModel(ctx, sessionKey, exclude, "")
}

func (p *Pool) AcquireExcludingModel(ctx context.Context, sessionKey string, exclude map[string]bool, model string) (*Account, func(), error) {
	if sessionKey == "" {
		return p.acquire(ctx, "", exclude, model)
	}
	unlock := p.lockSession(sessionKey)
	acc, release, err := p.acquire(ctx, sessionKey, exclude, model)
	unlock()
	return acc, release, err
}

func (p *Pool) lockSession(key string) func() {
	p.mu.Lock()
	lock := p.sessions[key]
	if lock == nil {
		lock = &sessionLock{}
		p.sessions[key] = lock
	}
	lock.refs++
	p.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		p.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(p.sessions, key)
		}
		p.mu.Unlock()
	}
}

func (p *Pool) acquire(ctx context.Context, sessionKey string, exclude map[string]bool, model string) (*Account, func(), error) {
	now := time.Now().Unix()
	for {
		accounts := p.store.AccountsForRefresh()
		healthy := make([]*Account, 0, len(accounts))
		byID := map[string]*Account{}
		for _, acc := range accounts {
			byID[acc.ID] = acc
			if exclude[acc.ID] {
				continue
			}
			if p.healthy(acc, now) && p.store.ModelHealthy(acc.ID, model, now) {
				healthy = append(healthy, acc)
			}
		}
		if len(healthy) == 0 {
			return nil, nil, errNoAvailableAccount
		}
		// Drop slots for removed accounts.
		p.mu.Lock()
		for id := range p.slots {
			if _, ok := byID[id]; !ok {
				delete(p.slots, id)
			}
		}
		p.mu.Unlock()

		if sessionKey != "" && p.store.db != nil {
			if accountID, ok := p.store.db.GetBinding(sessionKey, now); ok {
				if acc, exists := byID[accountID]; exists && !exclude[accountID] && p.healthy(acc, now) && p.store.ModelHealthy(acc.ID, model, now) {
					slot := p.slotFor(acc)
					release, err := p.holdSlot(ctx, slot, acc.Concurrency)
					if err != nil {
						return nil, nil, err
					}
					p.store.db.TouchBinding(sessionKey, now, p.stickyTTL)
					p.store.SetActive(acc.ID)
					return acc, release, nil
				}
				p.store.db.Unbind(sessionKey)
			}
		}

		order := make([]*Account, len(healthy))
		copy(order, healthy)
		rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for _, acc := range order {
			slot := p.slotFor(acc)
			if !slot.available(acc.Concurrency) {
				continue
			}
			release, err := p.holdSlot(ctx, slot, acc.Concurrency)
			if err != nil {
				continue
			}
			if sessionKey != "" && p.store.db != nil {
				p.store.db.Bind(sessionKey, acc.ID, now, p.stickyTTL)
			}
			p.store.SetActive(acc.ID)
			return acc, release, nil
		}

		p.mu.Lock()
		wake := p.wake
		p.mu.Unlock()
		select {
		case <-wake:
			continue
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

func (p *Pool) Stats() (inFlight int, forced int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inFlight, p.forced
}

func (p *Pool) Invalidate(sessionKey, accountID string) {
	if p.store.db == nil {
		return
	}
	if sessionKey != "" {
		if bound, ok := p.store.db.GetBinding(sessionKey, time.Now().Unix()); ok && bound == accountID {
			p.store.db.Unbind(sessionKey)
		}
	}
}

func (p *Pool) InvalidateAccount(accountID string) {
	if p.store.db != nil {
		p.store.db.UnbindAccount(accountID)
	}
}

var errNoAvailableAccount = &poolError{"no available account"}

type poolError struct{ message string }

func (e *poolError) Error() string { return e.message }

var sessionPattern = regexp.MustCompile(`(?i)session[_-]([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

func extractSessionKey(r *http.Request, body map[string]any) string {
	for _, name := range []string{
		"X-Session-Id", "X-ZCode-Session-Id", "X-OMP-Session-Id",
		"X-Claude-Code-Session-Id", "X-Conversation-Id",
	} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return sessionKey(value)
		}
	}
	if metadata, ok := body["metadata"].(map[string]any); ok {
		for _, name := range []string{"session_id", "sessionId", "conversation_id", "conversationId"} {
			if value, ok := metadata[name].(string); ok && strings.TrimSpace(value) != "" {
				return sessionKey(value)
			}
		}
		if value, ok := metadata["user_id"].(string); ok {
			if match := sessionPattern.FindStringSubmatch(value); len(match) > 1 {
				return sessionKey(match[1])
			}
		}
	}
	return ""
}

func sessionKey(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 200 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
