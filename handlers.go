package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

type Server struct {
	store    *Store
	upstream *Upstream
	oauth    *OAuthManager
	pool     *Pool
	start    time.Time
	baseURL  string
	version  string

	adminPass      string
	adminHash      string
	adminGenerated bool
}

func NewServer(store *Store, upstream *Upstream, baseURL string) *Server {
	server := &Server{
		store:    store,
		upstream: upstream,
		oauth:    NewOAuthManager(upstream, store),
		pool:     NewPool(store),
		start:    time.Now(),
		baseURL:  baseURL,
		version:  appVersion,
	}
	server.initAdminPassword()
	return server
}

func (s *Server) summary(acc *Account) *AccountSummary {
	if acc == nil {
		return nil
	}
	status := "可用"
	switch {
	case !acc.Enabled:
		status = "已禁用"
	case acc.Quota.Granted > 0 && acc.Quota.Remaining <= 0:
		status = "额度耗尽"
	case acc.ExhaustedUntil > time.Now().Unix():
		status = "冷却中"
	}
	return &AccountSummary{
		ID: acc.ID, Label: acc.Label, Provider: acc.Provider, Mode: acc.Mode,
		Priority: acc.Priority, Enabled: acc.Enabled, Status: status,
		Concurrency: clampConcurrency(acc.Concurrency),
		User:        acc.User, Quota: acc.Quota, LastError: acc.LastError,
		ExhaustedUntil: acc.ExhaustedUntil,
		LastUsedAt:     acc.LastUsedAt, CreatedAt: acc.CreatedAt, Usage: acc.Usage,
		HasProject: acc.Project != nil,
	}
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	accounts := s.store.Summaries()
	overview := Overview{Recent: s.store.Logs(12, "", "")}
	overview.Accounts.Total = len(accounts)
	for _, acc := range accounts {
		if acc.Enabled {
			overview.Accounts.Enabled++
		}
		if acc.Enabled && acc.Quota.Remaining > 0 {
			overview.Accounts.Available++
		}
		if acc.ID == s.store.Settings().ActiveAccountID && acc.Enabled && (acc.Quota.Remaining > 0 || acc.Status == "可用") {
			overview.ActiveAccount = acc
		}
	}
	if overview.ActiveAccount == nil {
		for _, acc := range accounts {
			if acc.Enabled {
				overview.ActiveAccount = acc
				break
			}
		}
	}
	req, total, u := s.store.TodayTotals()
	overview.Today.Requests = req
	overview.Today.TotalTokens = total
	overview.Today.InputTokens = u.InputTokens
	overview.Today.OutputTokens = u.OutputTokens
	overview.Today.CacheReadTokens = u.CacheReadTokens
	overview.Today.CacheWriteTokens = u.CacheWriteTokens
	overview.Today.ReasoningTokens = u.ReasoningTokens
	settings := s.store.Settings()
	overview.Server = ServerInfo{
		Version: s.version, BaseURL: s.baseURL, Fingerprint: settings.Fingerprint,
		QuotaSeconds: settings.QuotaIntervalSec, DataDir: s.store.dir,
	}
	writeJSON(w, 200, overview)
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"items": s.store.Summaries()})
	case http.MethodPost:
		var body struct {
			Provider string `json:"provider"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Provider == "" {
			body.Provider = "zai"
		}
		flow, err := s.oauth.Start(r.Context(), body.Provider)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"flowId": flow.ID, "authorizeUrl": flow.AuthorizeURL})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if err := s.store.ImportDesktop(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": s.store.Summaries()})
}

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	if flowID == "" {
		writeError(w, 400, "flowId required")
		return
	}
	status, acc, err := s.oauth.Status(r.Context(), flowID)
	if err != nil {
		writeJSON(w, 200, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	out := map[string]any{"status": status}
	if acc != nil {
		out["account"] = s.summary(acc)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeError(w, 400, "url required")
		return
	}
	acc, err := s.oauth.HandleCallback(r.Context(), body.URL)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "account": s.summary(acc)})
}

func (s *Server) handleAccountByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, 404, "account not found")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodPatch && action == "":
		var body struct {
			Label       *string `json:"label"`
			Priority    *int    `json:"priority"`
			Enabled     *bool   `json:"enabled"`
			Concurrency *int    `json:"concurrency"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		if !s.store.UpdateAccount(id, func(a *Account) {
			if body.Label != nil {
				a.Label = strings.TrimSpace(*body.Label)
			}
			if body.Priority != nil {
				a.Priority = *body.Priority
			}
			if body.Enabled != nil {
				a.Enabled = *body.Enabled
			}
			if body.Concurrency != nil {
				a.Concurrency = clampConcurrency(*body.Concurrency)
			}
		}) {
			writeError(w, 404, "account not found")
			return
		}
		if body.Concurrency != nil {
			s.store.SetConcurrency(id, *body.Concurrency)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodDelete && action == "":
		s.store.DeleteAccount(id)
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodPost && action == "refresh":
		acc := s.store.GetAccount(id)
		if acc == nil {
			writeError(w, 404, "account not found")
			return
		}
		if err := s.refreshQuota(r.Context(), acc); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "account": s.summary(s.store.GetAccount(id))})
	case r.Method == http.MethodPost && action == "clear-cooldown":
		if !s.store.UpdateAccount(id, func(a *Account) {
			a.ExhaustedUntil = 0
			a.LastError = ""
			a.LastErrorAt = 0
			a.RateLimitStreak = 0
		}) {
			writeError(w, 404, "account not found")
			return
		}
		s.store.ResetAccountModelStates(id)
		s.pool.signal()
		writeJSON(w, 200, map[string]any{"ok": true, "account": s.summary(s.store.GetAccount(id))})
	case r.Method == http.MethodGet && action == "activities":
		acc := s.store.GetAccount(id)
		if acc == nil {
			writeError(w, 404, "account not found")
			return
		}
		snapshot, err := s.detectActivities(r.Context(), acc)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, snapshot)
	case r.Method == http.MethodPost && action == "claim":
		acc := s.store.GetAccount(id)
		if acc == nil {
			writeError(w, 404, "account not found")
			return
		}
		var body struct {
			PlanID             string `json:"planId"`
			CaptchaVerifyParam string `json:"captchaVerifyParam"`
			CaptchaRegion      string `json:"captchaRegion"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		result, err := s.claimActivity(r.Context(), acc, body.PlanID, body.CaptchaVerifyParam, body.CaptchaRegion)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, result)
	default:
		writeError(w, 404, "not found")
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/logs")
	if strings.HasPrefix(path, "/") && len(path) > 1 {
		id := strings.TrimPrefix(path, "/")
		if e := s.store.GetLog(id); e != nil {
			writeJSON(w, 200, e)
			return
		}
		writeError(w, 404, "log not found")
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	status := r.URL.Query().Get("status")
	accountID := r.URL.Query().Get("accountId")
	writeJSON(w, 200, map[string]any{"items": s.store.Logs(limit, status, accountID)})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings := s.store.Settings()
		settings.AdminPasswordHash = ""
		writeJSON(w, 200, map[string]any{"settings": settings, "server": ServerInfo{
			Version: s.version, BaseURL: s.baseURL, Fingerprint: settings.Fingerprint,
			QuotaSeconds: settings.QuotaIntervalSec, DataDir: s.store.dir,
		}})
	case http.MethodPatch:
		var body struct {
			ProxyToken       *string `json:"proxyToken"`
			Fingerprint      *bool   `json:"fingerprint"`
			StoreBodies      *bool   `json:"storeBodies"`
			QuotaIntervalSec *int    `json:"quotaIntervalSec"`
			PromptMode       *string `json:"promptMode"`
			UAProfile        *string `json:"uaProfile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, 400, "invalid json")
			return
		}
		s.store.UpdateSettings(func(st *Settings) {
			if body.ProxyToken != nil {
				st.ProxyToken = strings.TrimSpace(*body.ProxyToken)
			}
			if body.Fingerprint != nil {
				st.Fingerprint = *body.Fingerprint
				st.FingerprintSet = true
			}
			if body.StoreBodies != nil {
				st.StoreBodies = *body.StoreBodies
			}
			if body.QuotaIntervalSec != nil && *body.QuotaIntervalSec >= 10 {
				st.QuotaIntervalSec = *body.QuotaIntervalSec
			}
			if body.PromptMode != nil {
				switch *body.PromptMode {
				case "append", "replace":
					st.PromptMode = *body.PromptMode
				}
			}
			if body.UAProfile != nil {
				switch *body.UAProfile {
				case "zcode", "claude-code", "chrome":
					st.UAProfile = *body.UAProfile
				}
			}
		})
		settings := s.store.Settings()
		settings.AdminPasswordHash = ""
		writeJSON(w, 200, map[string]any{"ok": true, "settings": settings})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := []map[string]any{
		{"id": "GLM-5.3-Flash", "object": "model", "created": s.start.Unix(), "owned_by": "zcode"},
		{"id": "GLM-5.3", "object": "model", "created": s.start.Unix(), "owned_by": "zcode"},
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	inFlight, forced := s.pool.Stats()
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": s.version, "uptime": time.Since(s.start).Seconds(),
		"accounts": len(s.store.Summaries()),
		"pool":     map[string]any{"inFlight": inFlight, "forcedReleases": forced},
	})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeError(w, 401, "invalid local proxy token")
		return
	}
	var body map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		writeError(w, 400, "request body is not valid JSON")
		return
	}
	if _, ok := body["messages"]; !ok {
		writeError(w, 400, "request body requires messages[]")
		return
	}
	stream, _ := body["stream"].(bool)
	model, _ := body["model"].(string)
	if model == "" {
		model = "GLM-5.3-Flash"
	}
	clientHeaders := map[string]string{
		"anthropic-version": r.Header.Get("anthropic-version"),
		"anthropic-beta":    r.Header.Get("anthropic-beta"),
	}
	clientReqBody := ""
	if s.store.Settings().StoreBodies {
		if data, err := json.Marshal(body); err == nil {
			clientReqBody = truncate(string(data), 8192)
		}
	}
	start := time.Now()
	sessionKey := extractSessionKey(r, body)
	accounts := s.store.AccountsForRefresh()
	maxAttempts := len(accounts) + 1
	requestTimeout := envSeconds("ZCODE_REQUEST_TIMEOUT_SEC", 600)
	var lastStatus int
	var lastBody []byte
	var lastErr error
	tried := map[string]bool{}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		acc, release, err := s.pool.AcquireExcludingModel(r.Context(), sessionKey, tried, model)
		if err != nil {
			if errors.Is(err, errNoAvailableAccount) {
				if lastStatus != 0 {
					break
				}
				writeError(w, 503, "no available account")
				return
			}
			if r.Context().Err() != nil {
				return
			}
			writeError(w, 502, err.Error())
			return
		}
		tried[acc.ID] = true
		done := false
		func() {
			defer release()
			ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
			defer cancel()
			resp, reqBytes, err := s.Forward(ctx, acc, body, clientHeaders)
			if err != nil {
				lastErr = err
				if r.Context().Err() == nil {
					if model != "" {
						s.store.RecordModelFailure(acc.ID, model, err.Error())
					} else {
						s.store.MarkExhausted(acc.ID, time.Now().Add(30*time.Second), err.Error())
					}
					s.pool.Invalidate(sessionKey, acc.ID)
					s.pool.InvalidateAccount(acc.ID)
				}
				return
			}
			if resp.StatusCode == 200 {
				usage, timing, respBytes, respPreview := s.writeUpstream(ctx, w, resp, stream, start)
				s.recordLog(acc, model, stream, 200, true, time.Since(start), usage, timing, resp.Header.Get("x-request-id"), sessionKey, r, clientReqBody, respPreview, len(reqBytes), respBytes, "")
				done = true
				return
			}
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			lastStatus = resp.StatusCode
			lastBody = data
			retry, cooldown, reason, _ := classifyUpstream(resp.StatusCode, data)
			if retry {
				if resp.StatusCode == 401 || resp.StatusCode == 403 {
					s.store.MarkExhausted(acc.ID, time.Now().Add(cooldown), reason)
				} else if model != "" {
					s.store.RecordModelFailure(acc.ID, model, reason)
				} else {
					s.store.MarkExhausted(acc.ID, time.Now().Add(cooldown), reason)
				}
				s.pool.Invalidate(sessionKey, acc.ID)
				s.pool.InvalidateAccount(acc.ID)
				return
			}
			s.recordLog(acc, model, stream, resp.StatusCode, false, time.Since(start), Usage{}, streamTiming{}, resp.Header.Get("x-request-id"), sessionKey, r, clientReqBody, truncate(string(data), 8192), len(reqBytes), len(data), reason)
			writeUpstreamError(w, resp.StatusCode, data)
			done = true
		}()
		if done || r.Context().Err() != nil {
			return
		}
	}
	if lastStatus == 0 {
		if lastErr != nil {
			writeError(w, 502, lastErr.Error())
		} else {
			writeError(w, 503, "no available account")
		}
		return
	}
	writeUpstreamError(w, lastStatus, lastBody)
}

type streamTiming struct {
	firstTokenMs int64
	lastTokenMs  int64
}

// timingMetrics turns the first/last token timestamps into the generation
// window and decode speed. Thinking tokens count toward generated output.
func timingMetrics(usage Usage, timing streamTiming) (int64, float64) {
	streamMs := timing.lastTokenMs - timing.firstTokenMs
	if streamMs < 0 {
		streamMs = 0
	}
	// ZCode/Anthropic usage.output_tokens already includes thinking tokens;
	// reasoning_tokens is a separate (often estimated) breakdown, so adding it
	// would double-count the thinking portion of the decode speed.
	tokens := usage.OutputTokens
	if streamMs == 0 || tokens == 0 {
		return streamMs, 0
	}
	return streamMs, float64(tokens) * 1000 / float64(streamMs)
}

func (s *Server) writeUpstream(ctx context.Context, w http.ResponseWriter, resp *fhttp.Response, stream bool, start time.Time) (Usage, streamTiming, int, string) {
	defer resp.Body.Close()
	// Close the upstream body when the caller goes away so a disconnected
	// downstream cannot pin the account slot.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = resp.Body.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	for _, name := range []string{"content-type", "cache-control", "x-request-id", "request-id", "x-zcode-request-id", "retry-after"} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(resp.StatusCode)
	if !stream {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_, _ = w.Write(data)
		return parseUsageFromJSON(data), streamTiming{}, len(data), truncate(string(data), 8192)
	}
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	tracker := &sseUsageTracker{}
	total := 0
	var preview strings.Builder
	var timing streamTiming
	var firstTokenAt, lastTokenAt time.Time
	for {
		if ctx.Err() != nil {
			break
		}
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			total += len(line)
			if preview.Len() < 8192 {
				preview.WriteString(line)
			}
			if tracker.feed(line) {
				now := time.Now()
				if firstTokenAt.IsZero() {
					firstTokenAt = now
				}
				lastTokenAt = now
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if !firstTokenAt.IsZero() {
		timing.firstTokenMs = firstTokenAt.Sub(start).Milliseconds()
		timing.lastTokenMs = lastTokenAt.Sub(start).Milliseconds()
	}
	return tracker.finish(), timing, total, truncate(preview.String(), 8192)
}

func (s *Server) recordLog(acc *Account, model string, stream bool, status int, ok bool, dur time.Duration, usage Usage, timing streamTiming, requestID, sessionKey string, r *http.Request, reqBody, respBody string, reqBytes, respBytes int, errMsg string) {
	streamMs, tokensPerSec := timingMetrics(usage, timing)
	entry := &LogEntry{
		ID: randomID(), Time: time.Now().Unix(), AccountID: acc.ID, AccountLabel: acc.Label,
		Model: model, Mode: acc.Mode, Status: status, OK: ok, DurationMs: dur.Milliseconds(),
		FirstTokenMs: timing.firstTokenMs, LastTokenMs: timing.lastTokenMs,
		StreamMs: streamMs, TokensPerSec: tokensPerSec,
		Stream: stream, Usage: usage, UpstreamStatus: status, RequestID: requestID, SessionID: sessionKey,
		ClientIP: clientIP(r), RequestBytes: reqBytes, ResponseBytes: respBytes,
		Error: errMsg,
	}
	if s.store.Settings().StoreBodies {
		entry.RequestBody = reqBody
		entry.ResponseBody = respBody
	}
	s.store.appendLog(entry)
	if ok {
		s.store.ResetModelState(acc.ID, model)
		s.store.UpdateAccountMemory(acc.ID, func(a *Account) {
			a.AddUsage(model, usage)
			a.LastError = ""
			a.LastErrorAt = 0
			a.ExhaustedUntil = 0
			a.RateLimitStreak = 0
		})
	}
}

func (s *Server) authorized(r *http.Request) bool {
	token := s.store.Settings().ProxyToken
	if token == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	apiKey := r.Header.Get("x-api-key")
	return auth == "Bearer "+token || apiKey == token
}

const (
	rateLimitFreezeAfter = 3
	rateLimitCooldown    = time.Minute
)

func classifyUpstream(status int, body []byte) (bool, time.Duration, string, bool) {
	var parsed struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	msg := parsed.Msg
	if msg == "" {
		msg = parsed.Error.Message
	}
	if status == 429 || parsed.Code == 1113 {
		return true, rateLimitCooldown, firstNonEmpty(msg, "额度或限流"), true
	}
	if status == 401 || status == 403 {
		return true, 30 * time.Minute, firstNonEmpty(msg, "认证失败"), false
	}
	if status == 405 && parsed.Code == 3012 {
		return true, time.Minute, "上游风控 3012", false
	}
	if status >= 500 {
		return true, 30 * time.Second, firstNonEmpty(msg, fmt.Sprintf("上游 HTTP %d", status)), false
	}
	return false, 0, firstNonEmpty(msg, fmt.Sprintf("上游 HTTP %d", status)), false
}

func writeUpstreamError(w http.ResponseWriter, status int, body []byte) {
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		parsed = map[string]any{"type": "error", "error": map[string]any{"type": "upstream_error", "message": string(body)}}
	}
	writeJSON(w, status, parsed)
}

func decodeJSON(r *http.Request, target any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = 500
		data = []byte(`{"error":"encode failed"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message}})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
