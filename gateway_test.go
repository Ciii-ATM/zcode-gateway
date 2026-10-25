package main

import (
	"context"
	"errors"
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

	// Same sticky session while its account is busy must wait.
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
