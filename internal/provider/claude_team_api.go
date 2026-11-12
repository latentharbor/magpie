package provider

import (
	"context"
	"github.com/yetone/magpie/internal/claudeteam"
	"net/http"
)

// This adapter supplies the provider transport. Seat protocol and validation
// belong to the standalone Team module; it never receives inference tokens.
var claudeTeamUUID = claudeteam.UUID

const claudeTeamWebAgent = claudeteam.WebAgent

type claudeTeamSeat = claudeteam.Seat
type claudeTeamAdmin = claudeteam.Admin
type claudeTeamHTTP struct {
	org, key, base string
	client         *http.Client
}

func newClaudeTeamHTTP(c ClaudeTeamConfig) *claudeTeamHTTP {
	h := claudeteam.NewHTTP(c)
	return &claudeTeamHTTP{org: h.Org, key: h.Key, base: h.Base, client: h.Client}
}

var newClaudeTeamAdmin = func(c ClaudeTeamConfig) claudeTeamAdmin { return newClaudeTeamHTTP(c) }

func (c *claudeTeamHTTP) backend() *claudeteam.HTTP {
	return &claudeteam.HTTP{Org: c.org, Key: c.key, Base: c.base, Client: c.client}
}
func (c *claudeTeamHTTP) Members(ctx context.Context) ([]claudeTeamSeat, error) {
	return c.backend().Members(ctx)
}
func (c *claudeTeamHTTP) SetTier(ctx context.Context, id, tier string) error {
	return c.backend().SetTier(ctx, id, tier)
}
func (c *claudeTeamHTTP) request(ctx context.Context, method, path string, payload, dst any) error {
	return c.backend().Request(ctx, method, path, payload, dst)
}
