package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeTeamAdministrationErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, mitigated, want string
		status                                   int
	}{
		{"challenge header", "application/json", `{"error":{"type":"authentication_error"}}`, "challenge", "Cloudflare browser verification required", 401},
		{"challenge HTML", "text/html", `<title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/x"></script>`, "", "Cloudflare browser verification required", 403},
		{"ordinary HTML", "text/html", `<title>Forbidden</title>`, "", "unclassified forbidden", 403},
		{"marker alone", "text/html", `/cdn-cgi/challenge-platform/x`, "", "unclassified forbidden", 403},
		{"authentication", "application/json", `{"error":{"type":"authentication_error","message":"private-token"}}`, "", "administrator authentication rejected", 403},
		{"permission", "application/json", `{"error":{"type":"permission_error"}}`, "", "administrator permission denied", 403},
		{"unauthorized", "text/plain", "private-token", "", "administrator authentication rejected", 401},
		{"unknown JSON", "application/json", `{"error":{"type":"private-token","message":"private-token"}}`, "", "unclassified forbidden", 403},
		{"admin throttle", "application/json", `{}`, "", "administration rate limited", 429},
		{"upstream failure", "application/json", `{}`, "", "cause is unclassified", 503},
		{"oversized", "application/json", strings.Repeat("private-token", 6000), "", "body incomplete", 403},
		{"success challenge", "text/html", "", "challenge", "Cloudflare browser verification required", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("cf-mitigated", tc.mitigated)
				w.Header().Set("cf-ray", "0123456789abcdef-SIN")
				w.Header().Set("Server", "cloudflare")
				w.Header().Set("request-id", "private-token")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := &claudeTeamHTTP{org: teamOrg, key: "private-token", base: server.URL, client: server.Client()}
			_, err := client.Members(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("classification or secret suppression failed: %v", err)
			}
			if strings.Contains(err.Error(), "Cloudflare") != strings.Contains(tc.want, "Cloudflare") {
				t.Fatalf("unconfirmed challenge attribution: %v", err)
			}
			if !strings.Contains(err.Error(), "cf-ray=0123456789abcdef-SIN") || !strings.Contains(err.Error(), "member list") {
				t.Fatalf("missing safe context: %v", err)
			}
		})
	}
}

type claudeTeamUnreadableBody struct{}

func (claudeTeamUnreadableBody) Read([]byte) (int, error) { return 0, errors.New("private-token") }
func (claudeTeamUnreadableBody) Close() error             { return nil }

func TestClaudeTeamAdministrationUnreadableError(t *testing.T) {
	res := &http.Response{StatusCode: 403, Header: make(http.Header), Body: claudeTeamUnreadableBody{}}
	res.Header.Set("request-id", "req_12345678")
	err := claudeTeamResponseError(http.MethodPut, res)
	for _, want := range []string{"unclassified forbidden", "body incomplete", "seat update", "request-id=req_12345678"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "private-token") {
		t.Fatal("reader error leaked")
	}
}
