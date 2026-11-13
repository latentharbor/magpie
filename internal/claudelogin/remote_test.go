package claudelogin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteInferenceKeepsNativeThreadsAndCacheTouch(t *testing.T) {
	const mapped = "nas/claude-team-fixture/claude-opus-5-5"
	const native = "claude-team-fixture/claude-opus-5-5"
	var calls, active atomic.Int32
	var wantMu sync.Mutex
	var wantBody string
	nas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		wantMu.Lock()
		want := wantBody
		wantMu.Unlock()
		if string(body) != want || r.Header.Get("Authorization") != "Bearer device-key" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Anthropic-Organization-Id") != "" {
			t.Errorf("remote body or credential boundary changed: %s", body)
		}
		if r.Header.Get("Anthropic-Beta") != "message-threads-2026-08-12,cache-keepalive-2026-09-03" || r.Header.Get("X-Claude-Code-Session-Id") != "native-session" || active.Load() != 1 {
			t.Error("native headers or process ownership lost")
		}
		if r.URL.Path == "/v1/messages/cache_touch" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Request-Id", "req_nas")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "not-official=1")
		io.WriteString(w, `{"id":"msg_nas","type":"message"}`)
	}))
	defer nas.Close()
	u, _ := url.Parse(nas.URL)
	client, p := fixture(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("selected request reached official transport")
		return nil, io.EOF
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("native remote request entered the local routing gateway")
		w.WriteHeader(500)
	}))
	p.Config = func() (Config, error) {
		return Config{Enabled: true, GatewayUsage: true, RemoteInference: true, Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": mapped}}, nil
	}
	p.Remote = RemoteOptions{Resolve: func(_ context.Context, selected string) (*RemoteTarget, error) {
		if selected != mapped {
			t.Errorf("wrong saved mapping: %q", selected)
		}
		return &RemoteTarget{URL: u, Key: "device-key", Model: native}, nil
	}, Begin: func() func() {
		active.Add(1)
		return func() { active.Add(-1) }
	}}
	post := func(path, body string) int {
		t.Helper()
		r, _ := http.NewRequest("POST", "https://"+Host+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer official-secret")
		r.Header.Set("Cookie", "sessionKey=official-secret")
		r.Header.Set("X-Api-Key", "official-secret")
		r.Header.Set("Anthropic-Organization-Id", "official-org")
		r.Header.Set("Anthropic-Beta", "message-threads-2026-08-12,cache-keepalive-2026-09-03")
		r.Header.Set("X-Claude-Code-Session-Id", "native-session")
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.Header.Get("Set-Cookie") != "" {
			t.Error("remote cookie reached official client")
		}
		return res.StatusCode
	}
	for _, thread := range []string{`{"type":"create"}`, `{"type":"continue", "previous_message_id":"msg_nas"}`} {
		body := `{"model":"claude-opus-5-5", "thread":` + thread + `, "system":[{"type":"text", "text":"native prefix", "cache_control":{"type":"ephemeral","ttl":"5m"}}], "messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"native","signature":"unchanged-signature"}]}], "output_config":{"effort":"max"}}`
		wantMu.Lock()
		wantBody = strings.Replace(body, `"model":"claude-opus-5-5"`, `"model":"`+native+`"`, 1)
		wantMu.Unlock()
		if code := post("/v1/messages", body); code != 200 {
			t.Fatalf("native thread status %d", code)
		}
	}
	wantMu.Lock()
	wantBody = `{"request_id":"req_nas"}`
	wantMu.Unlock()
	if code := post("/v1/messages/cache_touch", `{"request_id":"req_nas"}`); code != 204 {
		t.Fatalf("cache touch status %d", code)
	}
	before := calls.Load()
	if code := post("/v1/messages/cache_touch", `{"request_id":"unknown"}`); code != 404 || calls.Load() != before {
		t.Fatal("unknown ownership reached the remote gateway")
	}
	res, err := client.Get("https://" + Host + "/api/oauth/usage")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("magpie_gateway_managed")) || calls.Load() != before {
		t.Fatal("local quota handling moved to the remote gateway")
	}
}

func TestRemoteInferenceCancellationReleasesProcessOwnership(t *testing.T) {
	started, cancelled, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	nas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer nas.Close()
	u, _ := url.Parse(nas.URL)
	client, p := fixture(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("local gateway called") }))
	p.Config = func() (Config, error) {
		return Config{Enabled: true, RemoteInference: true, Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "nas/claude-team-fixture/opus"}}, nil
	}
	p.Remote = RemoteOptions{Resolve: func(context.Context, string) (*RemoteTarget, error) {
		return &RemoteTarget{URL: u, Key: "device-key", Model: "claude-team-fixture/opus"}, nil
	}, Begin: func() func() { return func() { close(ended) } }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", "https://"+Host+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","stream":true}`))
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("remote stream never started")
	}
	select {
	case <-ended:
		t.Fatal("ownership released before stream ended")
	default:
	}
	cancel()
	for name, done := range map[string]<-chan struct{}{"remote cancellation": cancelled, "ownership release": ended} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("missing %s", name)
		}
	}
}

func TestRemoteInferenceFailsClosedAndLocalMappingsStayLocal(t *testing.T) {
	for _, mode := range []string{"off", "local", "removed", "invalid", "no-owner"} {
		t.Run(mode, func(t *testing.T) {
			var local atomic.Int32
			client, p := fixture(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("failure leaked to official transport")
				return nil, io.EOF
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				local.Add(1)
				body, _ := io.ReadAll(r.Body)
				if !bytes.Contains(body, []byte(`"model":"group/opus"`)) || r.Header.Get("Authorization") != "Bearer magpie-claude-desktop" {
					t.Error("local routing changed")
				}
				w.WriteHeader(204)
			}))
			p.Config = func() (Config, error) {
				return Config{Enabled: true, RemoteInference: mode != "off", Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "group/opus"}}, nil
			}
			p.Remote.Resolve = func(context.Context, string) (*RemoteTarget, error) {
				switch mode {
				case "off":
					t.Error("disabled remote mode resolved a remote target")
				case "local":
					return nil, nil
				case "removed":
					return nil, fmt.Errorf("private resolver failure")
				case "invalid":
					return &RemoteTarget{URL: &url.URL{Scheme: "file", Path: "/private"}, Key: "secret", Model: "m"}, nil
				case "no-owner":
					return &RemoteTarget{URL: p.Gateway, Key: "secret", Model: "m"}, nil
				}
				return nil, nil
			}
			if mode != "no-owner" {
				p.Remote.Begin = func() func() { return func() {} }
			}
			r, _ := http.NewRequest("POST", "https://"+Host+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
			res, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if mode == "off" || mode == "local" {
				if res.StatusCode != 204 || local.Load() != 1 {
					t.Fatalf("local routing status %d", res.StatusCode)
				}
			} else if res.StatusCode != 503 || local.Load() != 0 || bytes.Contains(body, []byte("private resolver")) || bytes.Contains(body, []byte("secret")) {
				t.Fatalf("remote failure did not fail closed: %d %s", res.StatusCode, body)
			}
		})
	}
}
