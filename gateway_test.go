package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPickAccountRotatesOnExhaustion(t *testing.T) {
	dir, err := os.MkdirTemp("", "zcg-rate-test-")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = os.RemoveAll(dir)
	})
	store.accounts = nil
	store.settings.ActiveAccountID = ""
	a1 := &Account{ID: "a1", Label: "first", Priority: 1, Enabled: true, CreatedAt: 1, Models: map[string]*UsageTotals{}}
	a2 := &Account{ID: "a2", Label: "second", Priority: 2, Enabled: true, CreatedAt: 2, Models: map[string]*UsageTotals{}}
	if err := store.AddAccount(a1); err != nil {
		t.Fatal(err)
	}
	if err := store.AddAccount(a2); err != nil {
		t.Fatal(err)
	}
	if got := store.PickAccount(time.Now()); got == nil || got.ID != "a1" {
		t.Fatalf("expected first account, got %#v", got)
	}
	store.MarkExhausted("a1", time.Now().Add(time.Hour), "quota")
	if got := store.PickAccount(time.Now()); got == nil || got.ID != "a2" {
		t.Fatalf("expected fallback account, got %#v", got)
	}
	store.UpdateAccount("a1", func(a *Account) {
		a.ExhaustedUntil = 0
		a.LastError = ""
	})
	if got := store.PickAccount(time.Now()); got == nil || got.ID != "a1" {
		t.Fatalf("expected recovery to first account, got %#v", got)
	}
}

func TestSystemFingerprintInjection(t *testing.T) {
	blocks := withSystemFingerprint(nil)
	if len(blocks) != 2 || blockText(blocks[0]) != SystemHead || blockText(blocks[1]) != SystemPrefix {
		t.Fatalf("unexpected fingerprint blocks: %#v", blocks)
	}
	blocks = withSystemFingerprint("client system")
	if len(blocks) != 3 || blockText(blocks[2]) != "client system" {
		t.Fatalf("client system not appended: %#v", blocks)
	}
	official := []any{
		map[string]any{"type": "text", "text": SystemHead},
		map[string]any{"type": "text", "text": SystemPrefix},
	}
	blocks = withSystemFingerprint(official)
	if len(blocks) != 2 {
		t.Fatalf("official fingerprint duplicated: %#v", blocks)
	}
}

func TestParseUsageAndStreamTracker(t *testing.T) {
	u := parseUsageFromJSON([]byte(`{"content":[{"type":"thinking","thinking":"12345678"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2}}`))
	if u.InputTokens != 10 || u.OutputTokens != 5 || u.CacheReadTokens != 2 || u.ReasoningTokens != 2 {
		t.Fatalf("unexpected usage: %#v", u)
	}
	tracker := &sseUsageTracker{}
	if tracker.feed(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":3}}}`) {
		t.Fatal("message_start must not count as a token")
	}
	if tracker.feed(`data: {"type":"message_delta","usage":{"output_tokens":5}}`) {
		t.Fatal("message_delta must not count as a token")
	}
	if !tracker.feed(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"abcdefgh"}}`) {
		t.Fatal("text_delta must count as a token")
	}
	if !tracker.feed(`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"abcdefgh"}}`) {
		t.Fatal("thinking_delta must count as a token")
	}
	got := tracker.finish()
	if got.InputTokens != 10 || got.OutputTokens != 5 || got.CacheReadTokens != 3 || got.ReasoningTokens != 2 {
		t.Fatalf("unexpected stream usage: %#v", got)
	}
}

func TestClassifyRateLimit(t *testing.T) {
	retry, cooldown, reason, rateLimited := classifyUpstream(429, []byte(`{"msg":"model concurrency limit exceeded"}`))
	if !retry || !rateLimited || cooldown != time.Minute || reason == "" {
		t.Fatalf("unexpected 429 classification: retry=%v cooldown=%v reason=%q rate=%v", retry, cooldown, reason, rateLimited)
	}
	retry, cooldown, _, rateLimited = classifyUpstream(400, []byte(`{"code":1113,"msg":"limit"}`))
	if !retry || !rateLimited || cooldown != time.Minute {
		t.Fatalf("unexpected code 1113 classification: retry=%v cooldown=%v rate=%v", retry, cooldown, rateLimited)
	}
	_, _, _, rateLimited = classifyUpstream(405, []byte(`{"code":3012}`))
	if rateLimited {
		t.Fatal("3012 must not be treated as a rate-limit streak")
	}
	if rateLimitFreezeAfter != 3 {
		t.Fatalf("rate limit threshold changed: %d", rateLimitFreezeAfter)
	}
}

func TestPoolExcludesTriedAccounts(t *testing.T) {
	store := newPoolTestStore(t)
	pool := NewPool(store)
	ctx := context.Background()
	acc, release, err := pool.AcquireExcluding(ctx, "session-a", map[string]bool{"a1": true})
	if err != nil || acc == nil || acc.ID != "a2" {
		t.Fatalf("expected a2, got %#v err=%v", acc, err)
	}
	release()
	if _, _, err := pool.AcquireExcluding(ctx, "session-b", map[string]bool{"a1": true, "a2": true}); !errors.Is(err, errNoAvailableAccount) {
		t.Fatalf("expected no available account, got %v", err)
	}
}

func TestModelCooldownEscalation(t *testing.T) {
	dir, err := os.MkdirTemp("", "zcg-model-state-")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(); _ = os.RemoveAll(dir) })
	now := int64(1000)
	for i := 0; i < 3; i++ {
		store.recordModelFailureAt("acc", "GLM-5.3-Flash", "429", now+int64(i))
	}
	state, ok := store.ModelStateFor("acc", "GLM-5.3-Flash")
	if !ok || state.Stage != 1 || state.CooldownUntil != now+2+60 || state.Streak != 0 {
		t.Fatalf("stage1 state: %#v ok=%v", state, ok)
	}
	if store.ModelHealthy("acc", "GLM-5.3-Flash", now+30) {
		t.Fatal("model should be cooling in stage 1")
	}
	if !store.ModelHealthy("acc", "GLM-5.3-Flash", now+63) {
		t.Fatal("model should recover after stage 1 cooldown")
	}
	base := now + 61
	for i := 0; i < 3; i++ {
		store.recordModelFailureAt("acc", "GLM-5.3-Flash", "429", base+int64(i))
	}
	state, _ = store.ModelStateFor("acc", "GLM-5.3-Flash")
	if state.Stage != 2 || state.CooldownUntil != base+2+600 {
		t.Fatalf("stage2 state: %#v", state)
	}
	base = state.CooldownUntil + 1
	for i := 0; i < 3; i++ {
		store.recordModelFailureAt("acc", "GLM-5.3-Flash", "429", base+int64(i))
	}
	state, _ = store.ModelStateFor("acc", "GLM-5.3-Flash")
	if !state.Disabled || state.Stage != 3 {
		t.Fatalf("stage3 state: %#v", state)
	}
	if store.ModelHealthy("acc", "GLM-5.3-Flash", base+10000) {
		t.Fatal("disabled model must not be scheduled")
	}
	store.ResetModelState("acc", "GLM-5.3-Flash")
	if !store.ModelHealthy("acc", "GLM-5.3-Flash", base+10000) {
		t.Fatal("reset should restore model")
	}
}

func TestPoolSkipsCoolingModelOnly(t *testing.T) {
	store := newPoolTestStore(t)
	for i := 0; i < 3; i++ {
		store.recordModelFailureAt("a1", "GLM-5.3-Flash", "429", time.Now().Unix()+int64(i))
	}
	pool := NewPool(store)
	acc, release, err := pool.AcquireForModel(context.Background(), "", "GLM-5.3-Flash")
	if err != nil || acc == nil || acc.ID != "a2" {
		t.Fatalf("cooling model should skip a1, got %#v err=%v", acc, err)
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := pool.AcquireExcludingModel(ctx, "", map[string]bool{"a2": true}, "GLM-5.3-Flash"); err == nil {
		t.Fatal("expected no account when only a1 has the cooling model")
	}
	other, releaseOther, err := pool.AcquireExcludingModel(context.Background(), "", map[string]bool{"a2": true}, "GLM-5.3")
	if err != nil || other.ID != "a1" {
		t.Fatalf("other model should still use a1, got %#v err=%v", other, err)
	}
	releaseOther()
}

func TestPoolHonorsPerAccountConcurrency(t *testing.T) {
	store := newPoolTestStore(t)
	store.UpdateAccountMemory("a1", func(a *Account) { a.Concurrency = 2 })
	pool := NewPool(store)
	excludeA2 := map[string]bool{"a2": true}
	r1, rel1, err := pool.AcquireExcluding(context.Background(), "", excludeA2)
	if err != nil || r1.ID != "a1" {
		t.Fatalf("first acquire: %v %#v", err, r1)
	}
	r2, rel2, err := pool.AcquireExcluding(context.Background(), "", excludeA2)
	if err != nil || r2.ID != "a1" {
		t.Fatalf("second acquire: %v %#v", err, r2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := pool.AcquireExcluding(ctx, "", excludeA2); err == nil {
		t.Fatal("third acquire should block while concurrency=2")
	}
	rel1()
	r3, rel3, err := pool.AcquireExcluding(context.Background(), "", excludeA2)
	if err != nil || r3.ID != "a1" {
		t.Fatalf("acquire after release: %v %#v", err, r3)
	}
	rel2()
	rel3()
}

func TestRateLimitFreezesAfterThreeConsecutiveFailures(t *testing.T) {
	dir, err := os.MkdirTemp("", "zcg-rate-test-")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = os.RemoveAll(dir)
	})
	store.accounts = nil
	store.settings.ProxyToken = ""
	acc := &Account{
		ID: "rate-test", Label: "rate-test", Provider: "zai", Mode: "start-plan",
		User: UserInfo{ID: "rate-user"}, Enabled: true, CreatedAt: 1,
		ZcodeJWT: "fake-token", Models: map[string]*UsageTotals{},
	}
	if err := store.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":1113,"msg":"model concurrency limit exceeded"}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("ZCODE_UPSTREAM", upstream.URL+"/v1/messages")
	server := NewServer(store, mustUpstream(t), "http://test")
	body := `{"model":"GLM-5.3-Flash","messages":[{"role":"user","content":"hi"}],"stream":false}`

	for attempt := 1; attempt <= 3; attempt++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		server.handleMessages(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: expected 429, got %d body=%s", attempt, rec.Code, rec.Body.String())
		}
		current := store.GetAccount("rate-test")
		state, _ := store.ModelStateFor("rate-test", "GLM-5.3-Flash")
		if attempt < 3 {
			if state.CooldownUntil != 0 {
				t.Fatalf("attempt %d: model cooled too early until %d", attempt, state.CooldownUntil)
			}
			if state.Streak != attempt {
				t.Fatalf("attempt %d: model streak=%d", attempt, state.Streak)
			}
		} else {
			if state.CooldownUntil <= time.Now().Unix() || state.CooldownUntil > time.Now().Add(61*time.Second).Unix() {
				t.Fatalf("attempt 3: unexpected model cooldown %d", state.CooldownUntil)
			}
			if state.Stage != 1 || state.Streak != 0 {
				t.Fatalf("attempt 3: stage=%d streak=%d", state.Stage, state.Streak)
			}
		}
		_ = current
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/rate-test/clear-cooldown", nil)
	server.handleAccountByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear cooldown: got %d body=%s", rec.Code, rec.Body.String())
	}
	current := store.GetAccount("rate-test")
	if current.ExhaustedUntil != 0 || current.LastError != "" {
		t.Fatalf("clear cooldown did not reset: %#v", current)
	}
	if _, ok := store.ModelStateFor("rate-test", "GLM-5.3-Flash"); ok {
		t.Fatal("clear cooldown did not reset model state")
	}
}

func mustUpstream(t *testing.T) *Upstream {
	t.Helper()
	upstream, err := NewUpstream()
	if err != nil {
		t.Fatal(err)
	}
	return upstream
}

func TestActivityTokenSelection(t *testing.T) {
	acc := &Account{AccessToken: "access-token", ZcodeJWT: "zcode-jwt"}
	if got := activityTokenForPath(acc, "/api/v1/marketing/touch"); got != "access-token" {
		t.Fatalf("marketing should use access token, got %q", got)
	}
	if got := activityTokenForPath(acc, "/api/v1/zcode-plan/billing/preview"); got != "access-token" {
		t.Fatalf("preview should use access token, got %q", got)
	}
	if got := activityTokenForPath(acc, "/api/v1/zcode-plan/billing/claim"); got != "zcode-jwt" {
		t.Fatalf("claim should use zcode JWT, got %q", got)
	}
	fallback := &Account{AccessToken: "access-token"}
	if got := activityTokenForPath(fallback, "/api/v1/zcode-plan/billing/claim"); got != "access-token" {
		t.Fatalf("claim fallback should use access token, got %q", got)
	}
}

func TestTimingMetrics(t *testing.T) {
	streamMs, speed := timingMetrics(
		Usage{OutputTokens: 90, ReasoningTokens: 10},
		streamTiming{firstTokenMs: 100, lastTokenMs: 1100},
	)
	if streamMs != 1000 || speed != 90 {
		t.Fatalf("expected 1000ms and 90 tok/s (output already includes thinking), got %dms and %f", streamMs, speed)
	}

	streamMs, speed = timingMetrics(Usage{OutputTokens: 10}, streamTiming{})
	if streamMs != 0 || speed != 0 {
		t.Fatalf("expected no timing for non-stream, got %dms and %f", streamMs, speed)
	}

	streamMs, speed = timingMetrics(
		Usage{OutputTokens: 10},
		streamTiming{firstTokenMs: 200, lastTokenMs: 100},
	)
	if streamMs != 0 || speed != 0 {
		t.Fatalf("expected clamped timing, got %dms and %f", streamMs, speed)
	}
}

func newPoolTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.accounts = nil
	store.settings.ActiveAccountID = ""
	for _, id := range []string{"a1", "a2"} {
		acc := &Account{
			ID: id, Label: id, Priority: len(store.accounts) + 1, Enabled: true,
			CreatedAt: int64(len(store.accounts) + 1), Models: map[string]*UsageTotals{},
			Quota: Quota{Granted: 1000, Remaining: 1000},
		}
		if err := store.AddAccount(acc); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestPoolStickyAndCapacity(t *testing.T) {
	store := newPoolTestStore(t)
	pool := NewPool(store)
	ctx := context.Background()

	first, releaseFirst, err := pool.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst()

	again, releaseAgain, err := pool.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAgain()
	if again.ID != first.ID {
		t.Fatalf("sticky session moved from %s to %s", first.ID, again.ID)
	}

	// Hold the sticky account and ask for a new session: it must use the other
	// account because each account has capacity one.
	other, releaseOther, err := pool.Acquire(ctx, "session-2")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOther()
	if other.ID == again.ID {
		t.Fatalf("concurrent session reused busy account %s", other.ID)
	}

	// Same sticky session while both accounts are busy has nowhere to borrow:
	// it waits (bounded by its context) instead of switching.
	blockedCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, _, err := pool.Acquire(blockedCtx, "session-1"); err == nil {
		t.Fatal("expected sticky acquire to block while account is busy")
	}
}

func TestExtractSessionKey(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Session-Id", "header-session")
	if got := extractSessionKey(req, nil); got != "header-session" {
		t.Fatalf("header session: %q", got)
	}
	body := map[string]any{"metadata": map[string]any{"user_id": "user_x_account__session_3bb23c9f-71e3-a8e9-70af-df6fc44a9183"}}
	if got := extractSessionKey(httptest.NewRequest("POST", "/", nil), body); got != "3bb23c9f-71e3-a8e9-70af-df6fc44a9183" {
		t.Fatalf("metadata session: %q", got)
	}
}

func TestSQLiteLogsAndTodayStats(t *testing.T) {
	store := newPoolTestStore(t)
	now := time.Now().Unix()
	for i, ok := range []bool{true, true, false} {
		store.appendLog(&LogEntry{
			ID: randomID(), Time: now - int64(i), AccountID: "a1", AccountLabel: "a1",
			Model: "GLM-5.3-Flash", Status: 200, OK: ok, Usage: Usage{InputTokens: 10, OutputTokens: 5},
			SessionID: "session-" + string(rune('a'+i)),
		})
	}
	logs := store.Logs(10, "", "a1")
	if len(logs) != 3 {
		t.Fatalf("expected 3 logs, got %d", len(logs))
	}
	stats := store.AccountTodayStats()["a1"]
	if stats == nil || stats.Requests != 3 || stats.Success != 2 || stats.Tokens != 45 {
		t.Fatalf("unexpected today stats: %#v", stats)
	}
}

func TestPoolSameSessionRace(t *testing.T) {
	store := newPoolTestStore(t)
	pool := NewPool(store)
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acc, release, err := pool.Acquire(context.Background(), "race-session")
			if err != nil {
				results <- "error:" + err.Error()
				return
			}
			results <- acc.ID
			time.Sleep(20 * time.Millisecond)
			release()
		}()
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for id := range results {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("same session used multiple accounts: %#v", seen)
	}
}

func TestPoolWatchdogFreesStuckSlot(t *testing.T) {
	t.Setenv("ZCODE_MAX_HOLD_SEC", "1")
	store := newPoolTestStore(t)
	pool := NewPool(store)

	acc, release, err := pool.Acquire(context.Background(), "watchdog-session")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an abandoned request: never release until after the watchdog.
	time.Sleep(1500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	again, releaseAgain, err := pool.Acquire(ctx, "watchdog-session")
	if err != nil {
		t.Fatalf("slot was not freed by watchdog: %v", err)
	}
	if again.ID != acc.ID {
		t.Fatalf("sticky session moved: %s -> %s", acc.ID, again.ID)
	}
	releaseAgain()
	release() // idempotent; must not block or panic
}

func TestAdminAuthFlow(t *testing.T) {
	store := newPoolTestStore(t)
	server := NewServer(store, &Upstream{}, "http://127.0.0.1:8787")
	server.adminPass = "s3cret"
	server.adminGenerated = false

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", server.handleLogin)
	mux.HandleFunc("/api/accounts", server.requireAdmin(server.handleAccounts))

	// Unauthenticated API must be rejected.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/accounts", nil))
	if rec.Code != 401 {
		t.Fatalf("expected 401 without login, got %d", rec.Code)
	}

	// Wrong password.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"nope"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expected 401 for wrong password, got %d", rec.Code)
	}

	// Correct password sets a session cookie that unlocks the API.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"s3cret"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 for login, got %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a cookie")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/accounts", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with session, got %d", rec.Code)
	}
}

func bindingOf(t *testing.T, store *Store, sessionKey string) string {
	t.Helper()
	if store.db == nil {
		t.Fatal("store has no db")
	}
	id, ok := store.db.GetBinding(sessionKey, time.Now().Unix())
	if !ok {
		return ""
	}
	return id
}

func TestPoolStickyPreferenceSurvivesBusyAccount(t *testing.T) {
	t.Setenv("ZCODE_STICKY_WAIT_MS", "50")
	store := newPoolTestStore(t)
	pool := NewPool(store)
	ctx := context.Background()

	// First acquire pins the session to a1.
	first, releaseFirst, err := pool.AcquireExcluding(ctx, "sticky", map[string]bool{"a2": true})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "a1" {
		t.Fatalf("expected a1, got %s", first.ID)
	}
	releaseFirst()

	// Another session occupies a1 so the preferred account is busy.
	_, releaseOther, err := pool.AcquireExcluding(ctx, "other", map[string]bool{"a2": true})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOther()

	// The sticky session waits out its budget, then borrows a2 for this
	// request without rewriting the preference.
	borrowed, releaseBorrowed, err := pool.Acquire(ctx, "sticky")
	if err != nil {
		t.Fatal(err)
	}
	if borrowed.ID != "a2" {
		t.Fatalf("expected fallback a2, got %s", borrowed.ID)
	}
	releaseBorrowed()
	if got := bindingOf(t, store, "sticky"); got != "a1" {
		t.Fatalf("fallback rewrote the preference: %s", got)
	}

	// Once a1 is free the session returns to it.
	releaseOther()
	again, releaseAgain, err := pool.Acquire(ctx, "sticky")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAgain()
	if again.ID != "a1" {
		t.Fatalf("session did not return to its preference: %s", again.ID)
	}
}

func TestPoolRetryExclusionKeepsPreference(t *testing.T) {
	store := newPoolTestStore(t)
	pool := NewPool(store)
	ctx := context.Background()

	acc, release, err := pool.Acquire(ctx, "session-x")
	if err != nil {
		t.Fatal(err)
	}
	release()

	// A retry that excludes the preferred account borrows the other one but
	// must not replace the binding.
	other, releaseOther, err := pool.AcquireExcluding(ctx, "session-x", map[string]bool{acc.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	releaseOther()
	if other.ID == acc.ID {
		t.Fatalf("excluded account was reused: %s", other.ID)
	}
	if got := bindingOf(t, store, "session-x"); got != acc.ID {
		t.Fatalf("retry exclusion rewrote the preference: %s", got)
	}
}

func TestPoolDropsBindingOnlyOnHardFailure(t *testing.T) {
	store := newPoolTestStore(t)
	server := NewServer(store, &Upstream{}, "http://127.0.0.1:8787")
	now := time.Now().Unix()

	// 429/5xx/network style failures are transient: the binding stays.
	store.db.Bind("transient", "a1", now, 600)
	server.dropBindingsIfAccountDead(store.GetAccount("a1"), false)
	if got := bindingOf(t, store, "transient"); got != "a1" {
		t.Fatalf("transient failure dropped the binding: %q", got)
	}

	// 401/403 is a hard failure and clears every session on that account.
	server.dropBindingsIfAccountDead(store.GetAccount("a1"), true)
	if got := bindingOf(t, store, "transient"); got != "" {
		t.Fatalf("auth failure kept the binding: %q", got)
	}

	// Disabled account is a hard failure.
	store.db.Bind("disabled", "a1", now, 600)
	store.UpdateAccount("a1", func(a *Account) { a.Enabled = false })
	server.dropBindingsIfAccountDead(store.GetAccount("a1"), false)
	if got := bindingOf(t, store, "disabled"); got != "" {
		t.Fatalf("disabled account kept the binding: %q", got)
	}

	// Quota exhaustion is a hard failure.
	store.UpdateAccount("a1", func(a *Account) {
		a.Enabled = true
		a.Quota = Quota{Granted: 10, Remaining: 0}
	})
	store.db.Bind("quota", "a1", now, 600)
	server.dropBindingsIfAccountDead(store.GetAccount("a1"), false)
	if got := bindingOf(t, store, "quota"); got != "" {
		t.Fatalf("exhausted account kept the binding: %q", got)
	}
}

func TestPoolTouchSessionSlidesTTL(t *testing.T) {
	store := newPoolTestStore(t)
	now := time.Now().Unix()
	store.db.Bind("slide", "a1", now, 10)
	expiry := func() int64 {
		var expires int64
		if err := store.db.sql.QueryRow(`SELECT expires_at FROM session_bindings WHERE session_key = ?`, "slide").Scan(&expires); err != nil {
			t.Fatal(err)
		}
		return expires
	}
	before := expiry()
	store.db.TouchBinding("slide", now+100, 10)
	after := expiry()
	if after <= before {
		t.Fatalf("touch did not slide the TTL: %d -> %d", before, after)
	}
	if want := int64(now + 110); after != want {
		t.Fatalf("expected expiry %d, got %d", want, after)
	}
	// An inactive session must still expire.
	if _, ok := store.db.GetBinding("slide", now+111); ok {
		t.Fatal("expired binding still served")
	}
}

func TestExtractSessionKeyCodexHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("session_id", "codex-session")
	if got := extractSessionKey(req, nil); got != "codex-session" {
		t.Fatalf("bare session_id header: %q", got)
	}
	req = httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("conversation_id", "conv-1")
	if got := extractSessionKey(req, nil); got != "conv-1" {
		t.Fatalf("bare conversation_id header: %q", got)
	}
	body := map[string]any{"session_id": "body-session"}
	if got := extractSessionKey(httptest.NewRequest("POST", "/", nil), body); got != "body-session" {
		t.Fatalf("body session_id: %q", got)
	}
}

func TestHandleMessagesKeepsBindingsOnRateLimit(t *testing.T) {
	store := newPoolTestStore(t)
	store.UpdateAccount("a1", func(a *Account) { a.ZcodeJWT = "token-a1" })
	store.UpdateAccount("a2", func(a *Account) { a.ZcodeJWT = "token-a2" })
	now := time.Now().Unix()
	store.db.Bind("victim-session", "a1", now, 600)
	store.db.Bind("retry-session", "a1", now, 600)

	var mu sync.Mutex
	a1Failures := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a1" {
			mu.Lock()
			a1Failures++
			fail := a1Failures == 1
			mu.Unlock()
			if fail {
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{"code":1113,"msg":"model concurrency limit exceeded"}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("ZCODE_UPSTREAM", upstream.URL)

	up, err := NewUpstream()
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, up, "http://127.0.0.1:8787")

	body := `{"model":"GLM-5.3-Flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	send := func(session string) int {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set("X-Session-Id", session)
		rec := httptest.NewRecorder()
		server.handleMessages(rec, req)
		return rec.Code
	}

	if code := send("retry-session"); code != 200 {
		t.Fatalf("expected retry to succeed on another account, got %d", code)
	}
	// The 429 must not clear the rate-limited account's other sessions.
	if got := bindingOf(t, store, "victim-session"); got != "a1" {
		t.Fatalf("transient 429 cleared an unrelated binding: %q", got)
	}
	// The failing session keeps its preference too.
	if got := bindingOf(t, store, "retry-session"); got != "a1" {
		t.Fatalf("transient 429 rewrote the session preference: %q", got)
	}
	lastAccount := func() string {
		var account string
		if err := store.db.sql.QueryRow(
			`SELECT account_id FROM logs WHERE session_id = ? ORDER BY rowid DESC LIMIT 1`,
			"retry-session").Scan(&account); err != nil {
			t.Fatal(err)
		}
		return account
	}
	if got := lastAccount(); got != "a2" {
		t.Fatalf("expected the retry to run on a2, got %q", got)
	}

	// Once a1 serves again the session returns to it.
	if code := send("retry-session"); code != 200 {
		t.Fatalf("expected recovery request to succeed, got %d", code)
	}
	if got := lastAccount(); got != "a1" {
		t.Fatalf("session did not return to its preference, got %q", got)
	}
}

func TestGeneratedProfilesAreDesktopSKUs(t *testing.T) {
	banned := map[string]bool{
		"1920x1080": true, "1366x768": true, "1280x720": true, "1600x900": true, "1440x900": true,
	}
	mids := map[string]bool{}
	for range 500 {
		p := generateProfile()
		if !isGeneratedProfile(p) {
			t.Fatalf("profile outside the SKU pool: %#v", p)
		}
		if banned[p.Screen] {
			t.Fatalf("low-DPI screen generated: %s", p.Screen)
		}
		if p.Platform != "darwin" && p.Platform != "win32" {
			t.Fatalf("non-desktop platform generated: %s", p.Platform)
		}
		var width, height int
		if _, err := fmt.Sscanf(p.Screen, "%dx%d", &width, &height); err != nil {
			t.Fatalf("unparseable screen %q", p.Screen)
		}
		if width < 1440 || height < 1080 {
			t.Fatalf("low-resolution screen generated: %s", p.Screen)
		}
		if len(p.DeviceMid) != 36 || mids[p.DeviceMid] {
			t.Fatalf("device_mid not unique UUID: %q", p.DeviceMid)
		}
		mids[p.DeviceMid] = true
	}
}

func TestEnsureProfileStableDistinctAndPersisted(t *testing.T) {
	store := newPoolTestStore(t)
	a1 := store.GetAccount("a1")
	a2 := store.GetAccount("a2")
	p1 := store.EnsureProfile(a1)
	p2 := store.EnsureProfile(a2)
	if p1.DeviceMid == p2.DeviceMid {
		t.Fatal("two accounts share a device_mid")
	}
	again := store.EnsureProfile(a1)
	if *again != *p1 {
		t.Fatalf("profile not stable: %#v -> %#v", p1, again)
	}
	row, err := store.loadProfile("a1")
	if err != nil || row.Profile.DeviceMid != p1.DeviceMid || row.Profile.Screen != p1.Screen {
		t.Fatalf("profile not persisted: %#v err=%v", row.Profile, err)
	}
}

func TestProfileUpgradeKeepsDeviceMid(t *testing.T) {
	store := newPoolTestStore(t)
	const mid = "11111111-1111-4111-8111-111111111111"
	legacy := DeviceProfile{
		Platform: "win32", Arch: "x64", OSVersion: "10.0.19045",
		Language: "zh-CN", Timezone: "Asia/Shanghai", Screen: "1920x1080", DeviceMid: mid,
	}
	if err := store.saveProfile("a1", legacy, 1, ""); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart: the in-memory profile is empty, so the row is loaded.
	store.UpdateAccountMemory("a1", func(a *Account) { a.Profile = nil })
	acc := store.GetAccount("a1")
	p := store.EnsureProfile(acc)
	if p.DeviceMid != mid {
		t.Fatalf("upgrade changed the device_mid: %s", p.DeviceMid)
	}
	if p.Screen == legacy.Screen || !isGeneratedProfile(*p) {
		t.Fatalf("legacy low-DPI profile was not upgraded: %#v", p)
	}
	row, err := store.loadProfile("a1")
	if err != nil || row.Profile.Screen != p.Screen {
		t.Fatalf("upgraded profile not persisted: %#v err=%v", row.Profile, err)
	}
}

func TestProfileUpgradeKeepsPlatform(t *testing.T) {
	store := newPoolTestStore(t)
	legacy := DeviceProfile{
		Platform: "darwin", Arch: "arm64", OSVersion: "24.6.0",
		Language: "en-US", Timezone: "America/New_York", Screen: "1512x982",
		DeviceMid: "33333333-3333-4333-8333-333333333333",
	}
	if err := store.saveProfile("a1", legacy, 1, ""); err != nil {
		t.Fatal(err)
	}
	store.UpdateAccountMemory("a1", func(a *Account) { a.Profile = nil })
	p := store.EnsureProfile(store.GetAccount("a1"))
	if p.Platform != "darwin" || p.Arch != "arm64" {
		t.Fatalf("upgrade changed the platform: %s-%s", p.Platform, p.Arch)
	}
	if p.DeviceMid != legacy.DeviceMid || p.Screen == legacy.Screen {
		t.Fatalf("unexpected upgraded profile: %#v", p)
	}
	if !isGeneratedProfile(*p) {
		t.Fatalf("upgraded profile outside the SKU pool: %#v", p)
	}
}

func TestIdentityHeadersFollowProfile(t *testing.T) {
	p := DeviceProfile{
		Platform: "darwin", Arch: "arm64", OSVersion: "24.6.0",
		Language: "ja-JP", Timezone: "Asia/Tokyo", Screen: "1728x1117",
		DeviceMid: "22222222-2222-4222-8222-222222222222",
	}
	h := identityHeaders(p)
	want := map[string]string{
		"X-Platform":        "darwin-arm64",
		"X-Os-Category":     "macos",
		"X-Os-Version":      "24.6.0",
		"X-Client-Language": "ja-JP",
		"X-Client-Timezone": "Asia/Tokyo",
		"X-Device-Mid":      p.DeviceMid,
	}
	for key, value := range want {
		if h[key] != value {
			t.Fatalf("%s = %q, want %q", key, h[key], value)
		}
	}
	if h["User-Agent"] != "ZCode/"+appVersion {
		t.Fatalf("unexpected UA %q", h["User-Agent"])
	}
}

func TestOAuthFlowProfileStable(t *testing.T) {
	flow := &OAuthFlow{}
	first := flow.profile()
	second := flow.profile()
	if first.DeviceMid == "" || first.DeviceMid != second.DeviceMid {
		t.Fatalf("oauth flow profile is not stable: %#v %#v", first, second)
	}
	if !isGeneratedProfile(second) {
		t.Fatalf("oauth flow profile outside the SKU pool: %#v", second)
	}
}

func TestMintProjectTokenUsesAccountProfile(t *testing.T) {
	store := newPoolTestStore(t)
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"project-token","expiresAt":4102444800}}`))
	}))
	defer srv.Close()
	store.UpdateAccount("a1", func(a *Account) {
		a.AccessToken = "access-token"
		a.Project = &ProjectLocation{OrganizationID: "org", ProjectID: "proj", APIKeyID: "key", Origin: srv.URL}
	})
	acc := store.GetAccount("a1")
	up, err := NewUpstream()
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, up, "http://127.0.0.1")
	token, expires, err := server.mintProjectToken(context.Background(), acc)
	if err != nil || token != "project-token" || expires != 4102444800 {
		t.Fatalf("mint failed: %q %d %v", token, expires, err)
	}
	profile := store.EnsureProfile(acc)
	if got := captured.Get("X-Device-Mid"); got != profile.DeviceMid {
		t.Fatalf("mint used device_mid %q, want %q", got, profile.DeviceMid)
	}
	if got := captured.Get("X-Platform"); got != profile.Platform+"-"+profile.Arch {
		t.Fatalf("mint used platform %q, want %s-%s", got, profile.Platform, profile.Arch)
	}
	if got := captured.Get("X-Os-Version"); got != profile.OSVersion {
		t.Fatalf("mint used os version %q, want %q", got, profile.OSVersion)
	}
}

func TestRotateProfileAssignsFreshIdentity(t *testing.T) {
	store := newPoolTestStore(t)
	before := *store.EnsureProfile(store.GetAccount("a1"))
	other := *store.EnsureProfile(store.GetAccount("a2"))

	rotated := store.RotateProfile("a1")
	if rotated == nil {
		t.Fatal("rotate returned nil")
	}
	if rotated.DeviceMid == before.DeviceMid || rotated.DeviceMid == other.DeviceMid {
		t.Fatalf("rotation reused a device_mid: %s", rotated.DeviceMid)
	}
	if !isGeneratedProfile(*rotated) {
		t.Fatalf("rotated profile outside the SKU pool: %#v", rotated)
	}
	stored, err := store.loadProfile("a1")
	if err != nil || stored.Profile.DeviceMid != rotated.DeviceMid {
		t.Fatalf("rotation not persisted: %#v err=%v", stored.Profile, err)
	}
	if current := store.EnsureProfile(store.GetAccount("a1")); current.DeviceMid != rotated.DeviceMid {
		t.Fatalf("in-memory profile not replaced: %#v", current)
	}
	if untouched := store.EnsureProfile(store.GetAccount("a2")); untouched.DeviceMid != other.DeviceMid {
		t.Fatalf("rotation touched another account: %#v", untouched)
	}
	if store.RotateProfile("missing") != nil {
		t.Fatal("rotate should return nil for unknown accounts")
	}
}
