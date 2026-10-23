# Native Claude Team controller (custom build)

This opt-in local fork exposes one normal subscription provider per Team
organization (`claude-team-{organization UUID}`), managing existing Premium
assignments in the background. Each subscription has its own routing, model,
fallback and on/off settings. The genuine Claude Code bridge still handles
inference, streaming and tool results. No request invokes Python,
Desktop, a quota endpoint, or an administrator check before inference.

## Configuration and ownership

`claude-team.json` beside the provider configuration holds `enabled`,
`organizationId`, `name`, `routing`, the administrator's `sessionKey`, selected
saved account UUIDs in `members`, and `threshold` (default 100). The original
organization keeps these legacy paths without rewriting its quota journal;
additional organizations use `claude-teams/{UUID}/{config,state}.json`.
Routing picks in `providers.json` override the configuration's initial policy.
Configuration and journals are written atomically
with mode 0600. GET APIs never return the credential; a blank key on save
retains it only within the same organization. OAuth inference credentials
remain in the existing Magpie login store. Desktop is not read.

Account and organization UUIDs come from saved OAuth profiles, not a manually
maintained email-to-Desktop mapping. Ambiguous, missing and cross-organization
identities are rejected. Only selected members' switches and seat tiers are
managed. Premium members outside the pool are never assigned or released.
The number of selected Premium members determines the active seat count;
activation does not purchase or allocate seats.

`claude-team-state.json` (0600) keeps observed allowance windows, original
login switches, last confirmed pool seat assignments, recent operations and an unfinished-transfer journal. A
corrupt/unreadable journal stops automatic writes and is never overwritten
as an empty state. Removing a member or disabling control restores its
original switches, not its credentials or previous cloud seat assignment.
An unfinished transfer permits an administrator credential renewal, but not
pool or organization edits that would invalidate its recovery plan.

## Background lifecycle

The gateway owns one controller per organization, each protected by an operation
lock and a cross-process file lock. Credentials, quotas, drain counters,
recovery plans and members never cross organizations. A corrupt additional
configuration is quarantined rather than returned to standalone Claude routing.
`NoteClaudeLimits` records actual Claude Code
5-hour and weekly usage and reset timestamps. Global windows reaching the
configured threshold trigger reconciliation; model-specific windows remain
visible but do not revoke a seat serving other models. `OnRenewed` and a
five-minute timer also trigger reconciliation. Healthy requests never cause
an administrator read. A failed administrator read preserves known switches.
Confirmed assignments are persisted and restored on restart without requiring
an administrator round trip. An unfinished transfer blocks cached admissions
until reconciliation. Legacy journals without assignments retain only enabled,
controller-owned saved logins for inference, never fabricating verified Premium
tiers. Disabled, parked, paused, revoked, removed and foreign-organization logins
are not restored as inference candidates. A successful complete member read
replaces this compatibility state with authoritative assignments.

Each successful member snapshot enables only Premium accounts in the pool.
A controller-owned `teamOff` bit prevents discovery, the agent's current
login and its stand-in fallback from resurrecting an unassigned member.
These switches take effect without restarting Magpie or Claude Desktop.
The controller does not change the agent's own OAuth login.

A transfer closes new admissions for its source, waits for that source's
active gateway requests to finish, verifies the original Premium set, journals
the intended transaction, releases the source, verifies the free seat, assigns
the replacement, and verifies the complete Premium set again. Other Premium
accounts keep serving while the source drains. A lost PUT response is followed
by a read, not a blind retry. Recovery recognizes already-assigned, unchanged,
or released-but-still-free states; only the last may restore the old seat.
Concurrent changes, including changes outside the pool, stop automatic writes.
A transfer accepted by the controller has a bounded independent context even
if the initiating GUI request disconnects.

Candidates with known remaining global usage are preferred. Accounts whose
recorded blocking windows elapsed are eligible probes, not fabricated zero
usage. The normal routing strategy also chooses standby replacements: Smart
uses the largest/weekly renewal first, then the 5-hour renewal when tied to the
hour; explicit in-order, rotation and other policies retain their semantics.
There is no preferred/current-CLI-login account or account-switch action in the
Team UI. An unknown reset on a known exhausted account is not assumed reset.
Quota reads for unassigned or draining accounts show only last-observed
windows and an explicit stale-reading warning; they do not invoke inference
or fabricate zero usage when a recorded reset passes.
5-hour and weekly windows are combined: an account is back only after every
blocking window has reset. Revoked/expired credentials are not automatically
cleared merely because an account receives a Premium seat.

The persisted snapshots feed ordinary account quota displays and routing even
in held/read-only mode before the generic quota cache warms up. Unassigned
accounts do not call `/usage`; legacy snapshots with no observation timestamp
do not invent one from the journal's modification time. After a verified
assignment, the controller refreshes the new account through the existing saved
Claude Code `/usage` command, bounded by 20 seconds, retaining credential-refresh
locking and persistence. This happens after handoff waiters are released; an
error retains the snapshot and never gates healthy inference. Disabling or
removing a provider suppresses automatic seat movement, without changing its
cloud assignments.

## Requests during handoff

Ordinary inference only updates a local in-flight counter. A stale plan that
still names a draining account is rejected locally before launching inference.
A failed managed Claude request whose output has not been committed can wait
for the current bounded handoff, re-resolve that provider's enabled accounts,
and continue with its original body. The final candidate's refusal is held
too. Visible text, reasoning and tool calls are not replayed. Account/model
scopes, pinned/strict affinity and sealed conversation ownership still apply.

Pending requests are in memory, not a disk spool of private prompts. At most
128 callers wait for a current handoff; each wait is bounded by two minutes
and the caller's cancellation. No request waits hours for quota renewal.
The member view exposes the earliest known reset. Stream keepalives use the
gateway's existing held-response machinery, including new requests arriving
while every selected account is temporarily closed. Early errors after
keepalive headers terminate the response with the protocol's stream error.

Only requests owned by this gateway can be drained. Independent Claude Code
processes and external admin changes are outside its control; do not run a
second automatic seat controller on the same pool.

## GUI and API

Providers → Add → Claude Team, or the Claude account editor's Claude Team
button, opens the organization settings. Add may create multiple subscriptions;
each appears in the ordinary provider list and uses its standard editor,
with Team-owned account rows and no Use / Make first controls. Organization
forms reuse the native modal chrome but own their internal form/section layout,
including narrow windows and long account names. They accept an organization UUID and an
administrator sessionKey, selects existing saved accounts, shows Premium /
Unassigned, seat counts, real quota snapshots and resets, and recent changes.
Manual rotation posts a background request; status polling makes no cloud
calls. The API is `GET /api/claude-team/subscriptions`,
`GET /api/claude-team/{config|members}?organizationId={UUID}` (or `?new=1` for
saved public identities),
`POST /api/claude-team/config`, and `POST /api/claude-team/rotate`.
Rotate includes `organizationId` and `from`; legacy no-organization requests
address the original controller. The original provider's `claude/...` model
alias and gateway-key model/account restrictions remain scoped to the original
organization, not automatically granted to new organizations.

The administrator transport is native Go: verified web member-list endpoint
`GET /api/organizations/{org}/members_v2` with pagination, and
`PUT /api/organizations/{org}/members/{account}` with
`seat_tier: unassigned | team_tier_1`. The web-client User-Agent is required
by the verified endpoint: a real sessionKey-only GET returned 403 with Go's
default User-Agent and succeeded with that header; no clearance cookie or
Desktop transport was needed at that time; a web-client User-Agent does not
guarantee admission. Responses carrying `cf-mitigated: challenge` are reported
as Cloudflare browser verification, not as expired OAuth or a missing model.
Completing verification in a browser does not guarantee a separate HTTP
transport is cleared; replacing the sessionKey alone may not resolve it.
Redirects are refused, response bodies
and credentials are never included in errors. Web-session expiry or a web
verification challenge is an operator-visible error, not a reason to disable
healthy inference. An empty Team route returns 503 with its control-plane
failure, not a model-not-found 404; known exhausted pools still return 429
with their earliest reset. Cached routes never authorize a cloud seat mutation:
every transfer still requires fresh complete member verification.

## Verification and deployment

Build and distribution instructions are in [Distribution builds and
updates](../fork-distribution.md). The application retains its normal name
and configuration locations. Do not operate another automatic controller
on the same pool.

Run tests under an isolated HOME with pinned Go caches. Provider tests cover
switching, draining, multiple seats and organizations, outside-pool protection,
lost responses, rollback, recovery and credential redaction. Gateway tests
traverse the actual HTTP handler stack with fake inference and administration.
Browser fixtures exercise the form in Chromium and WebKit in five languages.
These checks do not themselves prove live administration or Desktop migration.

Before production activation, separately verify administration and controlled
Unassigned/Premium behavior with explicit operator authorization. A generic
403 is not proof that an account lacks a seat, and must not clear genuine
credential-revocation marks. Never publish local credentials, member mappings,
private validation logs or recovery journals as distribution assets.
