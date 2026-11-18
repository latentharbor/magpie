package provider

import (
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/claudeteam"
)

// ObserveClaudeTeamModel records real demand without an administrator call.
// A scoped window alone never revokes a seat still serving other models.
func ObserveClaudeTeamModel(user, model string) bool {
	t := claudeTeamForUser(user)
	t.mu.Lock()
	id := t.users[strings.ToLower(user)]
	now := time.Now()
	kind := claudeteam.ModelWindow(model)
	if !t.cfg.Enabled || !slices.Contains(t.cfg.Members, id) || kind == "" {
		t.mu.Unlock()
		return false
	}
	spent := claudeteam.SpentForModel(t.disk.Limits[id], t.cfg.Threshold, now, model)
	// A provider alias can name a parked member. With no initial candidate,
	// wake on the verified Premium pool rather than that alias's own window.
	hasPremium, blocked := false, true
	for _, account := range t.cfg.Members {
		if t.seats[account] != "team_tier_1" {
			continue
		}
		hasPremium = true
		if !claudeteam.SpentForModel(t.disk.Limits[account], t.cfg.Threshold, now, model) {
			blocked = false
		}
	}
	blocked = hasPremium && blocked
	if kind == "seven_day_overage_included" || spent || claudeteam.ModelWindow(t.modelDemand) == kind {
		t.modelDemand, t.modelDemandUntil = model, now.Add(15*time.Minute)
	}
	// A demand arriving after this pass chose its source belongs to the next
	// pass. Do not lose it just because reconciliation is already running.
	wake := (spent || blocked) && !t.requested
	if wake {
		t.requested = true
		t.signalLocked()
	}
	t.mu.Unlock()
	if wake {
		t.notify()
	}
	return spent
}

func ClaudeTeamModelExhausted(user, model string) bool {
	t := claudeTeamForUser(user)
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.users[strings.ToLower(user)]
	return t.cfg.Enabled && slices.Contains(t.cfg.Members, id) && claudeteam.SpentForModel(t.disk.Limits[id], t.cfg.Threshold, time.Now(), model)
}
func (t *claudeTeamController) cooldownLocked(id string, now time.Time) (ClaudeTeamEvent, bool) {
	if e, ok := t.disk.Cooldowns[id]; ok {
		return e, now.Before(e.RetryAt)
	}
	return claudeteam.CoolingTarget(t.disk.History, id, now)
}
func ClaudeTeamUserModelReset(user, model string) (time.Time, bool) {
	return claudeTeamForUser(user).resetModel(model)
}
func (t *claudeTeamController) resetModel(model string) (time.Time, bool) {
	v := t.status()
	if !v.Config.Enabled {
		return time.Time{}, false
	}
	var earliest time.Time
	for _, m := range v.Members {
		if !m.Selected || m.Lapsed || m.SeatTier != "unassigned" && m.SeatTier != "team_tier_1" {
			continue
		}
		var back time.Time
		for _, w := range m.Limits {
			if w.Kind != "five_hour" && w.Kind != "seven_day" && w.Kind != claudeteam.ModelWindow(model) {
				continue
			}
			if w.Used*100 < v.Config.Threshold || w.ResetsAt > 0 && w.ResetsAt <= time.Now().Unix() {
				continue
			}
			if w.ResetsAt <= 0 {
				return time.Time{}, false
			}
			if at := time.Unix(w.ResetsAt, 0); at.After(back) {
				back = at
			}
		}
		if back.IsZero() {
			return time.Time{}, false
		}
		if earliest.IsZero() || back.Before(earliest) {
			earliest = back
		}
	}
	return earliest, !earliest.IsZero()
}
