package gateway

import (
	"net/http"

	"github.com/yetone/magpie/internal/provider"
)

// The existing session allocator reserves new conversations atomically and
// keeps their warm account as the pool grows or shrinks. A user-set affinity
// or routing strategy, a group and an anonymous request keep their own policy.
func teamSessionMode(p provider.Provider, in http.Header) string {
	if p.Affinity == "" && p.Routing == "" && provider.ClaudeTeamOf(p) != "" && sessionNamed(in) {
		return provider.AffinitySession
	}
	return p.Affinity
}

func teamPoolAlike(a, b candidate) bool {
	org := provider.ClaudeTeamOf(a.p)
	return org != "" && a.p.ID == b.p.ID && provider.ClaudeTeamOf(b.p) == org && a.model == b.model && a.effort == b.effort && a.p.Account != nil && b.p.Account != nil && !provider.ClaudeTeamModelExhausted(a.p.Account.User, a.model) && !provider.ClaudeTeamModelExhausted(b.p.Account.User, b.model)
}

func teamOpeningAccount(key string, c candidate) {
	sticks.Lock()
	defer sticks.Unlock()
	if st, ok := sticks.opening[key]; ok {
		st.rest, st.who, st.model, st.effort = c.rest, c.who(), c.model, c.effort
		sticks.opening[key] = st
	}
}
