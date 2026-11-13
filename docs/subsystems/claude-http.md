# Optional Claude HTTP transport

This distribution retains an opt-in transport alongside the upstream Claude
Code bridge. `settings.claudeTransport` is a gateway-wide setting for Claude
subscriptions, exposed in their provider preferences. `cli` uses the upstream
bridge and its separate interactive/print-mode setting; `http` uses
`internal/gateway/claude_http*.go`. The chosen transport is captured when a
request enters the gateway, so a settings save does not redirect a running turn.

The HTTP adapter uses the selected saved account through
`provider.ClaudeHTTPAuth`. Native credential storage and refresh locking remain
owned by the provider; Team supplies only eligibility and admission/draining.
The account's OAuth credential is sent to the fixed Anthropic API origin only,
with redirects refused. Management credentials never enter this path.

Recognized native callers retain their message/tool structure, valid signed
thinking, explicit effort and cache policy. Unsigned historical thinking is
removed from an outgoing copy before generic normalization. The diagnostic
counts those repairs without saving their text. Explicit 5m/1h cache markers
remain caller choices. This path creates no nested Claude Code conversation.

Native Thread ownership and cache touches use `internal/claudeanchor` and the
`claude_thread_*` gateway helpers. A missing or moved Thread keeps its recoverable
status/code, allowing the caller to replay its complete history. The gateway
never invents a complete replay from a delta. Thread error details are limited
to the recognized recovery code; unrelated upstream diagnostics are not copied.

The retained compatibility profile is based on the earlier 2.1.309/2.1.310
offline captures in gateway testdata. Keeping it here does not establish that
every newer CLI, Desktop build or server behaves identically, nor that quota
consumption matches another transport. Updating that profile is separate from
this structural migration.

GitHub CI runs the existing `TestClaudeHTTP*`, `TestClaudeThread*`, Team handoff,
credential refresh and UI setting fixtures. Those checks use local fake servers
on the runners; they do not establish real-account consumption equivalence.
The [Desktop login proxy](claude-login.md) is a separate optional entry point
and can route to either transport.
