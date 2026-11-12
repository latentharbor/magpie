package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type ClaudeTeamOrganizationInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Discovery only reads the administrator's organizations. It neither saves the
// session nor allocates a seat; a second explicit request creates the provider.
var readClaudeTeamOrganizations = func(ctx context.Context, key string) ([]ClaudeTeamOrganizationInfo, error) {
	c := newClaudeTeamHTTP(ClaudeTeamConfig{SessionKey: key})
	return c.organizations(ctx)
}

func (c *claudeTeamHTTP) organizations(ctx context.Context) ([]ClaudeTeamOrganizationInfo, error) {
	var rows []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/organizations", nil, &rows); err != nil {
		return nil, err
	}
	out := []ClaudeTeamOrganizationInfo{}
	for _, row := range rows {
		if claudeTeamUUID.MatchString(row.UUID) {
			out = append(out, ClaudeTeamOrganizationInfo{strings.ToLower(row.UUID), row.Name})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no organizations found for this administrator session")
	}
	return out, nil
}

func DiscoverClaudeTeams(ctx context.Context, key string) ([]ClaudeTeamOrganizationInfo, error) {
	c := ClaudeTeamConfig{OrganizationID: teamSessionOrg, SessionKey: key}
	if err := validateClaudeTeam(&c); err != nil {
		return nil, err
	}
	if c.SessionKey == "" {
		return nil, errors.New("paste the administrator sessionKey value")
	}
	return readClaudeTeamOrganizations(ctx, c.SessionKey)
}

// A valid placeholder is used only to validate a session before discovery.
const teamSessionOrg = "00000000-0000-4000-8000-000000000000"

func CreateClaudeTeam(ctx context.Context, key, org string) (ClaudeTeamView, error) {
	c := ClaudeTeamConfig{OrganizationID: org, SessionKey: key, Threshold: 100}
	if err := validateClaudeTeam(&c); err != nil {
		return ClaudeTeamView{}, err
	}
	if c.SessionKey == "" {
		return ClaudeTeamView{}, errors.New("paste the administrator sessionKey value")
	}
	// Verify access even with an empty pool. Never infer permissions from an
	// organization UUID pasted into the form.
	if _, err := newClaudeTeamAdmin(c).Members(ctx); err != nil {
		return ClaudeTeamView{}, err
	}
	if t, err := claudeTeamForOrg(c.OrganizationID, false); err == nil {
		old, err := t.readConfig()
		if err != nil {
			return ClaudeTeamView{}, err
		}
		c = old // re-adding keeps the pool, routing and rotation preference
		c.SessionKey = key
	}
	if err := SaveClaudeTeam(ctx, c); err != nil {
		return ClaudeTeamView{}, err
	}
	if err := ShowAccount(claudeTeamPrefix + c.OrganizationID); err != nil {
		return ClaudeTeamView{}, err
	}
	forgetAccountCaches()
	return ClaudeTeamOrganization(c.OrganizationID)
}
