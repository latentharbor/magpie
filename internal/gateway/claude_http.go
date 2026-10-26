package gateway

// Opt-in HTTP inference shares the normal routing, accounting and Team drain
// path. It does not share a running CLI's conversation or refresh its tokens.
// The compatibility profile is pinned to an offline native CLI capture.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	ledger "github.com/yetone/magpie/internal/usage"
)

const claudeHTTPVersion = "2.1.310"

type claudeTransportKey struct{}
type claudeHTTPKey struct{}
type claudeHTTPProbeKey struct{}
type claudeHTTPCall struct {
	auth        provider.ClaudeHTTPCredentials
	session     string
	prompt      string
	turn        int
	native      bool
	headers     http.Header
	probe       bool
	diagnostic  *ledger.ClaudeHTTPDiagnostic
	callerCache bool
	owner       claudeThreadOwner
}

var claudeHTTPAuth = provider.ClaudeHTTPAuth

// Snapshot once at admission, including all account/fallback attempts. A GUI
// change affects the next request, never one already streaming or waiting.
func withClaudeTransport(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), claudeTransportKey{}, settings.Load().ClaudeTransport))
}

func usesClaudeHTTP(ctx context.Context) bool {
	mode, ok := ctx.Value(claudeTransportKey{}).(string)
	if !ok {
		mode = settings.Load().ClaudeTransport
	}
	return mode == "http"
}

func (s *Server) serveClaudeHTTP(w http.ResponseWriter, r *http.Request, from provider.Protocol, p provider.Provider, model string, body []byte, usage *Usage) (status int, message string) {
	auth, err := claudeHTTPAuth(p.Via(r.Context()), p.Account)
	if err != nil {
		msg := "Claude Code HTTP: " + err.Error()
		return writeError(w, from, http.StatusServiceUnavailable, msg), msg
	}
	defer func() {
		if status >= 400 {
			provider.NoteClaudeSignInFailure(p.Account.User, auth.LoginVersion, message)
		}
	}()
	owner := claudeOwner(p, model, auth)
	owner.grant = modelOf(body)
	if from == provider.Anthropic && s.claudeThreadMoved(r, body, owner) {
		writeJSON(w, http.StatusNotFound, claudeThreadErrorBody(http.StatusNotFound, claudeThreadReplay, "thread_not_found"))
		return http.StatusNotFound, claudeThreadReplay
	}
	// Never accept an endpoint or a device/account identity from an API caller.
	// A copied provider keeps the original account for scopes, meters and proxy.
	p.Anthropic = "https://api.anthropic.com"
	seed := auth.AccountUUID + "\x00" + conversationID(r.Header, body)
	session := uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
	turn := 0
	for _, m := range gjson.GetBytes(body, "messages").Array() {
		if m.Get("role").String() == "user" {
			turn++
		}
	}
	prompt := uuid.NewSHA1(uuid.NameSpaceOID, []byte(session+"\x00"+string(body))).String()
	c := claudeHTTPCall{auth: auth, session: session, prompt: prompt, turn: max(1, turn), headers: r.Header.Clone()}
	c.owner = owner
	c.diagnostic = &ledger.ClaudeHTTPDiagnostic{}
	usage.claudeHTTP = c.diagnostic
	c.probe, _ = r.Context().Value(claudeHTTPProbeKey{}).(bool)
	if from == provider.Anthropic && nativeClaudeHTTPRequest(r.Header, body) {
		c.native = true
		if id := claudeHTTPUUID(r.Header.Get("x-claude-code-session-id")); id != "" {
			c.session = id
		} else if id := claudeHTTPUUID(gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String()); id != "" {
			c.session = id
		}
		if id := claudeHTTPUUID(r.Header.Get("x-claude-code-prompt-id")); id != "" {
			c.prompt = id
		}
	}
	ctx := context.WithValue(r.Context(), claudeHTTPKey{}, c)
	r = r.WithContext(ctx)
	if from == provider.Anthropic {
		status, msg, _ := s.passthrough(w, r, p, from, model, body, usage)
		if status >= 200 && status < 300 {
			s.rememberClaudeAnchors(r, owner, usage)
		}
		return status, msg
	}
	return s.translate(w, r, p, from, provider.Anthropic, model, body, usage)
}

func claudeHTTPOf(ctx context.Context, p provider.Provider) (claudeHTTPCall, bool) {
	c, ok := ctx.Value(claudeHTTPKey{}).(claudeHTTPCall)
	return c, ok && p.Account != nil && p.Account.Agent == "claude"
}

// claudeHTTPBody preserves native tool names, call IDs, signed thinking and
// client cache breakpoints. No tool alias protocol is needed: Magpie, rather
// than a hidden CLI harness, receives and returns the native tool calls.
func claudeHTTPBody(body []byte, c claudeHTTPCall) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid Anthropic request")
	}
	callerCache := len(claudeHTTPCachePaths(body)) != 0
	c.callerCache = callerCache
	body, dropped, err := claudeHTTPUnsignedHistory(body)
	if err != nil {
		return nil, err
	}
	body, err = claudeHTTPPreparedBody(body, c)
	if err == nil && c.diagnostic != nil {
		*c.diagnostic = claudeHTTPDiagnostic(body, c, callerCache, dropped)
	}
	return body, err
}

func claudeHTTPPreparedBody(body []byte, c claudeHTTPCall) ([]byte, error) {
	if c.native {
		// Keep the caller's system blocks byte-for-byte, including cache marks,
		// SDK identity variants, attribution and continuity tags. Only the
		// selected credential owns device/account metadata; re-sign afterwards.
		body, err := claudeHTTPMetadata(body, c)
		if err != nil {
			return nil, err
		}
		return signAnthropicMessagesBody(body)
	}
	var system []json.RawMessage
	var identity json.RawMessage
	s := gjson.GetBytes(body, "system")
	if s.Type == gjson.String {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": s.String()})
		if claudeHTTPIdentityText(s.String()) {
			identity = b
		} else if !strings.HasPrefix(s.String(), "x-anthropic-billing-header:") {
			system = append(system, b)
		}
	} else if s.Exists() {
		if !s.IsArray() {
			return nil, fmt.Errorf("system must be text or an array")
		}
		for _, b := range s.Array() {
			t := b.Get("text").String()
			if claudeHTTPIdentityText(t) {
				if identity == nil {
					identity = json.RawMessage(b.Raw)
				}
				continue
			}
			if strings.HasPrefix(t, "x-anthropic-billing-header:") {
				continue
			}
			system = append(system, json.RawMessage(b.Raw))
		}
	}
	first := ""
	for _, m := range gjson.GetBytes(body, "messages").Array() {
		if m.Get("role").String() != "user" {
			continue
		}
		t := m.Get("content")
		if t.Type == gjson.String {
			first = t.String()
		} else {
			for _, b := range t.Array() {
				if b.Get("type").String() == "text" {
					first = b.Get("text").String()
					break
				}
			}
		}
		break
	}
	// Algorithm observed by CLIProxyAPI and checked against the local binary.
	// See third_party/cliproxyapi-LICENSE.txt for the compatibility reference.
	units := utf16.Encode([]rune(first))
	sampled := []uint16{'0', '0', '0'}
	for i, n := range []int{4, 7, 20} {
		if n < len(units) {
			sampled[i] = units[n]
		}
	}
	sum := sha256.Sum256([]byte("59cf53e54c78" + string(utf16.Decode(sampled)) + claudeHTTPVersion))
	billing := fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=cli; cch=00000; cc_prompt_id=%s; cc_turn_origin=human; cc_prompt_index=%d; cc_turn_index=%d;", claudeHTTPVersion, hex.EncodeToString(sum[:])[:3], c.prompt, max(1, c.turn), max(1, c.turn))
	b, _ := json.Marshal(map[string]string{"type": "text", "text": billing})
	if identity == nil {
		identity, _ = json.Marshal(map[string]string{"type": "text", "text": claudeHTTPIdentity})
	}
	system = append([]json.RawMessage{b, identity}, system...)
	var err error
	body, err = sjson.SetBytes(body, "system", system)
	if err != nil {
		return nil, err
	}
	body, err = claudeHTTPMetadata(body, c)
	if err != nil {
		return nil, err
	}
	if !gjson.GetBytes(body, "tools").Exists() {
		body, err = sjson.SetBytes(body, "tools", []any{})
		if err != nil {
			return nil, err
		}
	}
	if !gjson.GetBytes(body, "max_tokens").Exists() {
		body, err = sjson.SetBytes(body, "max_tokens", 32000)
	}
	if err != nil {
		return nil, err
	}
	if !gjson.GetBytes(body, "diagnostics").Exists() {
		body, err = sjson.SetBytes(body, "diagnostics", map[string]any{"previous_message_id": nil})
		if err != nil {
			return nil, err
		}
	}
	body, err = claudeHTTPDefaultCache(body, c)
	if err != nil {
		return nil, err
	}
	return signAnthropicMessagesBody(body)
}

const claudeHTTPIdentity = "You are Claude Code, Anthropic's official CLI for Claude."

func claudeHTTPHeaders(req *http.Request, body []byte, c claudeHTTPCall) {
	h := req.Header
	h.Set("Accept-Encoding", claudeHTTPAcceptEncoding)
	if c.native {
		for k, values := range c.headers {
			if claudeCodeHeader(k) || strings.EqualFold(k, "x-client-request-id") || strings.EqualFold(k, "Accept") || strings.EqualFold(k, "Accept-Encoding") {
				h[k] = append([]string(nil), values...)
			}
		}
		h.Del("x-api-key")
		// Organization selection belongs to the chosen OAuth credential, as
		// account_uuid does in metadata. A native caller may be signed into
		// a different organization from the account the router selected.
		h.Del("Anthropic-Organization-Id")
		h.Set("Authorization", "Bearer "+c.auth.Token)
		h.Set("x-claude-code-session-id", c.session)
		provider.MergeList(h, "Anthropic-Beta", "oauth-2025-04-20")
		return
	}
	h.Del("x-api-key")
	h.Set("Authorization", "Bearer "+c.auth.Token)
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "claude-cli/"+claudeHTTPVersion+" (external, cli)")
	h.Set("x-app", "cli")
	h.Set("x-claude-code-session-id", c.session)
	h.Set("x-claude-code-prompt-id", c.prompt)
	h.Set("x-claude-code-request-class", "main")
	if class := c.headers.Get("x-claude-code-request-class"); class != "" {
		h.Set("x-claude-code-request-class", class)
	}
	h.Set("x-client-request-id", uuid.NewString())
	h.Set("anthropic-version", "2023-06-01")
	h.Set("anthropic-dangerous-direct-browser-access", "true")
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64" // Node's process.arch, rather than Go's spelling.
	}
	for k, v := range map[string]string{"lang": "js", "package-version": "0.128.0", "runtime": "node", "runtime-version": "v26.3.0", "retry-count": "0", "timeout": "600", "arch": arch, "os": claudeHTTPOS()} {
		h.Set("x-stainless-"+k, v)
	}
	betas := []string{"claude-code-20250219", "oauth-2025-04-20", "interleaved-thinking-2025-05-14", "thinking-token-count-2026-05-13", "context-management-2025-06-27", "prompt-caching-scope-2026-01-05"}
	for _, m := range gjson.GetBytes(body, "messages").Array() {
		if m.Get("role").String() == "system" {
			betas = append(betas, "per-turn-control-2026-07-01", "mid-conversation-tool-changes-2026-07-01", "mid-conversation-system-clear-at-2026-08-21")
			break
		}
	}
	if gjson.GetBytes(body, "output_config.effort").Exists() {
		betas = append(betas, "effort-2025-11-24")
	}
	if gjson.GetBytes(body, "thinking.type").String() == "adaptive" {
		betas = append(betas, "thinking-binding-controls-2026-08-01")
	}
	if gjson.GetBytes(body, "thinking.display").String() == "updates" {
		betas = append(betas, "thinking-display-updates-2026-08-18")
	}
	// Inspect parsed fields, not text containing a pretend cache-control object.
	var ttl bool
	for _, path := range claudeHTTPCachePaths(body) {
		ttl = ttl || gjson.GetBytes(body, path+".ttl").String() == "1h"
	}
	if ttl {
		betas = append(betas, "extended-cache-ttl-2025-04-11")
	}
	if gjson.GetBytes(body, "diagnostics").Exists() {
		betas = append(betas, "cache-diagnosis-2026-04-07")
	}
	previous := h.Get("anthropic-beta")
	h.Del("anthropic-beta")
	h.Set("Anthropic-Beta", strings.Join(betas, ","))
	provider.MergeList(h, "Anthropic-Beta", previous)
}

func claudeHTTPOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return "Other"
	}
}

type claudeHTTPProbeReply struct{ h http.Header }

func (w *claudeHTTPProbeReply) Header() http.Header       { return w.h }
func (*claudeHTTPProbeReply) WriteHeader(int)             {}
func (*claudeHTTPProbeReply) Write(b []byte) (int, error) { return len(b), nil }

func probeClaudeHTTP(ctx context.Context, p provider.Provider, model string) error {
	s := New()
	defer s.subscription.abortAll()
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 8, "stream": false, "messages": []any{map[string]string{"role": "user", "content": "hi"}}})
	ctx = context.WithValue(ctx, claudeTransportKey{}, "http")
	ctx = context.WithValue(ctx, claudeHTTPProbeKey{}, true)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/messages", nil)
	if err != nil {
		return err
	}
	status, msg := s.serveClaudeSubscription(&claudeHTTPProbeReply{h: make(http.Header)}, r, provider.Anthropic, p, model, body, &Usage{})
	if status >= 400 {
		return errors.New(msg)
	}
	return nil
}

func init() { provider.ProbeClaudeHTTPVia(probeClaudeHTTP) }

// Header feedback reaches exactly the same quota observer as CLI events,
// including a hard rejection whose utilization still lags below 100%.
func heardClaudeHTTPLimits(p provider.Provider, res *http.Response) {
	if p.Account == nil || p.Account.Agent != "claude" || res == nil {
		return
	}
	var out []provider.ClaudeLimit
	for _, w := range []struct{ wire, kind string }{{"5h", "five_hour"}, {"7d", "seven_day"}, {"7d_opus", "seven_day_opus"}, {"7d_sonnet", "seven_day_sonnet"}, {"7d_oi", "seven_day_overage_included"}} {
		prefix := "anthropic-ratelimit-unified-" + w.wire
		used, err := strconv.ParseFloat(res.Header.Get(prefix+"-utilization"), 64)
		claim := res.Header.Get("anthropic-ratelimit-unified-representative-claim")
		rejected := res.Header.Get(prefix+"-status") == "rejected" || res.Header.Get("anthropic-ratelimit-unified-status") == "rejected" && (claim == w.wire || claim == w.kind)
		if rejected {
			used, err = 1, nil
		}
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 1 {
			continue
		}
		reset, _ := strconv.ParseInt(res.Header.Get(prefix+"-reset"), 10, 64)
		out = append(out, provider.ClaudeLimit{Kind: w.kind, Used: used, ResetsAt: reset})
	}
	provider.NoteClaudeLimits(p.Account.User, out)
}
