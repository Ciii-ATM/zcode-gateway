package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	fhttp "github.com/bogdanfinn/fhttp"
	"io"
	"strings"
	"time"
)

type HTTPDoer interface {
	Do(*fhttp.Request) (*fhttp.Response, error)
}

type OAuthTokenResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Token string `json:"token"`
		Zai   *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"zai"`
		Bigmodel *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"bigmodel"`
		User struct {
			UserID string `json:"user_id"`
			Name   string `json:"name"`
			Email  string `json:"email"`
			Avatar string `json:"avatar"`
		} `json:"user"`
		ExpiresIn int `json:"expires_in"`
	} `json:"data"`
}

type CustomerInfo struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		CustomerNumber string `json:"customerNumber"`
		UserType       string `json:"userType"`
		Organizations  []struct {
			OrganizationID   string `json:"organizationId"`
			OrganizationName string `json:"organizationName"`
			Projects         []struct {
				ProjectID   string `json:"projectId"`
				ProjectName string `json:"projectName"`
				ProjectType any    `json:"projectType"`
				Status      string `json:"status"`
			} `json:"projects"`
		} `json:"organizations"`
	} `json:"data"`
}

type PlanEntitlement struct {
	EntitlementID string   `json:"entitlement_id"`
	ShowName      string   `json:"show_name"`
	Capabilities  []string `json:"capabilities"`
	GrantUnits    int64    `json:"grant_units"`
	Period        string   `json:"period"`
	EffectiveAt   int64    `json:"effective_at"`
	Priority      int      `json:"priority"`
}

type PlanInfo struct {
	UserPlanID   string            `json:"user_plan_id"`
	PlanID       string            `json:"plan_id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	Priority     int               `json:"priority"`
	StartsAt     int64             `json:"starts_at"`
	EndsAt       int64             `json:"ends_at"`
	Entitlements []PlanEntitlement `json:"entitlements"`
}

type PlanResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		ServerTime int64      `json:"server_time"`
		Plans      []PlanInfo `json:"plans"`
		Balances   []Balance  `json:"balances"`
	} `json:"data"`
}

type Balance struct {
	EntitlementID  string `json:"entitlement_id"`
	ShowName       string `json:"show_name"`
	TotalUnits     int64  `json:"total_units"`
	UsedUnits      int64  `json:"used_units"`
	RemainingUnits int64  `json:"remaining_units"`
	AvailableUnits int64  `json:"available_units"`
	ReservedUnits  *int64 `json:"reserved_units"`
	UserPlanID     string `json:"user_plan_id"`
	PlanID         string `json:"plan_id"`
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sourceHeaders returns the identity headers for calls that happen before an
// account exists (OAuth start/poll/callback). The profile is generated per
// flow; no value is read from the host machine.
func sourceHeaders(profile DeviceProfile) map[string]string {
	return identityHeaders(profile)
}

func doRequest(ctx context.Context, client HTTPDoer, method, url string, headers map[string]string, body []byte) (*fhttp.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	}
	return client.Do(req)
}

func readJSON(resp *fhttp.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func exchangeBusinessToken(ctx context.Context, client HTTPDoer, raw string, profile DeviceProfile) (string, error) {
	body, _ := json.Marshal(map[string]string{"token": raw})
	headers := sourceHeaders(profile)
	headers["Content-Type"] = "application/json"
	resp, err := doRequest(ctx, client, "POST", "https://api.z.ai/api/auth/z/login", headers, body)
	if err != nil {
		return "", err
	}
	data, err := readJSON(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("business token exchange HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Code int `json:"code"`
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &parsed) != nil || parsed.Data.AccessToken == "" {
		return "", errors.New("business token exchange returned no access_token")
	}
	return parsed.Data.AccessToken, nil
}

func fetchCustomerInfo(ctx context.Context, client HTTPDoer, token, provider string, profile DeviceProfile) (*CustomerInfo, error) {
	origin := "https://api.z.ai"
	if provider == "bigmodel" {
		origin = "https://bigmodel.cn"
	}
	headers := sourceHeaders(profile)
	headers["Authorization"] = token
	headers["Content-Type"] = "application/json"
	resp, err := doRequest(ctx, client, "GET", origin+"/api/biz/customer/getCustomerInfo", headers, nil)
	if err != nil {
		return nil, err
	}
	data, err := readJSON(resp)
	if err != nil {
		return nil, err
	}
	var parsed CustomerInfo
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if parsed.Code != 0 && parsed.Code != 200 {
		return nil, fmt.Errorf("customer info code=%d", parsed.Code)
	}
	return &parsed, nil
}

func fetchCurrentPlan(ctx context.Context, client HTTPDoer, zcodeJWT string, headers map[string]string, profile DeviceProfile) (*PlanResponse, error) {
	if headers == nil {
		headers = sourceHeaders(profile)
	}
	headers["Authorization"] = "Bearer " + zcodeJWT
	headers["Accept"] = "application/json"
	url := fmt.Sprintf("https://zcode.z.ai/api/v1/zcode-plan/billing/current?app_version=%s", appVersion)
	resp, err := doRequest(ctx, client, "GET", url, headers, nil)
	if err != nil {
		return nil, err
	}
	data, err := readJSON(resp)
	if err != nil {
		return nil, err
	}
	var parsed PlanResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if parsed.Code != 0 && parsed.Code != 200 {
		return nil, fmt.Errorf("billing/current code=%d %s", parsed.Code, parsed.Msg)
	}
	return &parsed, nil
}

func defaultProject(customer *CustomerInfo, provider string) *ProjectLocation {
	if customer == nil || len(customer.Data.Organizations) == 0 {
		return nil
	}
	orgIdx := 0
	for i, org := range customer.Data.Organizations {
		if strings.Contains(org.OrganizationName, "默认机构") {
			orgIdx = i
			break
		}
	}
	org := customer.Data.Organizations[orgIdx]
	if len(org.Projects) == 0 {
		return nil
	}
	project := org.Projects[0]
	for _, p := range org.Projects {
		if strings.Contains(p.ProjectName, "默认项目") {
			project = p
			break
		}
	}
	origin := "https://api.z.ai"
	family := "zai"
	if provider == "bigmodel" {
		origin = "https://bigmodel.cn"
		family = "bigmodel"
	}
	return &ProjectLocation{
		OrganizationID: org.OrganizationID,
		ProjectID:      project.ProjectID,
		Origin:         origin,
		Family:         family,
	}
}

func findOrCreateProjectKey(ctx context.Context, client HTTPDoer, token, provider string, project *ProjectLocation, profile DeviceProfile) (string, error) {
	origin := project.Origin
	if origin == "" {
		origin = "https://api.z.ai"
	}
	base := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys",
		strings.TrimRight(origin, "/"), project.OrganizationID, project.ProjectID)
	headers := sourceHeaders(profile)
	if provider == "zai" {
		headers["Authorization"] = "Bearer " + token
	} else {
		headers["Authorization"] = token
	}
	headers["Content-Type"] = "application/json"
	resp, err := doRequest(ctx, client, "GET", base, headers, nil)
	if err != nil {
		return "", err
	}
	data, err := readJSON(resp)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Code int `json:"code"`
		Data []struct {
			APIKey  string `json:"apiKey"`
			Name    string `json:"name"`
			KeyType int    `json:"keyType"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &parsed) != nil {
		return "", errors.New("api_keys response is invalid")
	}
	for _, key := range parsed.Data {
		if key.Name == "zcode-api-key" {
			return key.APIKey, nil
		}
	}
	body, _ := json.Marshal(map[string]any{"name": "zcode-api-key", "usageScene": 1})
	resp, err = doRequest(ctx, client, "POST", base, headers, body)
	if err != nil {
		return "", err
	}
	data, err = readJSON(resp)
	if err != nil {
		return "", err
	}
	var created struct {
		APIKey  string `json:"apiKey"`
		Name    string `json:"name"`
		KeyType int    `json:"keyType"`
	}
	if json.Unmarshal(data, &created) != nil || created.APIKey == "" {
		return "", errors.New("api key creation returned no apiKey")
	}
	return created.APIKey, nil
}

// mintProjectToken asks for a project access token using the account's own
// device profile, so the login identity and the message identity match.
func (s *Server) mintProjectToken(ctx context.Context, acc *Account) (string, int64, error) {
	if acc.Project == nil {
		return "", 0, errors.New("account has no project key location")
	}
	origin := strings.TrimRight(acc.Project.Origin, "/")
	if origin == "" {
		origin = "https://api.z.ai"
	}
	url := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys/%s/access_tokens",
		origin, acc.Project.OrganizationID, acc.Project.ProjectID, acc.Project.APIKeyID)
	headers := s.profileHeaders(acc)
	headers["Authorization"] = "Bearer " + acc.AccessToken
	headers["Content-Type"] = "application/json"
	body, _ := json.Marshal(map[string]string{"clientType": "zcode", "clientVersion": appVersion})
	resp, err := doRequest(ctx, s.upstream, "POST", url, headers, body)
	if err != nil {
		return "", 0, err
	}
	data, err := readJSON(resp)
	if err != nil {
		return "", 0, err
	}
	var parsed struct {
		Code int `json:"code"`
		Data struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
			ExpiresIn   int64  `json:"expiresIn"`
			Enabled     *bool  `json:"enabled"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &parsed) != nil || parsed.Data.AccessToken == "" {
		return "", 0, fmt.Errorf("access_tokens response is invalid (HTTP %d)", resp.StatusCode)
	}
	if parsed.Data.Enabled != nil && !*parsed.Data.Enabled {
		return "", 0, errors.New("project token is disabled")
	}
	expires := parsed.Data.ExpiresAt
	if expires == 0 && parsed.Data.ExpiresIn > 0 {
		expires = time.Now().Unix() + parsed.Data.ExpiresIn
	}
	return parsed.Data.AccessToken, expires, nil
}
