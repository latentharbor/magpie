package claudelogin

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCacheTouchFollowsGatewayRequestOwnership(t *testing.T) {
	var touches, official atomic.Int32
	client, _ := fixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		official.Add(1)
		if r.URL.Path != "/v1/messages/cache_touch" || r.Header.Get("Authorization") != "Bearer official-fixture" {
			t.Error("official request changed")
		}
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: http.NoBody}, nil
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer magpie-claude-desktop" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("official credentials reached gateway")
		}
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("Request-Id", "req_gateway")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_gateway","type":"message"}`)
			return
		}
		touches.Add(1)
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/messages/cache_touch" || string(b) != `{"request_id":"req_gateway"}` || r.Header.Get("Content-Encoding") != "" || r.Header.Get("Anthropic-Beta") != "cache-keepalive-2026-09-03" {
			t.Errorf("native touch changed: %s %s", r.URL, b)
		}
		w.WriteHeader(204)
	}))
	post := func(path, body string, compressed bool) int {
		t.Helper()
		payload := []byte(body)
		if compressed {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			gz.Write(payload)
			gz.Close()
			payload = buf.Bytes()
		}
		r, _ := http.NewRequest("POST", "https://api.anthropic.com"+path, bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer official-fixture")
		r.Header.Set("Cookie", "fixture=secret")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Anthropic-Beta", "cache-keepalive-2026-09-03")
		if compressed {
			r.Header.Set("Content-Encoding", "gzip")
		}
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if post("/v1/messages", `{"model":"claude-opus-5-5","messages":[]}`, false) != 200 {
		t.Fatal("inference failed")
	}
	for _, compressed := range []bool{false, true} {
		if post("/v1/messages/cache_touch", `{"request_id":"req_gateway"}`, compressed) != 204 {
			t.Fatal("gateway cache touch failed")
		}
	}
	if official.Load() != 0 || touches.Load() != 2 {
		t.Fatal("gateway cache touched official account")
	}
	if post("/v1/messages/cache_touch", `{"request_id":"req_official"}`, false) != 204 || official.Load() != 1 {
		t.Fatal("unknown official cache touch was redirected")
	}
}

func TestCacheTouchDoesNotPretendSuccessWithoutOwner(t *testing.T) {
	p := &Proxy{
		Config: func() (Config, error) {
			return Config{Enabled: true, GatewayUsage: true, Listen: "127.0.0.1:3426", Models: map[string]string{"claude-opus-5-5": "group/opus"}}, nil
		},
		Official: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unknown gateway touch leaked to official")
			return nil, io.EOF
		}),
	}
	r, _ := http.NewRequest("POST", "https://"+Host+"/v1/messages/cache_touch", strings.NewReader(`{"request_id":"req_before_restart"}`))
	w := httptest.NewRecorder()
	p.inference(w, r)
	if w.Code != 404 {
		t.Fatalf("unknown touch claimed success: %d", w.Code)
	}
}
