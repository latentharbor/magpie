package provider

import (
	"math"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/routes"
)

// ClaudeTeamSmart spends the usable allowance that expires first, even if
// it has little left. Both inference and standby selection use this order;
// the ordinary Smart 90/98% bands otherwise leave that allowance unused.
// Stable hourly ties avoid moving seats for insignificant timestamp drift.
func ClaudeTeamSmart(seats []routes.Seat) []int {
	order := make([]int, len(seats))
	for i := range seats {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int {
		a, b := seats[i], seats[j]
		rank := func(s routes.Seat) int {
			if !s.Known {
				return 1
			}
			if s.Used >= 100 {
				return 2
			}
			return 0
		}
		if x, y := rank(a), rank(b); x != y {
			return x - y
		}
		if !a.Known || a.Used >= 100 {
			return 0
		}
		for k := 0; k < len(a.Soon) || k < len(b.Soon); k++ {
			var x, y time.Time
			if k < len(a.Soon) {
				x = a.Soon[k].Truncate(time.Hour)
			}
			if k < len(b.Soon) {
				y = b.Soon[k].Truncate(time.Hour)
			}
			if x.Equal(y) {
				continue
			}
			if x.IsZero() {
				return 1
			}
			if y.IsZero() {
				return -1
			}
			if x.Before(y) {
				return -1
			}
			return 1
		}
		return 0
	})
	return order
}

// Partial observations cannot establish the next weekly expiry. Try them
// after complete usable snapshots, rather than taking a seat just to learn.
// A never-started zero-use window legitimately has no reset timestamp.
func claudeTeamQuotaKnown(ls []ClaudeLimit) bool {
	for _, kind := range []string{"five_hour", "seven_day"} {
		i := slices.IndexFunc(ls, func(l ClaudeLimit) bool { return l.Kind == kind })
		if i < 0 {
			return false
		}
		l := ls[i]
		if math.IsNaN(l.Used) || math.IsInf(l.Used, 0) || l.Used < 0 || l.Used > 1 || l.Used > 0 && l.ResetsAt <= 0 {
			return false
		}
	}
	return true
}
