package claudeteam

import (
	"errors"
	"slices"
	"time"
)

// Candidate contains observations, never credentials. The host computes the
// router's quota weight once; the same order serves inference and standby.
type Candidate struct {
	ID, Tier                      string
	Lapsed, Unavailable, Reliable bool
	SourceBlocked                 bool
	Limits                        []Limit
	Weight                        Weight
}

func Spent(ws []Limit, threshold float64, now time.Time) bool {
	for _, w := range ws {
		if w.Kind != "five_hour" && w.Kind != "seven_day" {
			continue
		}
		if w.Used*100 >= threshold && (w.ResetsAt == 0 || w.ResetsAt > now.Unix()) {
			return true
		}
	}
	return false
}

func Choose(c Config, members []Candidate, manual string, history []Event, now time.Time) (string, string, error) {
	return ChooseForModel(c, members, manual, history, now, "")
}

func ChooseForModel(c Config, members []Candidate, manual string, history []Event, now time.Time, model string) (string, string, error) {
	from, to := "", ""
	modelBlocked := model != ""
	for _, s := range members {
		if s.Tier == "team_tier_1" && slices.Contains(c.Members, s.ID) && !s.Lapsed && !SpentForModel(s.Limits, c.Threshold, now, model) {
			modelBlocked = false
		}
	}
	var candidates []Candidate
	var weights []Weight
	for _, s := range members {
		if !slices.Contains(c.Members, s.ID) {
			continue
		}
		if s.Tier == "team_tier_1" && (manual != "" && manual == s.ID || manual == "" && !s.SourceBlocked && (s.Lapsed || Spent(s.Limits, c.Threshold, now) || modelBlocked && SpentForModel(s.Limits, c.Threshold, now, model))) && from == "" {
			from = s.ID
		}
		_, cooling := CoolingTarget(history, s.ID, now)
		if s.Tier != "unassigned" || s.Unavailable || s.Lapsed || cooling || SpentForModel(s.Limits, c.Threshold, now, model) {
			continue
		}
		candidates = append(candidates, s)
		w := s.Weight
		w.Reliable = s.Reliable
		weights = append(weights, w)
	}
	var standby Candidate
	if len(candidates) > 0 {
		at := 0
		if c.Routing == "" {
			order := Order(weights)
			at = order[0]
		} else {
			at = routeOrder(c.Routing, "claude-team-"+c.OrganizationID+"/standby", weights)[0]
		}
		standby = candidates[at]
		to = standby.ID
	}
	// Expiry decides which standby takes an actually needed slot. Reordering
	// healthy Premium inference must not consume a cloud seat movement.
	if manual != "" && from == "" {
		return "", "", errors.New("the chosen source has no Premium seat in this pool")
	}
	if from != "" && to == "" {
		return from, "", errors.New("all Claude Team alternatives are cooling down, signed out or unknown; see the earliest reset")
	}
	return from, to, nil
}
