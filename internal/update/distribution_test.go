package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
)

func distributionFixture(t *testing.T) (ed25519.PrivateKey, *Release, []byte) {
	t.Helper()
	seed := sha256.Sum256([]byte("public deterministic update test fixture, never a real release key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	oldTrust, oldKey, oldVersion := distributionTrust, distributionPublicKey, distributionVersion
	distributionTrust = ReleaseRepository
	distributionPublicKey = base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	distributionVersion = "1.0.0"
	t.Cleanup(func() { distributionTrust, distributionPublicKey, distributionVersion = oldTrust, oldKey, oldVersion })
	r := &Release{Schema: 1, Repository: ReleaseRepository, Version: "1.0.1", Notes: "Maintenance.\n<!-- lang:zh -->\n维护。", URL: releaseHome + "/tag/v1.0.1", Assets: map[string]Asset{}}
	sum := sha256.Sum256([]byte("app"))
	for name, platform := range distributionPlatforms() {
		r.Assets[name] = Asset{Platform: platform, URL: releaseHome + "/download/v1.0.1/" + name, Size: 3, SHA256: hex.EncodeToString(sum[:])}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return key, r, raw
}

func fixtureSignature(key ed25519.PrivateKey, raw []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw)) + "\n")
}

func TestDistributionSignatureAndStrictManifest(t *testing.T) {
	key, _, raw := distributionFixture(t)
	rel, err := verifyDistribution(raw, fixtureSignature(key, raw))
	if err != nil || !trustedDistribution(rel) || mayStageDistribution(rel) != nil {
		t.Fatalf("valid signed release: %v", err)
	}
	for _, tc := range []struct{ name, raw string }{
		{"trailing JSON", string(raw) + "{}"},
		{"duplicate root", strings.Replace(string(raw), `"version":"1.0.1"`, `"version":"1.0.1","version":"9.0.0"`, 1)},
		{"case alias", strings.Replace(string(raw), `"version":"1.0.1"`, `"Version":"1.0.1"`, 1)},
		{"unknown root", strings.Replace(string(raw), `"schema":1`, `"schema":1,"next_key":"attacker"`, 1)},
		{"duplicate asset", strings.Replace(string(raw), `"size":3`, `"size":3,"size":4`, 1)},
		{"cross repository", strings.Replace(string(raw), "/latentharbor/magpie/releases/download/", "/other/repo/releases/download/", 1)},
		{"fractional size", strings.Replace(string(raw), `"size":3`, `"size":3.0`, 1)},
		{"bad platform", strings.Replace(string(raw), `"platform":"android/arm64"`, `"platform":"linux/arm64"`, 1)},
		{"missing platform", strings.Replace(string(raw), `"platform":"android/arm64",`, "", 1)},
		{"zero size", strings.Replace(string(raw), `"size":3`, `"size":0`, 1)},
		{"unknown schema", strings.Replace(string(raw), `"schema":1`, `"schema":2`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.raw == string(raw) {
				t.Fatal("test did not alter the fixture")
			}
			bad := []byte(tc.raw)
			if _, err := verifyDistribution(bad, fixtureSignature(key, bad)); err == nil {
				t.Fatal("ambiguous or unsafe, but correctly signed, manifest was accepted")
			}
		})
	}
	if _, err := verifyDistribution(append(raw, ' '), fixtureSignature(key, raw)); err == nil {
		t.Fatal("changed raw bytes accepted under the old signature")
	}
	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if _, err := verifyDistribution(raw, fixtureSignature(other, raw)); err == nil {
		t.Fatal("untrusted release signer accepted")
	}
	for _, sig := range [][]byte{nil, []byte("not a signature"), append(fixtureSignature(key, raw), '\n')} {
		if _, err := verifyDistribution(raw, sig); err == nil {
			t.Fatal("invalid signature encoding accepted")
		}
	}
}

func TestDistributionPinnedTrustAndRollback(t *testing.T) {
	key, _, raw := distributionFixture(t)
	rel, err := verifyDistribution(raw, fixtureSignature(key, raw))
	if err != nil {
		t.Fatal(err)
	}
	a := rel.Assets[AppAsset()]
	a.Size++
	rel.Assets[AppAsset()] = a
	if mayStageDistribution(rel) == nil {
		t.Fatal("release modified after authentication was staged")
	}
	rel, _ = verifyDistribution(raw, fixtureSignature(key, raw))
	for _, current := range []string{"1.0.1", "1.0.2", "dev", ""} {
		distributionVersion = current
		if mayStageDistribution(rel) == nil {
			t.Errorf("installed %q accepted equal, old or unversioned update", current)
		}
	}
	distributionVersion = "1.0.0"
	distributionTrust = "other/repository"
	if trustedDistribution(rel) || mayStageDistribution(rel) == nil {
		t.Fatal("another repository opted into this key")
	}
	distributionTrust = ReleaseRepository
	distributionPublicKey = ""
	if _, err := verifyDistribution(raw, fixtureSignature(key, raw)); err == nil || trustedDistribution(rel) {
		t.Fatal("missing build-pinned key became trusted")
	}
}

type distributionTransport func(*http.Request) (*http.Response, error)

func (f distributionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func distributionResponse(r *http.Request, code int, body string, location string) *http.Response {
	h := http.Header{}
	if location != "" {
		h.Set("Location", location)
	}
	return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d test", code), Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestDistributionFetchAuthenticatesBeforeStaging(t *testing.T) {
	key, _, raw := distributionFixture(t)
	t.Setenv("MAGPIE_UPDATE_FEED", "")
	old := client
	t.Cleanup(func() { client = old })
	var asked []string
	client = &http.Client{Transport: distributionTransport(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		switch r.URL.String() {
		case Feed():
			return distributionResponse(r, 302, "", releaseHome+"/download/v1.0.1/latest.json"), nil
		case releaseHome + "/download/v1.0.1/latest.json":
			return distributionResponse(r, 302, "", "https://release-assets.githubusercontent.com/fixture/manifest"), nil
		case "https://release-assets.githubusercontent.com/fixture/manifest":
			return distributionResponse(r, 200, string(raw), ""), nil
		case releaseHome + "/download/v1.0.1/latest.json.sig":
			return distributionResponse(r, 200, string(fixtureSignature(key, raw)), ""), nil
		default:
			return nil, fmt.Errorf("unexpected network request: %s", r.URL)
		}
	})}
	rel, err := LatestIn(context.Background(), "zh")
	if err != nil || rel.Notes != "维护。" || mayStageDistribution(rel) != nil {
		t.Fatalf("signed feed through GitHub CDN: %+v %v", rel, err)
	}
	if len(asked) != 4 {
		t.Fatalf("requests = %v", asked)
	}
	distributionVersion = "1.0.2"
	if _, err := Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("replayed older signed feed: %v", err)
	}
}

func TestDistributionRejectsRedirectAndAssetTampering(t *testing.T) {
	key, _, raw := distributionFixture(t)
	rel, err := verifyDistribution(raw, fixtureSignature(key, raw))
	if err != nil {
		t.Fatal(err)
	}
	old := client
	t.Cleanup(func() { client = old })
	asset := rel.Assets[AppAsset()]
	for _, target := range []string{
		"https://github.com/other/repo/releases/download/v1.0.1/app.zip",
		"https://github.com/latentharbor/magpie/releases/download/../elsewhere",
		"http://github.com/latentharbor/magpie/releases/download/v1.0.1/app.zip",
		"https://attacker.example/app.zip",
	} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			client = &http.Client{Transport: distributionTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					return nil, errorsNoRedirectTarget()
				}
				return distributionResponse(r, 302, "", target), nil
			})}
			path := filepath.Join(t.TempDir(), "download")
			if err := fetch(context.Background(), asset, path); err == nil || calls != 1 {
				t.Fatalf("unsafe redirect followed: calls=%d err=%v", calls, err)
			}
		})
	}
	for _, body := range []string{"ap", "apps", "bad"} {
		client = &http.Client{Transport: distributionTransport(func(r *http.Request) (*http.Response, error) {
			return distributionResponse(r, 200, body, ""), nil
		})}
		path := filepath.Join(t.TempDir(), "download")
		if err := fetch(context.Background(), asset, path); err == nil {
			t.Fatalf("tampered asset %q accepted", body)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("tampered download retained")
		}
	}
}

func errorsNoRedirectTarget() error { return fmt.Errorf("redirect target must never be requested") }

func TestDistributionAdHocTrustDoesNotDowngradeApple(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("codesign fixture is a Unix shell script")
	}
	key, _, raw := distributionFixture(t)
	rel, _ := verifyDistribution(raw, fixtureSignature(key, raw))
	for _, tc := range []struct {
		name, installed, downloaded string
		broken, wantOK              bool
	}{
		{"opted in and both ad hoc", "not set", "not set", false, true},
		{"Apple cannot downgrade", "TEAMONE", "not set", false, false},
		{"no silent signer migration", "not set", "TEAMONE", false, false},
		{"Apple same team", "TEAMONE", "TEAMONE", false, true},
		{"Apple different team", "TEAMONE", "TEAMTWO", false, false},
		{"invalid local code", "not set", "not set", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			exit := 0
			if tc.broken {
				exit = 1
			}
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --verify ]; then exit %d; fi\nif [ \"$2\" = installed.app ]; then echo 'TeamIdentifier=%s' >&2; else echo 'TeamIdentifier=%s' >&2; fi\necho Signature=adhoc >&2\n", exit, tc.installed, tc.downloaded)
			testenv.Program(t, filepath.Join(dir, "codesign"), script)
			t.Setenv("PATH", dir)
			if err := stagedSigner(context.Background(), "installed.app", "downloaded.app", rel); (err == nil) != tc.wantOK {
				t.Fatalf("staged signer acceptance=%v want=%v: %v", err == nil, tc.wantOK, err)
			}
			if tc.installed == "not set" && tc.downloaded == "not set" && !tc.broken {
				if err := stagedSigner(context.Background(), "installed.app", "downloaded.app", &Release{}); err == nil {
					t.Fatal("ad hoc accepted without authenticated proof")
				}
			}
		})
	}
}
