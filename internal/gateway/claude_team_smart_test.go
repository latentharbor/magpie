package gateway

import (
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
)

func TestClaudeTeamSmartInferenceAlsoSpendsExpiringWeek(t *testing.T) {
	old := allowances
	t.Cleanup(func() { allowances = old })
	now := time.Now()
	windows := func(used float64, week time.Duration) provider.Allowance {
		return provider.Allowance{{Used: 10, Resets: now.Add(4 * time.Hour), Span: 5 * time.Hour}, {Used: used, Resets: now.Add(week), Span: 7 * 24 * time.Hour}}
	}
	share := map[string]provider.Allowance{"soon": windows(99, 2*time.Hour), "later": windows(10, 48*time.Hour)}
	allowances = func(string) map[string]provider.Allowance { return share }
	p := provider.Provider{ID: "claude-team-00000000-0000-4000-8000-000000000001", Account: &provider.Account{Agent: "claude"}}
	candidateFor := func(user string) candidate {
		return candidate{p: provider.Provider{Account: &provider.Account{Agent: "claude", User: user}}, model: "claude-sonnet-5", rest: user}
	}
	cs := []candidate{candidateFor("later"), candidateFor("soon")}
	check := func(want string) {
		t.Helper()
		got := route(p, cs, "claude-sonnet-5", provider.Anthropic)
		if got[0].rest != want {
			t.Fatalf("first=%s want=%s", got[0].rest, want)
		}
	}
	check("soon")
	share["soon"][0].Used = 100
	check("later")
	share["soon"][0].Used = 10
	share["soon"][1].Used = 100
	check("later")
	share["soon"][1].Resets = now.Add(-time.Hour)
	check("later") // refreshed week no longer expires first
	share["soon"] = windows(99, 2*time.Hour)
	p.ID = "ordinary-subscription"
	check("later") // don't change other providers' Smart
	p.ID = "claude-team-00000000-0000-4000-8000-000000000001"
	p.Routing = provider.Ordered
	remove := routes.Register(provider.Ordered, routes.Ordered)
	defer remove()
	check("later")
}
