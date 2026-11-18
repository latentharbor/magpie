package claudeteam

import (
	"math"
	"slices"
	"strings"
	"time"
)

func ModelWindow(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	model = strings.ToLower(model)
	for _, name := range []string{"fable", "opus", "sonnet"} {
		if strings.HasPrefix(model, "claude-"+name+"-") || model == name {
			if name == "fable" {
				return "seven_day_overage_included"
			}
			return "seven_day_" + name
		}
	}
	return ""
}
func SpentForModel(ws []Limit, threshold float64, now time.Time, model string) bool {
	if Spent(ws, threshold, now) {
		return true
	}
	kind := ModelWindow(model)
	if kind == "" {
		return false
	}
	return slices.ContainsFunc(ws, func(w Limit) bool {
		return w.Kind == kind && w.Used*100 >= threshold && (w.ResetsAt == 0 || w.ResetsAt > now.Unix())
	})
}
func QuotaKnownForModel(ws []Limit, model string) bool {
	if !QuotaKnown(ws) {
		return false
	}
	// Fable is always a separate allowance on an eligible subscription. Opus
	// and Sonnet caps are optional; their absence is not an unknown Fable cap.
	return ModelWindow(model) != "seven_day_overage_included" || slices.ContainsFunc(ws, func(w Limit) bool {
		return w.Kind == "seven_day_overage_included" && !math.IsNaN(w.Used) && !math.IsInf(w.Used, 0) && w.Used >= 0 && w.Used <= 1 && (w.Used == 0 || w.ResetsAt > 0)
	})
}
