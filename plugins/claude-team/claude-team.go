// Package claudeteam owns the Team management UI. The background engine is
// independent of the GUI; CLI and Docker use the same provider host adapter.
package claudeteam

import (
	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/provider"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID: "claude-team", API: "^1", Requires: []string{"window", "providers", "gateway"},
		Name: "Claude Team", Sub: "Manage organization seats and standby accounts",
		Opens: "providers", One: "1 organization", Many: "{n} organizations",
		InUse:  provider.ClaudeTeamConfiguredCount,
		Parts:  []pluginrt.Part{{Slot: "part", Key: "claude-team"}},
		Client: []string{"i18n.js", "client.js", "team.css"},
	})
}
