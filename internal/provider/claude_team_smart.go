package provider

import (
	"github.com/yetone/magpie/internal/claudeteam"

	"time"
)

func ClaudeTeamSmart(seats []claudeteam.Weight) []int  { return claudeteam.Order(seats) }
func claudeTeamQuotaKnown(ls []ClaudeLimit) bool { return claudeteam.QuotaKnown(ls) }
func claudeTeamExpiryWorthwhile(active, standby claudeteam.Weight, now time.Time) bool {
	return claudeteam.ExpiryWorthwhile(active, standby, now)
}
