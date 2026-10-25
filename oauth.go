package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type OAuthFlow struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	PollToken    string    `json:"-"`
	AuthorizeURL string    `json:"authorizeUrl"`
	State        string    `json:"state"`
	RedirectURI  string    `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
	AccountID    string    `json:"accountId,omitempty"`
	// Profile is the device identity used for every call of this flow; a new
	// account inherits it so login and message traffic share one device.
	Profile DeviceProfile `json:"-"`
}

type OAuthManager struct {
	mu     sync.Mutex
	flows  map[string]*OAuthFlow
	client HTTPDoer
	store  *Store
}

func NewOAuthManager(client HTTPDoer, store *Store) *OAuthManager {
	return &OAuthManager{flows: map[string]*OAuthFlow{}, client: client, store: store}
}

func (m *OAuthManager) Start(ctx context.Context, provider string) (*OAuthFlow, error) {
	if provider != "zai" && provider != "bigmodel" {
		provider = "zai"
	}
	pollToken := randomHex(32)
	profile := generateProfile()
	body, _ := json.Marshal(map[string]string{"provider": provider})
	headers := sourceHeaders(profile)
	headers["Authorization"] = "Bearer " + pollToken
	headers["Content-Type"] = "application/json"
	resp, err := doRequest(ctx, m.client, http.MethodPost, "https://zcode.z.ai/api/v1/oauth/cli/init", headers, body)
	if err != nil {
		return nil, err
	}
	data, err := readJSON(resp)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FlowID          string `json:"flow_id"`
			AuthorizeURL    string `json:"authorize_url"`
			ExpiresAt       int64  `json:"expires_at"`
			PollIntervalSec int    `json:"poll_interval_sec"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Code != 0 || parsed.Data.FlowID == "" || parsed.Data.AuthorizeURL == "" {
		return nil, fmt.Errorf("oauth init failed: HTTP %d code=%d msg=%s", resp.StatusCode, parsed.Code, parsed.Msg)
	}
	u, _ := url.Parse(parsed.Data.AuthorizeURL)
	state := ""
	if u != nil {
		state = u.Query().Get("state")
	}
	flow := &OAuthFlow{
		ID:           parsed.Data.FlowID,
		Provider:     provider,
		PollToken:    pollToken,
		AuthorizeURL: parsed.Data.AuthorizeURL,
		State:        state,
		RedirectURI:  "zcode://oauth/callback",
		CreatedAt:    time.Now(),
		Profile:      profile,
	}
	m.mu.Lock()
	m.flows[flow.ID] = flow
	m.mu.Unlock()
	return flow, nil
}

// profile returns the flow's device profile, generating one for flows created
// before the field existed.
func (f *OAuthFlow) profile() DeviceProfile {
	if f.Profile.DeviceMid == "" {
		f.Profile = generateProfile()
	}
	return f.Profile
}

func (m *OAuthManager) Get(flowID string) *OAuthFlow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flows[flowID]
}

func (m *OAuthManager) findByState(state string) *OAuthFlow {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, flow := range m.flows {
		if flow.State == state {
			return flow
		}
	}
	return nil
}

func (m *OAuthManager) Status(ctx context.Context, flowID string) (string, *Account, error) {
	flow := m.Get(flowID)
	if flow == nil {
		return "error", nil, errors.New("oauth flow not found")
	}
	url := "https://zcode.z.ai/api/v1/oauth/cli/poll/" + url.PathEscape(flow.ID)
	headers := sourceHeaders(flow.profile())
	headers["Authorization"] = "Bearer " + flow.PollToken
	resp, err := doRequest(ctx, m.client, http.MethodGet, url, headers, nil)
	if err != nil {
		return "pending", nil, err
	}
	data, err := readJSON(resp)
	if err != nil {
		return "pending", nil, err
	}
	var parsed struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Code != 0 {
		return "pending", nil, nil
	}
	var status struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(parsed.Data, &status)
	switch status.Status {
	case "pending":
		return "pending", nil, nil
	case "failed":
		return "error", nil, errors.New("OAuth authorization failed")
	case "ready":
		var tokenResp OAuthTokenResponse
		if err := json.Unmarshal(data, &tokenResp); err != nil {
			return "error", nil, err
		}
		acc, err := m.saveAccount(ctx, flow, &tokenResp)
		if err != nil {
			return "error", nil, err
		}
		return "ready", acc, nil
	default:
		return "pending", nil, nil
	}
}

func (m *OAuthManager) HandleCallback(ctx context.Context, rawURL string) (*Account, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, err
	}
	q := u.Query()
	code := q.Get("code")
	state := q.Get("state")
	if code == "" {
		return nil, errors.New("callback URL has no code")
	}
	flow := m.findByState(state)
	if flow == nil {
		// Allow the desktop deep-link callback without a stored flow.
		flow = &OAuthFlow{Provider: "zai", RedirectURI: "zcode://oauth/callback", State: state, Profile: generateProfile()}
	}
	// The CLI flow usually completes through the poll endpoint; try that first.
	if flow.ID != "" {
		if status, acc, err := m.Status(ctx, flow.ID); err == nil && status == "ready" && acc != nil {
			return acc, nil
		}
	}
	body, _ := json.Marshal(map[string]string{
		"provider":     flow.Provider,
		"code":         code,
		"redirect_uri": flow.RedirectURI,
		"state":        state,
	})
	headers := sourceHeaders(flow.profile())
	headers["Content-Type"] = "application/json"
	resp, err := doRequest(ctx, m.client, http.MethodPost, "https://zcode.z.ai/api/v1/oauth/token", headers, body)
	if err != nil {
		return nil, err
	}
	data, err := readJSON(resp)
	if err != nil {
		return nil, err
	}
	var tokenResp OAuthTokenResponse
	if err := json.Unmarshal(data, &tokenResp); err != nil {
		return nil, err
	}
	if tokenResp.Code != 0 {
		return nil, fmt.Errorf("token exchange failed: code=%d %s", tokenResp.Code, tokenResp.Msg)
	}
	return m.saveAccount(ctx, flow, &tokenResp)
}

func (m *OAuthManager) saveAccount(ctx context.Context, flow *OAuthFlow, tokenResp *OAuthTokenResponse) (*Account, error) {
	provider := flow.Provider
	if provider == "" {
		provider = "zai"
	}
	zcodeJWT := strings.TrimSpace(tokenResp.Data.Token)
	if zcodeJWT == "" {
		return nil, errors.New("token response has no data.token")
	}
	var rawAccess string
	if provider == "bigmodel" {
		if tokenResp.Data.Bigmodel != nil {
			rawAccess = tokenResp.Data.Bigmodel.AccessToken
		}
	} else if tokenResp.Data.Zai != nil {
		rawAccess = tokenResp.Data.Zai.AccessToken
	}
	if rawAccess == "" {
		return nil, errors.New("token response has no provider access_token")
	}
	accessToken := rawAccess
	if provider == "zai" {
		business, err := exchangeBusinessToken(ctx, m.client, rawAccess, flow.profile())
		if err != nil {
			return nil, err
		}
		accessToken = business
	}
	customer, err := fetchCustomerInfo(ctx, m.client, accessToken, provider, flow.profile())
	var project *ProjectLocation
	if err == nil {
		project = defaultProject(customer, provider)
	}
	plan, planErr := fetchCurrentPlan(ctx, m.client, zcodeJWT, sourceHeaders(flow.profile()), flow.profile())
	mode := "start-plan"
	planName := ""
	planID := ""
	planStatus := ""
	var endsAt int64
	if planErr == nil {
		for _, p := range plan.Data.Plans {
			if strings.EqualFold(p.Status, "active") {
				planName = p.Name
				planID = p.PlanID
				planStatus = p.Status
				endsAt = p.EndsAt
				if !strings.Contains(p.PlanID, "start-plan") {
					mode = "coding-plan"
				}
				break
			}
		}
	}
	if mode == "coding-plan" && project != nil {
		if keyID, err := findOrCreateProjectKey(ctx, m.client, accessToken, provider, project, flow.profile()); err == nil {
			project.APIKeyID = keyID
		}
	}
	user := UserInfo{
		ID:     tokenResp.Data.User.UserID,
		Email:  tokenResp.Data.User.Email,
		Name:   tokenResp.Data.User.Name,
		Avatar: tokenResp.Data.User.Avatar,
	}
	if user.ID == "" {
		user.ID = "oauth-" + randomID()
	}
	label := user.Name
	if label == "" {
		label = user.Email
	}
	if label == "" {
		label = "账号 " + user.ID[:min(6, len(user.ID))]
	}
	acc := &Account{
		Label:       label,
		Profile:     &flow.Profile,
		Provider:    provider,
		Mode:        mode,
		User:        user,
		Project:     project,
		Priority:    1,
		Enabled:     true,
		CreatedAt:   time.Now().Unix(),
		ZcodeJWT:    zcodeJWT,
		AccessToken: accessToken,
		Models:      map[string]*UsageTotals{},
		Quota: Quota{
			PlanName: planName,
			PlanID:   planID,
			Status:   planStatus,
			EndsAt:   endsAt,
			Source:   "pending",
		},
	}
	if existing := m.store.FindByUser(provider, user.ID); existing != nil {
		acc.ID = existing.ID
		acc.Priority = existing.Priority
		acc.Enabled = existing.Enabled
		acc.CreatedAt = existing.CreatedAt
		acc.Usage = existing.Usage
		acc.Models = existing.Models
		acc.Quota = existing.Quota
		m.store.UpdateAccount(existing.ID, func(a *Account) {
			a.Mode = mode
			a.User = user
			a.Project = project
			a.ZcodeJWT = zcodeJWT
			a.AccessToken = accessToken
			a.LastError = ""
			a.ExhaustedUntil = 0
		})
		return m.store.GetAccount(existing.ID), nil
	}
	if err := m.store.AddAccount(acc); err != nil {
		return nil, err
	}
	return m.store.GetAccount(acc.ID), nil
}
