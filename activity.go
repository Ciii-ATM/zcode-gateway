package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const zcodeAPIBase = "https://zcode.z.ai"

type ActivityEntitlement struct {
	EntitlementID string `json:"entitlementId"`
	Model         string `json:"model"`
	Meter         string `json:"meter,omitempty"`
	Unit          string `json:"unit,omitempty"`
	Amount        int64  `json:"amount"`
	Period        string `json:"period,omitempty"`
	EffectiveAt   int64  `json:"effectiveAt,omitempty"`
	Priority      int    `json:"priority,omitempty"`
}

type ActivityPlan struct {
	PlanID        string                `json:"planId"`
	Name          string                `json:"name"`
	Description   string                `json:"description,omitempty"`
	Priority      int                   `json:"priority"`
	StartsAt      int64                 `json:"startsAt,omitempty"`
	EndsAt        int64                 `json:"endsAt,omitempty"`
	EffectiveAt   int64                 `json:"effectiveAt,omitempty"`
	Claimable     bool                  `json:"claimable"`
	Claimed       bool                  `json:"claimed"`
	CurrentStatus string                `json:"currentStatus,omitempty"`
	Entitlements  []ActivityEntitlement `json:"entitlements"`
}

type ActivityCampaign struct {
	CampaignID string   `json:"campaignId"`
	Position   string   `json:"position"`
	Priority   int      `json:"priority"`
	PlanIDs    []string `json:"planIds"`
}

type ActivityCaptcha struct {
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"`
	Region  string `json:"region"`
	SceneID string `json:"sceneId"`
}

type ActivitySnapshot struct {
	ServerTime int64              `json:"serverTime"`
	Campaigns  []ActivityCampaign `json:"campaigns"`
	Plans      []ActivityPlan     `json:"plans"`
	Captcha    *ActivityCaptcha   `json:"captcha,omitempty"`
	Warnings   []string           `json:"warnings,omitempty"`
}

type ClaimResult struct {
	OK             bool          `json:"ok"`
	Code           int           `json:"code"`
	Message        string        `json:"message,omitempty"`
	UpstreamStatus int           `json:"upstreamStatus,omitempty"`
	Plan           *ActivityPlan `json:"plan,omitempty"`
}

func (s *Server) activityHeaders(acc *Account) map[string]string {
	return s.profileHeaders(acc)
}

func jwtUserID(acc *Account) string {
	token := strings.TrimSpace(acc.ZcodeJWT)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		UserID string `json:"user_id"`
		Sub    string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.UserID != "" {
		return claims.UserID
	}
	return claims.Sub
}

func (s *Server) reportActivation(ctx context.Context, acc *Account) []string {
	today := time.Now().Format("2006-01-02")
	if acc.LastActivationDate == today {
		return nil
	}
	userID := jwtUserID(acc)
	if userID == "" {
		return []string{"激活上报: JWT 无 user_id"}
	}
	p := s.store.EnsureProfile(acc)
	var warnings []string
	for _, element := range []string{"app_launch", "app_daily_active"} {
		body := map[string]any{
			"event_id":           randomUUID(),
			"client_timezone":    p.Timezone,
			"client_language":    p.Language,
			"element_name":       element,
			"event_region":       "app",
			"event_type":         "view",
			"event_text":         "",
			"event_extra_detail": map[string]any{},
			"user_id":            userID,
			"screen_resolution":  p.Screen,
			"app_version":        appVersion,
			"device_os_category": func() string {
				if p.Platform == "darwin" {
					return "macos"
				}
				if p.Platform == "win32" {
					return "windows"
				}
				return "linux"
			}(),
			"device_os_version": p.OSVersion,
			"device_mid":        p.DeviceMid,
			"mac_id":            "",
			"marketing_params":  "{}",
		}
		data, _ := json.Marshal(body)
		headers := map[string]string{"Content-Type": "application/json", "User-Agent": "ZCode/" + appVersion}
		callCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		resp, err := doRequest(callCtx, s.upstream, "POST", zcodeAPIBase+"/api/v1/event/report", headers, data)
		if err != nil {
			cancel()
			warnings = append(warnings, "激活事件: "+err.Error())
			continue
		}
		raw, _ := readJSON(resp)
		cancel()
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			if code, ok := activityInt(parsed, "code"); ok && code != 0 {
				warnings = append(warnings, "激活事件: "+activityResponseMessage(parsed, resp.StatusCode))
			}
		}
	}
	if len(warnings) == 0 {
		s.store.UpdateAccount(acc.ID, func(a *Account) { a.LastActivationDate = today })
		acc.LastActivationDate = today
	}
	return warnings
}

func activityToken(acc *Account) string {
	if token := strings.TrimSpace(acc.AccessToken); token != "" {
		return token
	}
	return strings.TrimSpace(acc.ZcodeJWT)
}

func activityTokenForPath(acc *Account, path string) string {
	// marketing/preview accept the OAuth access token; the billing claim
	// endpoint authenticates with the account's ZCode JWT instead.
	if strings.HasSuffix(path, "/claim") || strings.HasSuffix(path, "/current") {
		if token := strings.TrimSpace(acc.ZcodeJWT); token != "" {
			return token
		}
	}
	return activityToken(acc)
}

func (s *Server) zcodeAPICall(ctx context.Context, acc *Account, method, path string, query url.Values, payload any, extraHeaders map[string]string) (map[string]any, int, error) {
	token := activityTokenForPath(acc, path)
	if token == "" {
		return nil, 0, fmt.Errorf("账号没有可用的登录凭据")
	}
	headers := s.activityHeaders(acc)
	headers["Authorization"] = "Bearer " + token
	headers["Accept"] = "application/json"
	for key, value := range extraHeaders {
		if strings.TrimSpace(value) != "" {
			headers[key] = value
		}
	}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		headers["Content-Type"] = "application/json"
	}
	target := zcodeAPIBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := doRequest(callCtx, s.upstream, method, target, headers, body)
	if err != nil {
		return nil, 0, err
	}
	data, err := readJSON(resp)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("上游返回不是 JSON: %s", truncate(string(data), 240))
	}
	if resp.StatusCode != 200 {
		return parsed, resp.StatusCode, fmt.Errorf("%s", activityResponseMessage(parsed, resp.StatusCode))
	}
	return parsed, resp.StatusCode, nil
}

func activityResponseMessage(parsed map[string]any, status int) string {
	if parsed != nil {
		if msg := activityString(parsed, "msg"); msg != "" {
			return msg
		}
		if msg := activityString(parsed, "message"); msg != "" {
			return msg
		}
		if data, ok := parsed["data"].(map[string]any); ok {
			if msg := activityString(data, "message"); msg != "" {
				return msg
			}
		}
	}
	return fmt.Sprintf("上游 HTTP %d", status)
}

func (s *Server) currentPlans(ctx context.Context, acc *Account) (map[string]string, error) {
	parsed, _, err := s.zcodeAPICall(ctx, acc, "GET", "/api/v1/zcode-plan/billing/current", url.Values{
		"app_version": {appVersion},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	if code, ok := activityInt(parsed, "code"); ok && code != 0 {
		return nil, fmt.Errorf("%s", activityResponseMessage(parsed, 200))
	}
	out := map[string]string{}
	for _, item := range activitySlice(activityMap(parsed["data"]), "plans") {
		plan := activityMap(item)
		id := activityString(plan, "plan_id")
		if id == "" {
			continue
		}
		out[id] = activityString(plan, "status")
	}
	return out, nil
}

func (s *Server) detectActivities(ctx context.Context, acc *Account) (*ActivitySnapshot, error) {
	token := activityToken(acc)
	if token == "" {
		return nil, fmt.Errorf("账号没有可用的登录凭据，请重新导入账号")
	}
	planMaps := map[string]map[string]any{}
	var campaigns []ActivityCampaign
	warnings := s.reportActivation(ctx, acc)
	serverTime := time.Now().Unix()
	var marketingOK, previewOK bool

	marketing, _, marketingErr := s.zcodeAPICall(ctx, acc, "GET", "/api/v1/marketing/touch", url.Values{"seq": {"0"}}, nil, nil)
	if marketingErr != nil {
		warnings = append(warnings, "活动横幅: "+marketingErr.Error())
	} else if code, ok := activityInt(marketing, "code"); ok && code != 0 {
		warnings = append(warnings, "活动横幅: "+activityResponseMessage(marketing, 200))
	} else {
		marketingOK = true
		data := activityMap(marketing["data"])
		if v, ok := activityInt(data, "server_time"); ok && v > 0 {
			serverTime = int64(v)
		}
		collectPlanMaps(data, planMaps)
		deliveries, _ := data["deliveries"].([]any)
		for _, item := range deliveries {
			delivery := activityMap(item)
			if delivery == nil {
				continue
			}
			ids := collectPlanIDs(delivery)
			if len(ids) == 0 {
				continue
			}
			campaigns = append(campaigns, ActivityCampaign{
				CampaignID: activityString(delivery, "campaign_id"),
				Position:   activityString(delivery, "resource_position"),
				Priority:   activityIntDefault(delivery, "priority", 0),
				PlanIDs:    ids,
			})
		}
	}

	preview, _, previewErr := s.zcodeAPICall(ctx, acc, "GET", "/api/v1/zcode-plan/billing/preview", url.Values{
		"app_version": {appVersion},
		"platform":    {platformHeader()},
	}, nil, nil)
	if previewErr != nil {
		warnings = append(warnings, "套餐资格: "+previewErr.Error())
	} else if code, ok := activityInt(preview, "code"); ok && code != 0 {
		warnings = append(warnings, "套餐资格: "+activityResponseMessage(preview, 200))
	} else {
		previewOK = true
		data := activityMap(preview["data"])
		if v, ok := activityInt(data, "server_time"); ok && v > 0 {
			serverTime = int64(v)
		}
		collectPlanMaps(data, planMaps)
	}

	if !marketingOK && !previewOK {
		if len(warnings) > 0 {
			return nil, fmt.Errorf("%s", strings.Join(warnings, "；"))
		}
		return nil, fmt.Errorf("活动接口不可用")
	}

	activePlans, currentErr := s.currentPlans(ctx, acc)
	if currentErr != nil {
		warnings = append(warnings, "已领取状态: "+currentErr.Error())
		activePlans = nil
	}

	plans := make([]ActivityPlan, 0, len(planMaps))
	for _, raw := range planMaps {
		plan := activityPlanFromMap(raw)
		if plan.PlanID == "" {
			continue
		}
		if status, ok := activePlans[plan.PlanID]; ok {
			plan.Claimed = true
			plan.CurrentStatus = status
		}
		plans = append(plans, plan)
	}
	// Plans returned by preview are claimable, even if no banner plan metadata
	// carried a start/end window.
	if previewOK {
		data := activityMap(preview["data"])
		claimable := map[string]bool{}
		for _, item := range activitySlice(data, "plans") {
			if id := activityString(activityMap(item), "plan_id"); id != "" {
				claimable[id] = true
			}
		}
		for i := range plans {
			plans[i].Claimable = claimable[plans[i].PlanID]
		}
	}
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].Priority != plans[j].Priority {
			return plans[i].Priority > plans[j].Priority
		}
		return plans[i].Name < plans[j].Name
	})
	var captcha *ActivityCaptcha
	if configs, _, cfgErr := s.zcodeAPICall(ctx, acc, "GET", "/api/v1/client/configs", url.Values{
		"app_version": {appVersion},
		"platform":    {platformHeader()},
	}, nil, nil); cfgErr == nil {
		cfg := activityMap(activityMap(configs["data"])["configs"])
		if raw := activityMap(cfg["captcha"]); raw != nil {
			captcha = &ActivityCaptcha{
				Enabled: activityBool(raw, "enabled"),
				Prefix:  activityString(raw, "prefix"),
				Region:  activityString(raw, "region"),
				SceneID: activityString(raw, "sceneId"),
			}
		}
	}

	return &ActivitySnapshot{
		ServerTime: serverTime,
		Campaigns:  campaigns,
		Plans:      plans,
		Captcha:    captcha,
		Warnings:   warnings,
	}, nil
}

func (s *Server) claimActivity(ctx context.Context, acc *Account, planID, captchaParam, captchaRegion string) (*ClaimResult, error) {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return nil, fmt.Errorf("缺少 plan_id")
	}
	_ = s.reportActivation(ctx, acc)
	if activePlans, err := s.currentPlans(ctx, acc); err == nil {
		if status, ok := activePlans[planID]; ok {
			return &ClaimResult{Code: 1003, Message: "已领取过该套餐，请勿重复领取", Plan: &ActivityPlan{PlanID: planID, Claimed: true, CurrentStatus: status}}, nil
		}
	}
	extra := map[string]string{}
	if strings.TrimSpace(captchaParam) != "" {
		extra["X-Aliyun-Captcha-Verify-Param"] = strings.TrimSpace(captchaParam)
		if region := strings.TrimSpace(captchaRegion); region != "" {
			extra["X-Aliyun-Captcha-Verify-Region"] = region
		}
	}
	parsed, status, err := s.zcodeAPICall(ctx, acc, "POST", "/api/v1/zcode-plan/billing/claim", nil, map[string]string{"plan_id": planID}, extra)
	if err != nil {
		if parsed == nil {
			return nil, err
		}
		code, _ := activityInt(parsed, "code")
		data := activityMap(parsed["data"])
		message := activityString(data, "message")
		if message == "" {
			message = activityResponseMessage(parsed, status)
		}
		return &ClaimResult{Code: code, Message: message, UpstreamStatus: status}, nil
	}
	code, _ := activityInt(parsed, "code")
	data := activityMap(parsed["data"])
	message := activityString(data, "message")
	if message == "" {
		message = activityResponseMessage(parsed, 200)
	}
	result := &ClaimResult{
		Code:    code,
		Message: message,
	}
	if code == 0 || code == 200 {
		result.OK = true
		result.Message = ""
	}
	if plan := activityMap(data["plan"]); plan != nil {
		normalized := activityPlanFromMap(plan)
		result.Plan = &normalized
	}
	return result, nil
}

func activityMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func activitySlice(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	v, _ := m[key].([]any)
	return v
}

func activityString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

func activityInt(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

func activityBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}

func activityIntDefault(m map[string]any, key string, fallback int) int {
	if v, ok := activityInt(m, key); ok {
		return v
	}
	return fallback
}

func activityInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func collectPlanMaps(value any, out map[string]map[string]any) {
	switch v := value.(type) {
	case map[string]any:
		if id := activityString(v, "plan_id"); id != "" {
			mergePlanMap(v, out)
		}
		if plan := activityMap(v["zcode_plan"]); plan != nil {
			mergePlanMap(plan, out)
		}
		for _, child := range v {
			collectPlanMaps(child, out)
		}
	case []any:
		for _, child := range v {
			collectPlanMaps(child, out)
		}
	}
}

func mergePlanMap(src map[string]any, out map[string]map[string]any) {
	id := activityString(src, "plan_id")
	if id == "" {
		return
	}
	dst := out[id]
	if dst == nil {
		dst = map[string]any{}
		out[id] = dst
	}
	for key, value := range src {
		if value == nil {
			continue
		}
		if key == "entitlements" {
			if list, ok := value.([]any); ok && len(list) > 0 {
				dst[key] = list
			}
			continue
		}
		dst[key] = value
	}
}

func collectPlanIDs(value any) []string {
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if id := activityString(x, "plan_id"); id != "" {
				seen[id] = true
			}
			if plan := activityMap(x["zcode_plan"]); plan != nil {
				if id := activityString(plan, "plan_id"); id != "" {
					seen[id] = true
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(value)
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func activityPlanFromMap(m map[string]any) ActivityPlan {
	plan := ActivityPlan{
		PlanID:      activityString(m, "plan_id"),
		Name:        activityString(m, "name"),
		Description: activityString(m, "description"),
		Priority:    activityIntDefault(m, "priority", 0),
		StartsAt:    activityInt64(m, "starts_at"),
		EndsAt:      activityInt64(m, "ends_at"),
		EffectiveAt: activityInt64(m, "effective_at"),
	}
	if plan.Name == "" {
		plan.Name = plan.PlanID
	}
	for _, item := range activitySlice(m, "entitlements") {
		e := activityMap(item)
		if e == nil {
			continue
		}
		plan.Entitlements = append(plan.Entitlements, ActivityEntitlement{
			EntitlementID: activityString(e, "entitlement_id"),
			Model:         activityString(e, "show_name"),
			Meter:         activityString(e, "meter"),
			Unit:          activityString(e, "unit_type"),
			Amount:        activityInt64(e, "grant_units"),
			Period:        activityString(e, "period"),
			EffectiveAt:   activityInt64(e, "effective_at"),
			Priority:      activityIntDefault(e, "priority", 0),
		})
	}
	return plan
}
