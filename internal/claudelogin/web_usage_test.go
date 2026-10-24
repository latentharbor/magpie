package claudelogin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const usageFixture = `{"five_hour":{"utilization":100,"resets_at":"fixture-reset"},"seven_day":{"utilization":100},"seat_tier":"unassigned","verification_required":true,"extra_usage":{"is_enabled":false},"limits":[{"kind":"session","percent":100,"severity":"critical"},{"kind":"weekly_all","percent":100,"severity":"critical"},{"kind":"weekly_scoped","percent":100,"severity":"critical"},{"kind":"future-unknown","percent":90}]}`

func TestOrganizationUsageScopeAndPreservedControls(t *testing.T) {
	const org = "49cd3c7d-fb4e-4cbc-8f73-aacdf1e4b8f7"
	for _, tc := range []struct {
		name, method, path string
		enabled, managed   bool
		status             int
		adapt              bool
	}{
		{"managed gzip", "GET", "/api/organizations/" + org + "/usage", true, true, 200, true},
		{"disabled", "GET", "/api/organizations/" + org + "/usage", false, true, 200, false},
		{"official usage", "GET", "/api/organizations/" + org + "/usage", true, false, 200, false},
		{"other org", "GET", "/api/organizations/other/usage", true, true, 200, false},
		{"subscription unchanged", "GET", "/api/organizations/" + org + "/subscription_details", true, true, 200, false},
		{"method unchanged", "POST", "/api/organizations/" + org + "/usage", true, true, 200, false},
		{"permission unchanged", "GET", "/api/organizations/" + org + "/usage", true, true, 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gz bytes.Buffer
			z := gzip.NewWriter(&gz)
			z.Write([]byte(usageFixture))
			z.Close()
			p := &Proxy{Config: func() (Config, error) {
				return Config{Enabled: tc.enabled, GatewayUsage: tc.managed, UsageOrganization: org, Listen: "127.0.0.1:3426", Models: map[string]string{"opus": "group/opus"}}, nil
			}, Official: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != UsageHost || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("Cookie") != "session=fixture" {
					t.Error("official identity or destination changed")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Encoding": []string{"gzip"}, "Etag": []string{"original"}}, Body: io.NopCloser(bytes.NewReader(gz.Bytes()))}, nil
			})}
			req := httptest.NewRequest(tc.method, "https://claude.ai"+tc.path, nil)
			req.Header.Set("Authorization", "Bearer fixture")
			req.Header.Set("Cookie", "session=fixture")
			rec := httptest.NewRecorder()
			p.webUsage(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d", rec.Code)
			}
			if !tc.adapt {
				if !bytes.Equal(rec.Body.Bytes(), gz.Bytes()) {
					t.Fatal("unrelated official reply changed")
				}
				return
			}
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("ETag") != "" {
				t.Fatal("stale response headers")
			}
			var obj map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
				t.Fatal(err)
			}
			if obj["seat_tier"] != "unassigned" || obj["verification_required"] != true || obj["extra_usage"].(map[string]any)["is_enabled"] != false {
				t.Fatal("subscription, verification or billing changed")
			}
			w := obj["five_hour"].(map[string]any)
			if w["utilization"] != float64(0) || w["resets_at"] != "fixture-reset" {
				t.Fatal("usage or reset wrong")
			}
			for i, row := range obj["limits"].([]any) {
				r := row.(map[string]any)
				if i < 3 {
					if r["percent"] != float64(0) || r["severity"] != "normal" {
						t.Fatal("normalized limits still exhausted")
					}
				} else if r["percent"] != float64(90) {
					t.Fatal("unknown window changed")
				}
			}
		})
	}
}
func TestWebUsageRejectsInvalidJSON(t *testing.T) {
	if _, err := gatewayWebUsage([]byte(`not json`)); err == nil {
		t.Fatal("invalid usage became available")
	}
	if _, err := gatewayWebUsage([]byte(`null`)); err == nil {
		t.Fatal("null usage became available")
	}
	if _, err := gatewayWebUsage([]byte(strings.Repeat("x", 20))); err == nil {
		t.Fatal("invalid usage accepted")
	}
}

func TestOrganizationUsageRealCONNECTAndRestrictedCA(t *testing.T) {
	const org = "49cd3c7d-fb4e-4cbc-8f73-aacdf1e4b8f7"
	client, p := fixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(usageFixture))}, nil
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("web usage reached inference gateway") }))
	a, err := EnsureWebAuthority(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.CA.PermittedDNSDomains) != 1 || a.CA.PermittedDNSDomains[0] != UsageHost || !a.CA.PermittedDNSDomainsCritical {
		t.Fatal("web CA not constrained")
	}
	p.WebAuthority = a
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(a.CA)
	p.Config = func() (Config, error) {
		return Config{Enabled: true, GatewayUsage: true, UsageOrganization: org, Listen: "127.0.0.1:3426", Models: map[string]string{"opus": "group/opus"}}, nil
	}
	res, err := client.Get("https://claude.ai/api/organizations/" + org + "/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(b, []byte(`"magpie_gateway_managed":true`)) {
		t.Fatalf("web CONNECT quota not handled: %d %s", res.StatusCode, b)
	}
	roots := x509.NewCertPool()
	roots.AddCert(a.CA)
	leaf, _ := x509.ParseCertificate(a.Certificate.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: Host}); err == nil {
		t.Fatal("web certificate authenticated inference API")
	}
}
