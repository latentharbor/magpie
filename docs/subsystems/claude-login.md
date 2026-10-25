# Keep Claude signed in (experimental, macOS)

This optional explicit CONNECT proxy preserves Claude Desktop's official app
and authentication. It routes only exact, user-selected inference model IDs to
Magpie. It does not unlock entitlements, patch the app, fabricate model
capabilities, or guarantee that every native feature works on another provider.

## Ownership and source of truth

- `internal/claudelogin`: configuration, private CA generation, TLS proxy,
  stream cancellation, and gateway-owned listener lifecycle.
- `internal/agent/desktop_login.go`: reversible native Desktop configuration.
- `internal/gui/claude_login.go`, `assets/claude-login*.js`: Settings → Network
  editor, status, explicit certificate preparation. The editor loads on demand.
- `internal/cli/claude_login.go`: `magpie claude-login` status, prepare, map,
  unmap, on, off, and `usage gateway|official`. GUI and CLI use the same serialized configuration writer.
- `gateway.Server.ListenAndServe`: starts the optional keeper only in the gateway process.

Configuration is `claude-login.json` under Magpie's configuration directory,
mode 0600. Fields are `enabled`, a literal loopback `listen` IP with a nonzero
numeric port (default `127.0.0.1:3426`), and `models`, mapping exact Claude
model IDs to Magpie `provider/model` or `group/id` identifiers. Wildcards are
rejected. The feature is disabled by default. Status reads do not generate keys,
change Desktop, or start a listener.

The editor owns a full-width stacked body rather than the shared label/control
grid. Mapping destinations can be selected from the current served models and
ready routing groups, or entered explicitly. A provider bulk action maps its
exact Claude model IDs and replaces destinations for those same sources only.
Unavailable saved targets remain visible with a warning; enabling rejects them
in both the editor and save API. Removing a Team never silently redirects its
inference to another organization. Select a replacement target explicitly.

## Native Desktop configuration

Static analysis of the installed official macOS **3.6556.2** bundle confirms
`egressProxyUrl` supports both first-party and third-party scopes. Its startup,
Code, Cowork HostLoop, and VM proxy paths exist. This is evidence for a native
configuration path, not an end-to-end compatibility verdict for future versions.

Enabling writes a separate proxy-only `Claude-3p/configLibrary` profile, selects
it in `_meta.json.appliedId`, and sets `deploymentMode: "1p"` in both Desktop
configuration directories. The profile contains no gateway provider, API key or
feature override. Existing third-party profiles and authentication data stay in
place. Desktop must be quit and reopened after enabling or disabling.

The normal third-party connection cannot be changed while this mode owns its
configuration. An inactive saved third-party profile no longer causes Magpie to
publish Desktop's third-party model capabilities. Native login routing is treated
as a connection beside the official provider, not as an externally replaced
third-party setup. The agent provider field reports magpie for this selected
proxy profile, with an official-login notice. Native mode and proxy changes still
produce drift warnings; the saved inactive third-party profile does not. The
Agents page names this native proxy connection explicitly and omits the inactive
third-party model-menu count.

`claude-login-wiring.json` records the four original JSON values durably before
any native write. All files are validated before mutation. Disabling restores
only values still owned by this operation and preserves unrelated settings. A
conflicting user edit or unreadable journal fails explicitly and keeps the
journal for recovery. No application data directory is deleted. Managed plist
policies and remote bootstrap profiles are not overridden.

## TLS and credentials

The proxy accepts only loopback native clients, rejects browser Origin headers,
and binds only a loopback IP. By default, only `api.anthropic.com:443` is decrypted; the TLS
SNI and inner HTTP authority must match it. Other HTTPS CONNECT destinations are
opaque tunnels pinned to resolved public addresses; private and metadata-network
destinations are refused. The upstream official TLS connection is validated
normally. No hosts file, PF rule, global OS proxy, Gatekeeper setting or certificate
validation bypass is used.

Explicit Prepare generates `claude-login-ca/authority.pem` (private CA/key bundle,
0600, directory 0700) and `ca.pem` (public certificate only). The generated CA has
a critical DNS name constraint for `api.anthropic.com`; the proxy issues and serves
only that host's leaf certificate. The private key must never be imported into a
trust store, uploaded, exported in a bug report, or shared. Existing corrupt or
expired authorities are not silently replaced. Read-only status verifies the public
certificate matches its private authority and reports its SHA-256 fingerprint.

**Trust is a separate user action.** Preparing does not install trust. Enabling is
blocked until the public certificate verifies against the current user's system
trust. Trusting a local CA is a material trust change; its scope and fingerprint
must be reviewed before real activation. Removing trust after use is separate from
configuration rollback, to avoid touching certificates the user manages.

On macOS use SSL-policy trust for this restricted CA. Chromium ignores Keychain
trust entries with a hostname policy string, even when macOS itself verifies the
leaf successfully. The CA certificate itself enforces its critical DNS constraint;
do not remove that constraint or bypass client certificate validation. Source:
[Chromium trust_store_mac.cc](https://chromium.googlesource.com/chromium/src/+/main/net/cert/internal/trust_store_mac.cc).
A successful system-trust status alone is not proof of Desktop or guest trust.

## Requests and failures

Only POST `/v1/messages` and `/v1/messages/count_tokens` examine model mappings.
Matching bodies retain tools, thinking signatures, cache markers, metadata and
`output_config`; only `model` is rewritten. Gzip input is supported, with a
64 MiB compressed/decoded limit. Native inference headers are retained through
an allowlist, while official Authorization, cookies, API keys and organization
headers are discarded. Magpie gets its local synthetic bearer identity instead.
Selected replies cannot set the official client's cookies or redirect it.

Native `POST /v1/messages/cache_touch` carries a `request_id`, not a model or
message ID (static Claude Code 2.1.309 reference is in gateway testdata).
The proxy remembers successful selected inference request IDs, including at
stream acceptance, and sends their touches to the gateway with official
credentials stripped. It does not rewrite the touch body or infer a new turn.
Only still-mapped targets qualify. Unknown IDs stay official when official
inference is allowed; with gateway-managed usage they fail explicitly.
The gateway separately resolves the original inference account and checks its
current eligibility and grants; the proxy's model mapping cannot select OAuth
credentials itself. Ownership metadata is bounded, process-local and does not
survive restart. Native upstream activation headers and touch errors are retained;
neither component manufactures keepalive success.

By default, unmatched inference and control-plane requests keep the official destination,
credentials and body. Control-plane forwarding does not depend on a readable
routing configuration. Broken inference rules fail explicitly; they never silently
fall through to paid official inference. A gateway refusal or transport failure
also never retries through the official subscription. SSE is flushed as received,
and cancelling a Desktop stream cancels the gateway request.

### Gateway-managed local Code usage

The opt-in `gatewayUsage` setting (`magpie claude-login usage gateway`) answers
GET `/api/oauth/usage` locally while routing is enabled. Native Code sees zero used
in its 5-hour and weekly windows, unknown reset times, and disabled extra billing.
This is local gateway availability, not the official subscription's remaining
quota. The response has `Cache-Control: no-store` and a `magpie_gateway_managed`
marker. Actual provider limits, failures and account rotation remain in Magpie.

Every Code inference model must be mapped in this mode: an unmapped messages or
count_tokens request fails explicitly instead of consuming official quota after
the local availability check. Official subscription, OAuth authorization, profile,
verification and cloud-side controls remain unchanged. This option does not make
an unsubscribed account work and does not provide unlimited backend allowance.
The subscribed first-party Code UI also uses the organization's claude.ai usage
response. Setting `usageOrganization` to one explicit UUID opts into that exact
GET endpoint, using a separately prepared and separately trusted host-constrained
CA (`claude-login prepare-usage`, `claude-login usage-organization <UUID>`).
No other organization or endpoint is modified. The original response preserves
unknown fields, reset times, subscription, verification and billing state. Only
known 5-hour/weekly counters and normalized session/weekly limit rows become
zero used with normal severity. Non-200 replies remain official errors. Malformed
or unsupported quota replies fail explicitly instead of pretending availability.

This additional interception takes effect on the next claude.ai CONNECT after
trusting its separate public CA. Restart Desktop to replace existing opaque tunnels. Preparing never grants trust.
The certificate scope is claude.ai and transport necessarily decrypts that host;
only the exact configured successful quota response is transformed. Other paths
retain official credentials, body and destination. Disabling the option
or proxy restores the official usage response.

The proxy stays reachable after rules are disabled, until Magpie exits, so a
Desktop process not yet restarted can still reach official services. Once started,
changing the listener port requires disabling and restarting Magpie first.
Magpie exit closes active tunnels; normal update/quit must wait for work to finish.
This first version does not provide seamless active-stream proxy process handover.
The generated leaf is valid for 30 days; restart Magpie before that period elapses.

## Compatibility and verification limits

The native UI and subscription entitlements remain controlled by Anthropic.
Ultra, Dynamic Workflows, tools, and any model-specific operation still depend on
the account, backend, and protocol support. This feature never advertises those
as available by forcing a flag. Remote Control is outside this feature's scope.
Global or managed Claude Code provider overrides can supersede this proxy and
must be checked during a real validation.

**Local validation (macOS 27.0.1, official Desktop 3.6556.2):** official login,
Code UI and its native Ultracode entry remain intact. A Code High request and an
Ultracode request returned successfully through the existing Team provider with
HTTP 200 and exact Opus 5.5 routing; Desktop sent High as `high` and Ultracode as
`xhigh`. Native auxiliary Haiku requests also routed successfully. Real activation
exposed and fixed CONNECT context lifetime cancellation; inner HTTP requests now
own cancellation independently of the hijacked outer CONNECT request.

A minimal official Cowork conversation returned successfully, but produced no
Magpie inference route. It must therefore be treated as official-service inference,
not a successful gateway test. This proxy does not redirect server-side Cowork
inference performed outside the local `api.anthropic.com` connection. Do not claim
Cowork gateway routing or complete feature parity based on its visible UI.

A Code Read-tool round trip against an isolated marker file also completed. One
intermediate request received an Anthropic Thread-state mismatch (400); Desktop
replayed it successfully. This remains a compatibility issue to investigate, not
a clean no-retry result. No fallback to official Code inference was used.

**Quota-only real test:** using a separately approved seven-day claude.ai test
certificate, subscribed Code displayed 0% used in both Session and Weekly windows
and kept official reset times. A minimal `QUOTA_UI_OK` turn completed through the
Magpie Team route. Temporary trust and test proxy were removed afterwards. This
does not prove behavior of a genuinely exhausted official account; that case
remains untested. The permanent implementation is gated on separate CA trust. With that trust
explicitly granted, the built-in Go proxy was also verified with both native
usage windows at zero and a real High Code response through Magpie (HTTP 200).

**Still pending before production use:** local Cowork guest inference and CA trust,
Thread-state retry behavior, tool-heavy Code turns, Dynamic Workflows and long-running tasks. The VM has a native
proxy path and host-address conversion, but a host Keychain trust result does not
prove guest trust. Windows/Linux native wiring is not supported by this version.

## Checks

Tests use generated fixture CAs and loopback servers, never real credentials or
system trust changes. `proxy_test.go` exercises real CONNECT + TLS, streaming,
credential stripping, gzip payload preservation, token counting, fallback refusal,
control-plane independence, and cancellation. Agent tests cover native mode and
profile rollback, malformed files, managed bootstrap refusal, conflicting edits,
corrupt journals and inactive third-party capabilities. GUI API tests verify
read-only status, trust gating and prepare-only semantics. `claude-login.test.cjs`
checks the real editor in both engines and all five languages, including narrow
layout and draft preservation. The routing test is also verified to fail when
selected model routing is deliberately removed via a Go source overlay.

Run `go test -race -tags nogui ./internal/claudelogin ./internal/agent ./internal/gui`
and `node --test internal/gui/tests/claude-login.test.cjs`, plus the repository's
full verification gate in `docs/code-standards.md` before release.
