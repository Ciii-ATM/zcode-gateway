package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) quotaLoop(ctx context.Context) {
	for {
		s.refreshAllQuotas(ctx)
		interval := time.Duration(s.store.Settings().QuotaIntervalSec) * time.Second
		if interval < 10*time.Second {
			interval = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (s *Server) refreshAllQuotas(ctx context.Context) {
	if s.store.db != nil {
		s.store.db.PruneBindings(time.Now().Unix())
	}
	accounts := s.store.AccountsForRefresh()
	sem := make(chan struct{}, 2)
	done := make(chan struct{}, len(accounts))
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		acc := acc
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; done <- struct{}{} }()
			_ = s.refreshQuota(ctx, acc)
		}()
	}
	for range accounts {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) refreshQuota(ctx context.Context, acc *Account) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	plan, err := fetchCurrentPlan(ctx, s.upstream, acc.ZcodeJWT, s.profileHeaders(acc))
	if err != nil {
		s.store.UpdateAccount(acc.ID, func(a *Account) {
			a.Quota.Error = err.Error()
			a.Quota.UpdatedAt = time.Now().Unix()
		})
		return err
	}
	quota := Quota{Source: "local", UpdatedAt: time.Now().Unix()}
	var activePlans []*PlanInfo
	var currentPlan *PlanInfo
	for i := range plan.Data.Plans {
		if !strings.EqualFold(plan.Data.Plans[i].Status, "active") {
			continue
		}
		activePlans = append(activePlans, &plan.Data.Plans[i])
		if currentPlan == nil || plan.Data.Plans[i].Priority > currentPlan.Priority {
			currentPlan = &plan.Data.Plans[i]
		}
	}
	if currentPlan != nil {
		quota.PlanName = currentPlan.Name
		quota.PlanID = currentPlan.PlanID
		quota.Status = currentPlan.Status
		quota.EndsAt = currentPlan.EndsAt
	}
	usageByModel := map[string]UsageTotals{}
	for name, totals := range acc.Models {
		if totals != nil {
			usageByModel[name] = *totals
		}
	}
	balances, balanceErr := s.tryBalance(ctx, acc, currentPlan)
	byEntitlement := map[string]Balance{}
	byPlanName := map[string]Balance{}
	for _, b := range balances {
		if b.EntitlementID != "" {
			byEntitlement[b.EntitlementID] = b
		}
		if b.UserPlanID != "" {
			byPlanName[b.UserPlanID+"|"+strings.ToLower(b.ShowName)] = b
		}
	}
	if balanceErr == nil && len(balances) > 0 {
		quota.Source = "api"
	}
	aggregate := map[string]*QuotaModel{}
	modelOrder := make([]string, 0, 4)
	for _, activePlan := range activePlans {
		qp := QuotaPlan{
			UserPlanID: activePlan.UserPlanID,
			PlanID:     activePlan.PlanID,
			Name:       activePlan.Name,
			Status:     activePlan.Status,
			Priority:   activePlan.Priority,
			StartsAt:   activePlan.StartsAt,
			EndsAt:     activePlan.EndsAt,
			Models:     []QuotaModel{},
		}
		for _, ent := range activePlan.Entitlements {
			b, ok := byEntitlement[ent.EntitlementID]
			if !ok {
				b, ok = byPlanName[activePlan.UserPlanID+"|"+strings.ToLower(ent.ShowName)]
			}
			used := int64(0)
			if ok {
				used = b.UsedUnits
				if used == 0 && b.TotalUnits > 0 {
					used = b.TotalUnits - b.RemainingUnits
				}
			} else if totals, ok := usageByModel[ent.ShowName]; ok {
				used = int64(totals.Total())
			}
			granted := ent.GrantUnits
			if ok && b.TotalUnits > 0 {
				granted = b.TotalUnits
			}
			remaining := granted - used
			if remaining < 0 {
				remaining = 0
			}
			percent := 0.0
			if granted > 0 {
				percent = float64(remaining) / float64(granted) * 100
			}
			model := QuotaModel{
				Name: ent.ShowName, Granted: granted, Used: used, Remaining: remaining, Percent: percent, Source: quota.Source,
			}
			qp.Models = append(qp.Models, model)
			qp.Granted += granted
			qp.Used += used
			qp.Remaining += remaining
			key := strings.ToLower(ent.ShowName)
			if agg, exists := aggregate[key]; exists {
				agg.Granted += granted
				agg.Used += used
				agg.Remaining += remaining
				if agg.Granted > 0 {
					agg.Percent = float64(agg.Remaining) / float64(agg.Granted) * 100
				}
			} else {
				copy := model
				aggregate[key] = &copy
				modelOrder = append(modelOrder, key)
			}
		}
		if qp.Granted > 0 {
			qp.Percent = float64(qp.Remaining) / float64(qp.Granted) * 100
		}
		quota.Plans = append(quota.Plans, qp)
		quota.Granted += qp.Granted
		quota.Used += qp.Used
		quota.Remaining += qp.Remaining
	}
	for _, key := range modelOrder {
		quota.Models = append(quota.Models, *aggregate[key])
	}
	if quota.Granted > 0 {
		quota.Percent = float64(quota.Remaining) / float64(quota.Granted) * 100
	}
	if balanceErr != nil && quota.Source == "local" {
		quota.Error = "" // local accounting is still valid; do not show an API error
	}
	s.store.UpdateAccount(acc.ID, func(a *Account) {
		a.Quota = quota
		if currentPlan != nil {
			if strings.EqualFold(currentPlan.Status, "active") {
				if strings.Contains(currentPlan.PlanID, "start-plan") {
					a.Mode = "start-plan"
				} else {
					a.Mode = "coding-plan"
				}
			} else {
				a.ExhaustedUntil = time.Now().Add(10 * time.Minute).Unix()
				a.LastError = "套餐已过期"
			}
		}
		if quota.Remaining <= 0 && quota.Granted > 0 {
			a.ExhaustedUntil = time.Now().Add(10 * time.Minute).Unix()
			a.LastError = "额度耗尽"
		} else {
			if a.ExhaustedUntil < time.Now().Unix() {
				a.ExhaustedUntil = 0
			}
			if a.LastError == "额度耗尽" {
				a.LastError = ""
				a.LastErrorAt = 0
			}
		}
	})
	return nil
}

func (s *Server) tryBalance(ctx context.Context, acc *Account, plan *PlanInfo) ([]Balance, error) {
	base := "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"
	headers := s.profileHeaders(acc)
	headers["Authorization"] = "Bearer " + acc.ZcodeJWT
	headers["Accept"] = "application/json"
	headers["X-Request-Id"] = randomUUID()
	headers["X-ZCode-Session-Type"] = "main"
	headers["X-ZCode-Trace-Id"] = randomUUID()
	headers["X-Query-Id"] = "01" + randomHex(11)
	headers["X-ZCode-Query-Source"] = "main_turn"
	headers["X-Session-Id"] = "session_" + randomHex(16)
	query := "?app_version=" + appVersion
	if plan != nil && plan.UserPlanID != "" {
		query += "&user_plan_id=" + plan.UserPlanID
	}
	resp, err := doRequest(ctx, s.upstream, http.MethodGet, base+query, headers, nil)
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
			Balances []Balance `json:"balances"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if parsed.Code != 0 || resp.StatusCode != 200 {
		return nil, fmt.Errorf("balance code=%d HTTP %d", parsed.Code, resp.StatusCode)
	}
	return parsed.Data.Balances, nil
}
