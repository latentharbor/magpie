# Standalone Claude proxy client

`cmd/claude-login-proxy` is a separate executable. `internal/claudeproxy` uses the
existing restricted TLS implementation in `internal/claudelogin` without importing
the provider registry, inference gateway, GUI, account discovery or Team runtime.
CI checks that dependency boundary before building the macOS archives.

`build/claude-login-proxy/macos` is a native SwiftUI settings application, separate
from both the daemon and Magpie GUI. Its bundled helper supplies `preferences`,
`save-preferences` and `probe-server` over JSON stdin/stdout, with no HTTP admin
port. Saved credentials are never returned. Saves use a file lock, an optimistic
revision check, immutable new credential files and an atomic backed-up config
replacement. Blank keys preserve saved credentials; unknown configuration fields
are retained. Connection checks use `/v1/models`, never inference, and do not
follow redirects with credentials. Native editor model state and light/dark page
rendering, plus settings/runtime regressions, run in `claude-proxy-app.yml`.

The application manages servers, exact model mappings, gateway usage display,
the image upstream, certificates and launchd service state. Listener addresses
remain read-only because Desktop also references them. Saving these settings
affects subsequent requests without restarting. Installing/updating the service
is an explicit action; updates require idle and retain configuration and trust.
Closing the settings window leaves the service running. New configurations
prepare restricted certificates but trust remains an explicit Keychain action.

The source of truth is `~/.config/claude-login-proxy/config.json`, schema 1. Exact
native model names map to `upstream-id/remote-model`. Each upstream owns an origin
and a private key file; no subscription credentials live here. `import-magpie`
copies remote mappings, device keys and the existing private certificate bundles
into this directory transactionally. It leaves the source intact. `init` prepares
a new client and generates certificates, but neither command installs trust.

The daemon serves the native CONNECT proxy and an optional loopback image-only
compatibility relay. Only its restricted API/usage hosts are decrypted. Selected
inference uses its remote device key and never falls back to official inference.
Quota responses express gateway-managed local availability; actual limits and
seat ownership remain on NAS. The certificate cache renews 30-day leaves with
the same constrained CA, after validating that authority and system trust again.

A private Unix status socket reports readiness and active outbound requests.
An admission gate rejects new work during shutdown and drains active response
bodies for at most 50 seconds before cancellation. The LaunchAgent owns no GUI;
MCP processes are started separately by their clients using the same executable.

`mcp-image` forwards newline-delimited JSON-RPC to NAS Streamable HTTP, retaining
session/protocol headers, request IDs and server events. Local reference image
paths are encoded only when named in a call. Images returned by NAS can be saved
locally, without overwriting existing files; pending generation IDs retain their
requested destinations. A local save failure keeps the generated result and
tells the client to use it instead of generating it again. Model selection and
image jobs remain NAS responsibilities.

The installer backs up every changed file, waits for idle, stops only the local
Magpie GUI gateway and disables its old login proxy/autostart. It reuses an active
Desktop proxy profile when the endpoint already matches, preserving official
login state, and redirects the existing image MCP entry. Configuration rollback
restores only JSON fields still owned by the installation. Certificates are
copied on the same Mac and no system trust setting is changed.

Verification is the dedicated `claude-login-proxy.yml` workflow: dependency and
vet checks, Linux/macOS proxy and MCP cases under the race detector, a negative
drain mutation, Python installer syntax, and signed arm64/amd64 archives. Full
Magpie regression is not the release gate for this independent client.
