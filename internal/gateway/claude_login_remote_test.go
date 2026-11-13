package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestClaudeLoginRemoteStreamsHoldOrdinaryAndHandoverRestarts(t *testing.T) {
	fresh(t)
	s := New()
	end := s.claudeLoginRemote().Begin()
	b := s.Busy()
	if b.Requests != 1 || b.Bridge != 1 || !b.Any() {
		t.Fatalf("remote TLS stream was not held across restarts: %+v", b)
	}
	end()
	b = s.Busy()
	if b.Any() || b.Bridge != 0 || b.Last.IsZero() || b.BridgeLast.IsZero() {
		t.Fatalf("remote TLS stream stayed busy: %+v", b)
	}
}

func TestClaudeLoginRemoteUsesOnlyTheSelectedDeviceKeyAndModel(t *testing.T) {
	fresh(t)
	const model = "claude-team-fixture/claude-opus-5-5"
	nas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/hello" {
			io.WriteString(w, `{"name":"magpie","version":"fixture"}`)
			return
		}
		io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","object":"model"}]}`)
	}))
	defer nas.Close()
	p := provider.Provider{ID: "nas", Name: "NAS", Preset: provider.RemoteMagpiePreset, Key: "device-key", Anthropic: nas.URL, Models: []string{model}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	r, err := resolveClaudeLoginRemote(context.Background(), "nas/"+model)
	if err != nil || r == nil || r.URL.String() != nas.URL || r.Model != model || r.Key != "device-key" {
		t.Fatalf("remote provider was not resolved correctly: %v", err)
	}
	p.Off = true
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveClaudeLoginRemote(context.Background(), "nas/"+model); err == nil {
		t.Fatal("disabled remote provider remained usable")
	}
}

func TestClaudeLoginRemoteGroupsStillUseLocalRouting(t *testing.T) {
	fresh(t)
	r, err := resolveClaudeLoginRemote(context.Background(), "group/choose-on-nas")
	if err != nil || r != nil {
		t.Fatal("a group bypassed its local routing policy")
	}
	if _, err := resolveClaudeLoginRemote(context.Background(), "missing/model"); err == nil {
		t.Fatal("removed provider did not fail closed")
	}
	if err := provider.Save(provider.Provider{ID: "local", Name: "Local", Key: "local-key", Anthropic: "http://127.0.0.1:1", Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	r, err = resolveClaudeLoginRemote(context.Background(), "local/m")
	if err != nil || r != nil {
		t.Fatal("local provider bypassed its local routing policy")
	}
}
