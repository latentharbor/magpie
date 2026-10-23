package gateway

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func awaitClaudeTeam(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	provider.AwaitClaudeTeamRotation(ctx)
}

func teamResetError(w http.ResponseWriter, from provider.Protocol, at time.Time) string {
	message := fmt.Sprintf("Claude Team usage limit reached: all available pool accounts are exhausted; earliest reset %s", at.UTC().Format(time.RFC3339))
	w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64(time.Until(at).Seconds())+1), 10))
	writeError(w, from, http.StatusTooManyRequests, message)
	return message
}

// teamSuccessors refreshes only a failed Claude account's peers after the
// background handoff. The calling key's account/model scope still applies;
// a pin or strict affinity never enters this path. Visible replies never do.
func (s *Server) teamSuccessors(r *http.Request, from provider.Protocol, c candidate, tried []candidate) []candidate {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	provider.AwaitClaudeTeamUser(ctx, c.p.Account.User)
	cancel()
	if r.Context().Err() != nil {
		return nil
	}
	p, model, ok := provider.Resolve(c.p.ID + "/" + c.model)
	if !ok {
		return nil
	}
	fresh := s.candidates(p, model, from)
	fresh = slices.DeleteFunc(fresh, func(next candidate) bool {
		if next.p.ID != c.p.ID || next.model != c.model || next.p.Account == nil || next.p.Account.Agent != "claude" {
			return true
		}
		if who, held := accountHolds(r); held && !accountAllowed(who, next) {
			return true
		}
		if who, held := keyHolds(r); held && !modelAllowed(who, next.p, next.model) {
			return true
		}
		return slices.ContainsFunc(tried, func(old candidate) bool { return old.who() == next.who() && old.model == next.model })
	})
	for i := range fresh {
		fresh[i].effort, fresh[i].fast = c.effort, c.fast
		fresh[i].firstToken, fresh[i].patience = c.firstToken, c.patience
	}
	return fresh
}
