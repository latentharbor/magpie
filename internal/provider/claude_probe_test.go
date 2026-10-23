package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

// A Claude account is tested by Claude Code, with a saved account's own
// sign-in, and nothing magpie sends reaches Anthropic's API in its name.
func TestClaudeTestRunsThroughClaudeCode(t *testing.T) {
	claudeHome(t)
	oldHTTP := claudeHTTPProbe
	ProbeClaudeHTTPVia(func(context.Context, Provider, string) error {
		t.Error("default CLI test called HTTP transport")
		return errors.New("unexpected HTTP probe")
	})
	t.Cleanup(func() { claudeHTTPProbe = oldHTTP })
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("asked the API directly: %s %s", r.Method, r.URL)
	}))
	defer api.Close()
	type call struct{ oauth, model string }
	var mu sync.Mutex
	var calls []call
	old := claudeCLIProbe
	defer func() { claudeCLIProbe = old }()
	ProbeClaudeVia(func(_ context.Context, oauth, model string) error {
		mu.Lock()
		calls = append(calls, call{oauth, model})
		mu.Unlock()
		if model == "claude-bad" {
			return errors.New("Claude Code: no such model")
		}
		return nil
	})

	acct := &Account{Agent: "claude", User: "b@example.com", token: func(context.Context) (string, error) { return "saved-token", nil }}
	acct.sign = func(context.Context, *http.Request, []byte) error { return errClaudeViaCLI }
	p := Provider{ID: "claude", Anthropic: api.URL, Account: acct}
	r := p.TestModels(context.Background(), []string{"claude-sonnet-4-5", "claude-bad"})
	if len(r) != 2 || !r[0].OK || r[1].OK || r[1].Error != "Claude Code: no such model" || r[0].Protocol != Anthropic {
		t.Fatalf("results %+v", r)
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].model > calls[j].model }) // models are tried at once
	if len(calls) != 2 || calls[0] != (call{"saved-token", "claude-sonnet-4-5"}) || calls[1].model != "claude-bad" {
		t.Fatalf("Claude Code asked %+v", calls)
	}

	// the one Claude Code is signed in to: it signs itself
	calls = nil
	own := Provider{ID: "claude", Anthropic: api.URL, Account: &Account{Agent: "claude"}}
	if r := own.TestModels(context.Background(), []string{"claude-haiku-4-5"}); len(r) != 1 || !r[0].OK {
		t.Fatalf("results %+v", r)
	}
	if len(calls) != 1 || calls[0] != (call{"", "claude-haiku-4-5"}) {
		t.Fatalf("Claude Code asked %+v", calls)
	}

	req, _ := http.NewRequest(http.MethodPost, api.URL+"/v1/messages", nil)
	if err := p.Sign(context.Background(), req, Anthropic, nil); !errors.Is(err, errClaudeViaCLI) {
		t.Fatalf("a direct request was signed: %v", err)
	}
}

func claudeHTTPProbeFixture(t *testing.T) {
	t.Helper()
	claudeHome(t)
	s := settings.Load()
	s.ClaudeTransport = "http"
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	oldCLI, oldHTTP := claudeCLIProbe, claudeHTTPProbe
	t.Cleanup(func() { claudeCLIProbe, claudeHTTPProbe = oldCLI, oldHTTP })
	ProbeClaudeVia(func(context.Context, string, string) error {
		t.Error("HTTP test fell back to CLI")
		return errors.New("unexpected CLI probe")
	})
}

func TestClaudeHTTPTestUsesSelectedProvider(t *testing.T) {
	claudeHTTPProbeFixture(t)
	// A missing native executable must not stop an HTTP test.
	claudeCLIProbe = nil
	for _, user := range []string{"own@example.com", "saved@example.com"} {
		t.Run(user, func(t *testing.T) {
			a := &Account{Agent: "claude", User: user, token: func(context.Context) (string, error) {
				t.Fatal("HTTP test requested a native config directory")
				return "", nil
			}}
			p := Provider{ID: "claude", Account: a, Proxy: "http://account-proxy.invalid"}
			calls := 0
			ProbeClaudeHTTPVia(func(ctx context.Context, got Provider, model string) error {
				calls++
				if got.Account != a || got.Proxy != p.Proxy || model != "claude-sonnet-4-5" {
					t.Errorf("HTTP tested another provider/model: %+v, %s", got, model)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > claudeWait || time.Until(deadline) <= 0 {
					t.Error("HTTP probe has no bounded test timeout")
				}
				return nil
			})
			rs := p.TestModels(context.Background(), []string{"claude-sonnet-4-5"})
			if len(rs) != 1 || !rs[0].OK || rs[0].Account != user || rs[0].Protocol != Anthropic || calls != 1 {
				t.Fatalf("HTTP test result %+v; calls %d", rs, calls)
			}
		})
	}
}

func TestClaudeHTTPTestErrorsStayOnHTTPTransport(t *testing.T) {
	claudeHTTPProbeFixture(t)
	l := savedLogin{Agent: "claude", User: "saved@example.com", Auth: mustJSONRaw(t, map[string]any{"claudeAiOauth": claudeAuth{AccessToken: "access", RefreshToken: "refresh"}})}
	writeFile(t, loginsPath(), []savedLogin{l})
	p := Provider{ID: "claude", Account: &Account{Agent: "claude", User: l.User, token: func(context.Context) (string, error) {
		t.Error("HTTP failure used native account token")
		return "", errors.New("unexpected native token")
	}}}
	// Even text matching the native credential classifier must not mark the
	// saved login refused: the HTTP transport owns its error classification.
	want := "OAuth session expired and could not be refreshed"
	ProbeClaudeHTTPVia(func(context.Context, Provider, string) error { return errors.New(want) })
	r := p.testClaude(context.Background(), "claude-sonnet-4-5")
	if r.OK || r.Error != want || r.Account != l.User {
		t.Fatalf("HTTP failure: %+v", r)
	}
	for _, kept := range readLogins() {
		if kept.Lapsed != "" || kept.Refused != "" {
			t.Fatalf("HTTP failure classified as native sign-in refusal: %+v", kept)
		}
	}
	ProbeClaudeHTTPVia(nil)
	r = p.testClaude(context.Background(), "claude-sonnet-4-5")
	if r.OK || r.Error != "Claude HTTP transport isn't available to run the test" {
		t.Fatalf("missing HTTP transport: %+v", r)
	}
}
