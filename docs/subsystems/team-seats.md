# Native Claude Team controller (custom build)

This opt-in local fork exposes one normal subscription provider per Team
organization (`claude-team-{organization UUID}`), managing existing Premium
assignments in the background. Each subscription has its own routing, model,
fallback and on/off settings. The genuine Claude Code bridge still handles
inference, streaming and tool results. No request invokes Python or Desktop.
Ordinary admission consults neither a quota endpoint nor an administrator API.

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

Administrator failures report the HTTP status, operation and response format.
Only `cf-mitigated: challenge` or recognized challenge-page markers identify
Cloudflare verification; an ordinary 403 remains unclassified. A 401 or explicit
authentication error identifies rejected authentication, while an explicit
permission error identifies management access denial. Neither `cf-ray` nor the
server header alone proves a challenge. Bounded error-body inspection never
exposes response text, credentials or arbitrary vendor error types; only validated
request IDs and edge correlation IDs are included. Diagnostics add no retries or
assignment writes, and an administrator read failure preserves inference state.

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
usage. The provider routing choice also chooses standby replacements. Team Smart
spends the earliest weekly allowance first, then the 5-hour allowance when the
weekly resets tie to the hour. Any account below the configured exhaustion
threshold remains eligible: ordinary Smart's 90/98% bands do not push a
nearly-expired, still-usable week behind a fresh week. The same order routes
inference among multiple active Premium members; a genuinely full applicable
window (including model-specific windows) still goes behind usable accounts.
This changes neither other subscriptions' Smart nor explicit routing picks.
Complete usable snapshots
precede partial/unknown observations; unknown members are probes only when no
known usable standby exists. Elapsed windows are eligible estimates without
overwriting observations. An elapsed 5-hour window has zero estimated usage
and no running deadline until the next use starts it; routing uses five hours
from now only as a prospective bound. Weekly windows, including model-specific
ones, advance from their observed reset on a fixed seven-day cadence, even
after several missed periods. Both standby selection and inference use that
anchored deadline, rather than seven days from the time of selection.

Background Smart reconciliation also replaces a healthy active member when a
known usable standby has an earlier expiry. It replaces the latest-expiring
selected Premium member first, preserving other seats and outside-pool members.
Equal reset hours retain the current assignment; unknown snapshots never evict a
healthy member. Every voluntary transfer uses the same admission close, drain,
fresh member verification and transaction recovery as exhaustion handoff. No
request performs an administrator check. Explicit in-order, rotation and other
policies retain their semantics and do not trigger voluntary expiry rebalancing.
There is no preferred/current-CLI-login account or account-switch action in the
Team UI. An unknown reset on a known exhausted account is not assumed reset.
Quota reads for unassigned or draining accounts do not invoke inference.
Displays project elapsed windows to zero with an explicit reset-based estimate
notice; this is never described as a fresh vendor reading. The Team member API
retains raw `limits` beside projected `windows`, and the private journal and
observation time remain unchanged. A missing reset does not clear positive
usage. A real successful reading replaces the estimate.
5-hour and weekly windows are combined: an account is back only after every
blocking window has reset. Revoked/expired credentials are not automatically
cleared merely because an account receives a Premium seat.

A confirmed sign-out of a selected Premium member also triggers replacement,
even when its quota is usable or its previous window has reset. Explicit native
or HTTP credential revocation, and an authoritative credential store cleared by
Claude Code, close stale admissions and wake this organization's controller.
The same drain, verified seat transaction and request handoff apply. Existing
persisted sign-outs are recognized at startup and periodic reconciliation.
Temporary refresh contention, network/read failures and a refusal of an older
credential do not revoke the current credential or move its seat. If no usable
standby exists, authentication failure remains an authentication failure, never
a fabricated allowance reset.

The persisted snapshots feed ordinary account quota displays and routing even
in held/read-only mode before the generic quota cache warms up. Unassigned
accounts do not call `/usage` for ordinary display; legacy snapshots with no
observation timestamp do not invent one from the journal's modification time.
Successful login of a selected member, adding a member to the pool, and an
observed Premium assignment (including changes made on the web) queue an immediate
background read through the existing saved Claude Code `/usage` command. Each
read is bounded by 20 seconds and retains credential-refresh locking. It saves
both global windows and reset times; a zero-use window may have no reset when
not yet started. Partial or failed reads preserve the last complete snapshot
and observation time, even after Unassigned, with a per-member error and retry.
Reads and retries never allocate or rotate seats. External assignments are
detected by the ordinary five-minute reconciliation, not a browser webhook.
An allowance read that finishes after a newer inference observation cannot
replace that observation in either the controller journal or the account card.
This applies to background captures and explicit card refreshes, so an older
low-use response cannot undo newly reported exhaustion during handoff. A new
read started after that observation can still replace it, including clearing
exhaustion after a real reset. Publication is serialized; network reads remain
outside that lock.
Fresh captures precede automatic rotation decisions; captures following an
internal transfer run after handoff waiters are released. Errors never gate
healthy inference. Disabling or
removing a provider suppresses automatic seat movement, without changing its
cloud assignments.

## Requests during handoff

Ordinary inference only updates a local in-flight counter. A stale plan that
still names a draining account is rejected locally before launching inference.
A failed managed Claude request whose output has not been committed can wait
for the current bounded handoff, re-resolve that provider's enabled accounts,
and continue with its original body, including a confirmed authentication loss.
The final candidate's refusal is held
too. Visible text, reasoning and tool calls are not replayed. Account/model
scopes, pinned/strict affinity and sealed conversation ownership still apply.

An ambiguous 429 on this uncommitted failure path rechecks the failed account's
saved-login `/usage`, bounded by 20 seconds. Concurrent failures share that
check, with a 30-second cooldown after either success or failure. Confirmed
global exhaustion enters the normal background handoff; a failed/partial
check retains the previous snapshot and does not invent exhaustion or move a
seat. A generic rate limit without confirmed exhaustion keeps the gateway's
bounded short-term retry policy, honoring `Retry-After` and request/token
reset headers. Unified 5-hour/weekly allowance deadlines do not suppress that
short-term retry. Normal successful requests still perform no quota read.

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

An existing organization's ordinary account list and its Team settings both
offer Add another Claude account at the bottom. They run the existing Claude
Code browser sign-in with an organization UUID constraint, retained through
retry and proxy changes. A foreign profile fails before being saved. Successful
login or file import automatically joins the matching configured organization,
persists membership locally, queues quota collection and returns the editor to
that organization. Joining does not need an administrator network request or
allocate a seat; a new member stays closed until its seat is verified. A quota
failure retains prior data and the existing retry action. A local membership
write failure reports that sign-in succeeded but joining failed, retaining the
credential rather than pretending the account was joined.

Creating an organization also adopts matching accounts already saved. On the
first controller run after upgrade, matching standalone accounts are adopted
once, recorded by `membershipInitialized` in the private journal. Later pool
exclusions survive restart; explicitly signing the account in again joins it
again. Only newly joined members are queued, not the entire saved pool.
Refreshing an open form incorporates automatic membership without overwriting
checkbox choices the user changed in that form.

Providers → Add → Claude Team first accepts the administrator session, discovers
its organizations, and creates the selected provider after verifying member-list
access. If discovery fails, an explicit organization UUID is offered with the
same access check. Discovery does not persist credentials or change seats.
The new provider appears even with no saved accounts and initially has rotation
disabled; its ordinary editor is where accounts and Team settings are managed.
Re-adding an existing hidden organization preserves its pool and settings.
Add may create multiple subscriptions; each uses its standard editor,
with Team-owned account rows and no Use / Make first controls. Organization
forms reuse the native modal chrome but own their internal form/section layout,
including narrow windows and long account names. They accept an organization UUID and an
administrator sessionKey, selects existing saved accounts, shows Premium /
Unassigned, seat counts, real quota snapshots and resets, and recent changes.
Claude sign-in completion reloads the local provider catalog without waiting for
unrelated vendors' model fetches, and invalidates account caches after automatic
Team membership has been saved. Open account lists poll local snapshots faster
while quota collection is pending, retaining the prior view on a read failure.
Manual rotation posts a background request; status polling makes no cloud
calls. The API is `GET /api/claude-team/subscriptions`,
`GET /api/claude-team/{config|members}?organizationId={UUID}` (or `?new=1` for
saved public identities),
`POST /api/claude-team/discover` (`sessionKey`),
`POST /api/claude-team/create` (`sessionKey`, `organizationId`),
`POST /api/claude-team/config`, `POST /api/claude-team/rotate`, and
`POST /api/claude-team/quota` (`organizationId`, `accountId`) for a background
read-only retry. The editor polls pending collections and shows failures while
retaining the last successful values; saving with pending/failed collection
keeps the editor open.
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
on the same pool. During an update handover the replacement controller waits
for the previous process's exclusive file lock instead of exiting permanently
after a single failed attempt. Waiting is cancellable and does not perform
administration or quota collection. After acquiring ownership it reloads the
configuration and recovery journal from disk. Filesystem/locking errors are
reported separately from ordinary contention; the lock file is never deleted.

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
