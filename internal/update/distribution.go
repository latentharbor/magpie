package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/proc"
)

// These values are linked into an explicitly opted-in distribution build.
// They are never read from settings, environment variables or the network.
// An absent key is not trust in an unsigned feed. Rotating the key requires
// a reviewed build installed manually; a release cannot nominate its own key.
var distributionTrust, distributionPublicKey, distributionVersion string

type distributionProof struct {
	key         string
	fingerprint [32]byte
}

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func distributionKey() (ed25519.PublicKey, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(distributionPublicKey)
	if distributionTrust != ReleaseRepository || err != nil || len(key) != ed25519.PublicKeySize ||
		base64.StdEncoding.EncodeToString(key) != distributionPublicKey {
		return nil, errors.New("this distribution has no valid build-pinned update key; install a trusted distribution build manually")
	}
	return ed25519.PublicKey(key), nil
}

// distributionClient rejects cross-repository redirects and HTTP downgrades.
// GitHub's asset CDN is permitted only after the repository URL; content
// authentication never relies on the CDN, redirect URL or TLS alone.
func distributionClient() *http.Client {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 6 || !distributionURL(req.URL, len(via) > 0) {
			return errors.New("distribution download redirected outside its repository")
		}
		return nil
	}
	return &c
}

func distributionURL(u *url.URL, cdn bool) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" || path.Clean(u.Path) != u.Path {
		return false
	}
	if cdn && (u.Host == "release-assets.githubusercontent.com" || u.Host == "objects.githubusercontent.com") {
		return true
	}
	if u.Host != "github.com" || u.RawQuery != "" || u.RawPath != "" {
		return false
	}
	return strings.HasPrefix(u.Path, "/"+ReleaseRepository+"/releases/download/") ||
		strings.HasPrefix(u.Path, "/"+ReleaseRepository+"/releases/latest/download/")
}

func distributionBytes(ctx context.Context, address string, limit int64) ([]byte, error) {
	u, err := url.Parse(address)
	if err != nil || !distributionURL(u, false) {
		return nil, errors.New("distribution feed must belong to its pinned repository")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	res, err := distributionClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("distribution feed: %s", res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("distribution feed is incomplete or too large")
	}
	return b, nil
}

func latestDistribution(ctx context.Context, lang string) (*Release, error) {
	if _, err := distributionKey(); err != nil {
		return nil, err
	}
	raw, err := distributionBytes(ctx, Feed(), 2<<20)
	if err != nil {
		return nil, err
	}
	// This untrusted version is used only to fetch the signature from this
	// repository's exact release, avoiding a latest-pointer race. No asset is
	// trusted or downloaded until the raw bytes have been authenticated.
	var hint struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &hint) != nil || !stableVersion.MatchString(hint.Version) || parse(hint.Version) == nil {
		return nil, errors.New("distribution manifest has no stable version")
	}
	signature, err := distributionBytes(ctx, releaseHome+"/download/v"+hint.Version+"/latest.json.sig", 128)
	if err != nil {
		return nil, err
	}
	rel, err := verifyDistribution(raw, signature)
	if err != nil {
		return nil, err
	}
	if stableVersion.MatchString(distributionVersion) && Newer(distributionVersion, rel.Version) {
		return nil, errors.New("distribution feed is older than this installed version")
	}
	// Language selection is presentation only. Bind the resulting object too,
	// so a caller cannot alter an authenticated asset after verification.
	rel.Notes = InLang(rel.Notes, lang)
	rel.proof = &distributionProof{key: distributionPublicKey, fingerprint: distributionFingerprint(rel)}
	return rel, nil
}

func verifyDistribution(raw, signature []byte) (*Release, error) {
	key, err := distributionKey()
	if err != nil {
		return nil, err
	}
	text := strings.TrimSuffix(string(signature), "\n")
	sig, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(sig) != text ||
		!ed25519.Verify(key, raw, sig) {
		return nil, errors.New("distribution manifest signature is invalid")
	}
	if len(raw) > 2<<20 || !utf8.Valid(raw) {
		return nil, errors.New("distribution manifest is not valid UTF-8 JSON")
	}
	if err := uniqueJSON(raw); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || !exactFields(fields, "schema", "repository", "version", "notes", "url", "assets") {
		return nil, errors.New("distribution manifest has unexpected fields")
	}
	var entries map[string]map[string]json.RawMessage
	if json.Unmarshal(fields["assets"], &entries) != nil {
		return nil, errors.New("distribution assets are invalid")
	}
	for _, entry := range entries {
		if !exactFields(entry, "platform", "url", "size", "sha256") {
			return nil, errors.New("distribution asset has unexpected fields")
		}
	}
	var rel Release
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&rel); err != nil {
		return nil, fmt.Errorf("distribution manifest: %w", err)
	}
	if rel.Schema != 1 || rel.Repository != ReleaseRepository || !stableVersion.MatchString(rel.Version) || parse(rel.Version) == nil ||
		rel.URL != releaseHome+"/tag/v"+rel.Version {
		return nil, errors.New("distribution manifest identity is invalid")
	}
	want := distributionPlatforms()
	if len(rel.Assets) != len(want) {
		return nil, errors.New("distribution manifest is missing platform assets")
	}
	for name, a := range rel.Assets {
		sum, err := hex.DecodeString(a.SHA256)
		if want[name] == "" || a.Platform != want[name] || a.Size <= 0 || a.Size > 2<<30 ||
			err != nil || len(sum) != sha256.Size || hex.EncodeToString(sum) != a.SHA256 ||
			a.URL != releaseHome+"/download/v"+rel.Version+"/"+name {
			return nil, errors.New("distribution asset identity, size or checksum is invalid")
		}
		a.signed = true
		rel.Assets[name] = a
	}
	rel.proof = &distributionProof{key: distributionPublicKey, fingerprint: distributionFingerprint(&rel)}
	return &rel, nil
}

func exactFields(fields map[string]json.RawMessage, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

// uniqueJSON rejects duplicate keys (including case aliases), trailing JSON
// values and excessive nesting, before Go's permissive struct decoder runs.
func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return errors.New("distribution manifest nests too deeply")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || seen[strings.ToLower(key)] {
					return errors.New("distribution manifest repeats a JSON key")
				}
				seen[strings.ToLower(key)] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
		case json.Delim('['):
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
		}
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("distribution manifest has trailing data")
	}
	return nil
}

func distributionPlatforms() map[string]string {
	out := map[string]string{}
	for _, arch := range []string{"amd64", "arm64"} {
		out["magpie-darwin-"+arch+".zip"] = "darwin/" + arch
		out["magpie-linux-"+arch] = "linux/" + arch
		out["magpie-windows-"+arch+".exe"] = "windows/" + arch
		for _, system := range []string{"darwin", "linux", "windows"} {
			name := "magpie-cli-" + system + "-" + arch
			if system == "windows" {
				name += ".exe"
			}
			out[name] = system + "/" + arch
		}
	}
	out["magpie-cli-android-arm64"] = "android/arm64"
	return out
}

func distributionFingerprint(rel *Release) [32]byte {
	b, _ := json.Marshal(rel)
	return sha256.Sum256(b)
}

func trustedDistribution(rel *Release) bool {
	_, err := distributionKey()
	return err == nil && rel != nil && rel.proof != nil && rel.proof.key == distributionPublicKey &&
		rel.proof.fingerprint == distributionFingerprint(rel)
}

func mayStageDistribution(rel *Release) error {
	if distributionTrust == "" && rel != nil && rel.proof == nil {
		return nil // Existing Apple-signed/upstream and explicit fixture feeds.
	}
	if !trustedDistribution(rel) {
		return errors.New("update has no authenticated distribution manifest")
	}
	if !stableVersion.MatchString(distributionVersion) || !Newer(rel.Version, distributionVersion) {
		return errors.New("distribution update must be newer than the installed build")
	}
	return nil
}

func stagedSigner(ctx context.Context, installed, downloaded string, rel *Release) error {
	err := sameSigner(ctx, installed, downloaded)
	if err == nil {
		return nil
	}
	// Apple-signed installations never silently downgrade to ad-hoc signing.
	// Only an explicitly opted-in build with an authenticated manifest takes
	// this path, and both bundles must still pass strict local code validation.
	if !trustedDistribution(rel) || !adHocSigner(ctx, installed) || !adHocSigner(ctx, downloaded) {
		return err
	}
	for _, app := range []string{installed, downloaded} {
		if b, err := proc.CommandContext(ctx, "codesign", "--verify", "--deep", "--strict", app).CombinedOutput(); err != nil {
			return fmt.Errorf("the app's local code signature is broken: %s", strings.TrimSpace(string(b)))
		}
	}
	return nil
}

func adHocSigner(ctx context.Context, app string) bool {
	b, err := proc.CommandContext(ctx, "codesign", "-dv", app).CombinedOutput()
	if err != nil {
		return false
	}
	lines := "\n" + string(b) + "\n"
	return strings.Contains(lines, "\nSignature=adhoc\n") && strings.Contains(lines, "\nTeamIdentifier=not set\n")
}
