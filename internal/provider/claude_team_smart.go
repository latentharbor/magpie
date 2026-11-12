package provider
import ("time"; "github.com/yetone/magpie/internal/claudeteam"; "github.com/yetone/magpie/internal/routes")
func ClaudeTeamSmart(seats []routes.Seat)[]int { return claudeteam.Order(seats) }
func claudeTeamQuotaKnown(ls []ClaudeLimit)bool { return claudeteam.QuotaKnown(ls) }
func claudeTeamExpiryWorthwhile(active,standby routes.Seat,now time.Time)bool { return claudeteam.ExpiryWorthwhile(active,standby,now) }
