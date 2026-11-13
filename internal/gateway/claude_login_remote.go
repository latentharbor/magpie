package gateway

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/yetone/magpie/internal/claudelogin"
	"github.com/yetone/magpie/internal/provider"
)

// Native Threads and cache_touch belong to the NAS that owns the inference
// account. This opt-in path avoids parsing them in a second routing gateway.
func (s *Server) claudeLoginRemote() claudelogin.RemoteOptions {
	return claudelogin.RemoteOptions{Resolve: resolveClaudeLoginRemote, Begin: func() func() {
		s.inFlight.Add(1)
		end := s.bridgeTurn() // this proxy's TLS stream cannot be handed over
		return func() {
			end()
			s.lastDone.Store(time.Now().UnixNano())
			if s.inFlight.Add(-1) == 0 {
				s.armRelease(s.idle.restFor())
			}
		}
	}}
}

func resolveClaudeLoginRemote(_ context.Context, selected string) (*claudelogin.RemoteTarget, error) {
	if provider.IsGroupRef(selected) {
		return nil, nil // groups still need the local router to choose a member
	}
	p, model, ok := provider.ResolveRoute(selected)
	if !ok || p.Off {
		return nil, fmt.Errorf("mapped provider is unavailable")
	}
	if !p.IsRemoteMagpie() {
		return nil, nil
	}
	if p.Key == "" || p.SelfAnswered() || (p.KeyProtocol != "" && p.KeyProtocol != provider.Anthropic) {
		return nil, fmt.Errorf("remote gateway needs its own key and destination")
	}
	u, err := url.Parse(p.Anthropic)
	if err != nil {
		return nil, fmt.Errorf("invalid remote gateway URL")
	}
	return &claudelogin.RemoteTarget{URL: u, Key: p.Key, Model: model}, nil
}
