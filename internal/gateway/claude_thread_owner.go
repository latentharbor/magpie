package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

type claudeThreadOwner struct {
	provider, model, user, account, organization string
	grant                                        string
}

func claudeAnchorKey(r *http.Request, kind, id string) string {
	if id == "" || len(id) > 256 {
		return ""
	}
	return access.Caller(r.Context()).KeyID + "\x00" + kind + "\x00" + id
}

func claudeOwner(p provider.Provider, model string, auth provider.ClaudeHTTPCredentials) claudeThreadOwner {
	return claudeThreadOwner{provider: p.ID, model: model, user: p.Account.User, account: auth.AccountUUID, organization: auth.OrganizationUUID}
}

func (o claudeThreadOwner) sameIdentity(other claudeThreadOwner) bool {
	o.grant, other.grant = "", ""
	return o == other
}

const claudeThreadReplay = "No thread state was found for the requested `previous_message_id`. Replay the full conversation with `thread: {\"type\": \"create\"}` to start a new Thread."

// Routing owns account selection. Never send one account's continuation delta
// to another, nor turn it into an incomplete create. Code owns the full replay.
func (s *Server) claudeThreadMoved(r *http.Request, body []byte, owner claudeThreadOwner) bool {
	if gjson.GetBytes(body, "thread.type").String() != "continue" {
		return false
	}
	key := claudeAnchorKey(r, "message", gjson.GetBytes(body, "thread.previous_message_id").String())
	old, known := s.claudeAnchors.Get(key, time.Now())
	return known && !old.sameIdentity(owner)
}

func (s *Server) rememberClaudeAnchors(r *http.Request, owner claudeThreadOwner, u *Usage) {
	now := time.Now()
	s.claudeAnchors.Put(claudeAnchorKey(r, "message", u.ResponseID), owner, now)
	s.claudeAnchors.Put(claudeAnchorKey(r, "request", u.RequestID), owner, now)
}

// Native 2.1.309 posts exactly {request_id: ...}; it is not a messages
// request and has no model or conversation history. Only a successful request
// owned by this caller may choose credentials. Unknown anchors fail explicitly.
func (s *Server) claudeCacheTouch(w http.ResponseWriter, r *http.Request) {
	r = withClaudeTransport(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(body) > 4096 || !gjson.ValidBytes(body) {
		writeError(w, provider.Anthropic, 400, "invalid Claude cache touch")
		return
	}
	id := gjson.GetBytes(body, "request_id")
	if id.Type != gjson.String {
		writeError(w, provider.Anthropic, 400, "Claude cache touch requires request_id")
		return
	}
	key := claudeAnchorKey(r, "request", id.String())
	owner, known := s.claudeAnchors.Get(key, time.Now())
	if !known || !usesClaudeHTTP(r.Context()) {
		writeError(w, provider.Anthropic, 404, "Claude cache request is unknown or no longer available")
		return
	}
	p, model, ok := provider.Resolve(owner.provider + "/" + owner.model)
	if ok {
		for _, c := range s.candidates(p, model, provider.Anthropic) {
			if c.p.ID != owner.provider || c.model != owner.model || c.p.Account == nil || c.p.Account.Agent != "claude" || c.p.Account.User != owner.user {
				continue
			}
			if who, held := accountHolds(r); held && !accountAllowed(who, c) {
				continue
			}
			if who, held := keyHolds(r); held && !claudeTouchAllowed(who, owner, c) {
				continue
			}
			s.touchClaudeOwner(w, r, body, key, owner, c.p)
			return
		}
	}
	writeError(w, provider.Anthropic, 404, "Claude cache account is no longer eligible")
}

func claudeTouchAllowed(who access.Identity, owner claudeThreadOwner, c candidate) bool {
	if modelAllowed(who, c.p, c.model) {
		return true
	}
	// Group grants authorize only members the group still contains. A removed
	// member cannot spend credentials through a remembered cache request.
	grant := owner.grant
	if id, ok := provider.GroupFor(grant); ok {
		grant = id
	}
	g, members, ok := provider.FindGroup(grant)
	if !ok || !groupAllowed(who, g, members) {
		return false
	}
	for _, m := range members {
		if m.Provider.ID == c.p.ID && m.Model == c.model {
			return true
		}
	}
	return false
}

func (s *Server) touchClaudeOwner(w http.ResponseWriter, r *http.Request, body []byte, key string, owner claudeThreadOwner, p provider.Provider) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	release, admitted := provider.TrackClaudeTeamRequest(p.Account.User)
	if !admitted {
		writeError(w, provider.Anthropic, 409, "Claude cache account is rotating")
		return
	}
	defer release()
	auth, err := claudeHTTPAuth(p.Via(r.Context()), p.Account)
	if err != nil || !claudeOwner(p, owner.model, auth).sameIdentity(owner) {
		writeError(w, provider.Anthropic, 404, "Claude cache account identity is no longer available")
		return
	}
	c := claudeHTTPCall{auth: auth, native: true, headers: r.Header.Clone()}
	ctx = context.WithValue(p.Via(r.Context()), claudeHTTPKey{}, c)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages/cache_touch", bytes.NewReader(body))
	if err != nil {
		writeError(w, provider.Anthropic, 500, "cannot construct Claude cache touch")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	claudeHTTPHeaders(req, body, c)
	// Do not let a cache-only operation introduce an empty inference session.
	req.Header.Del("x-claude-code-session-id")
	if v := r.Header.Get("x-claude-code-session-id"); v != "" {
		req.Header.Set("x-claude-code-session-id", v)
	}
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := p.Do(&client, req)
	if err != nil {
		writeError(w, provider.Anthropic, 502, "Claude cache touch upstream unavailable")
		return
	}
	defer res.Body.Close()
	for _, h := range []string{"Content-Type", "Request-Id", "X-Request-Id", "Retry-After"} {
		if v := res.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		s.claudeAnchors.Put(key, owner, time.Now())
	}
	w.WriteHeader(res.StatusCode)
	io.Copy(w, io.LimitReader(res.Body, 1<<20))
}
