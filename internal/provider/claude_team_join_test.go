package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func teamNewLogin(n int, org string) savedLogin {
	profile, _ := json.Marshal(map[string]string{"emailAddress": teamID(n) + "@example.test", "accountUuid": teamID(n), "organizationUuid": org})
	return savedLogin{Agent: "claude", User: teamID(n) + "@example.test", Profile: profile,
		Auth: json.RawMessage(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"fixture-access-%d","refreshToken":"fixture-refresh-%d","expiresAt":9999999999999}}`, n, n))}
}

func TestClaudeTeamLoginAutomaticallyJoinsAndCaptures(t *testing.T) {
	tm, f, _ := teamFixture(t)
	tm.quotaDue = map[string]bool{}
	newClaudeTeamAdmin = func(ClaudeTeamConfig) claudeTeamAdmin {
		t.Fatal("login must not need the administrator network")
		return nil
	}
	l := teamNewLogin(5, teamOrg)
	if _, err := addLogin(l); err != nil {
		t.Fatal(err)
	}
	c, err := tm.readConfig()
	if err != nil || !slices.Contains(c.Members, teamID(5)) {
		t.Fatal("new login was not persisted in its organization", err)
	}
	v := tm.status()
	i := slices.IndexFunc(v.Members, func(m ClaudeTeamMember) bool { return m.AccountID == teamID(5) })
	if i < 0 || !v.Members[i].Selected || !v.Members[i].QuotaPending || v.Members[i].On {
		t.Fatalf("joined member must be visible, queued and closed until verified: %+v", v.Members)
	}
	if release, admitted := tm.track(l.User); admitted {
		release()
		t.Fatal("unverified new login was admitted")
	}
	old := UsageClaudeVia(func(context.Context, string) (string, error) { return teamUsageFixture(), nil })
	t.Cleanup(func() { UsageClaudeVia(old) })
	tm.collectQuotaDue(context.Background())
	if len(tm.disk.Limits[teamID(5)]) != 2 || tm.disk.Observed[teamID(5)].IsZero() {
		t.Fatal("login missed the complete quota snapshot")
	}
	if _, err := addLogin(l); err != nil {
		t.Fatal(err)
	}
	c, _ = tm.readConfig()
	if len(c.Members) != 4 || !tm.quotaDue[teamID(5)] || len(f.writes) != 0 {
		t.Fatal("re-login duplicated membership, skipped capture, or moved a seat")
	}
}

func TestClaudeTeamLoginOnlyJoinsMatchingOrganization(t *testing.T) {
	tm, _, c := teamFixture(t)
	other := c
	other.OrganizationID, other.Enabled, other.Members = teamID(70), false, nil
	l := teamNewLogin(71, other.OrganizationID)
	ls := append(readLogins(), l)
	if err := writeLogins(ls); err != nil {
		t.Fatal(err)
	}
	if err := SaveClaudeTeam(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	v, err := ClaudeTeamOrganization(other.OrganizationID)
	if err != nil || !slices.Contains(v.Config.Members, teamID(71)) {
		t.Fatal("new organization did not adopt its already saved account", err)
	}
	if _, err := addLogin(teamNewLogin(72, other.OrganizationID)); err != nil {
		t.Fatal(err)
	}
	v, _ = ClaudeTeamOrganization(other.OrganizationID)
	if len(v.Config.Members) != 2 || len(tm.status().Config.Members) != 3 {
		t.Fatal("membership crossed organizations")
	}
	if _, err := addLogin(teamNewLogin(80, teamID(79))); err != nil {
		t.Fatal(err)
	}
	if len(ClaudeTeamSubscriptions()) != 2 {
		t.Fatal("ordinary login created a Team subscription")
	}
}

func TestClaudeTeamLoginKeepsCredentialsWhenJoinCannotBeSaved(t *testing.T) {
	tm, _, _ := teamFixture(t)
	if err := os.WriteFile(tm.configPath(), []byte(`{"broken"`), 0600); err != nil {
		t.Fatal(err)
	}
	l := teamNewLogin(5, teamOrg)
	if _, err := addLogin(l); err == nil || !strings.Contains(err.Error(), "signed in, but") {
		t.Fatal("membership failure was hidden", err)
	}
	if !slices.ContainsFunc(readLogins(), func(k savedLogin) bool { return k.User == l.User }) {
		t.Fatal("membership failure discarded the successful sign-in")
	}
	if slices.Contains(tm.status().Config.Members, teamID(5)) {
		t.Fatal("failed write claimed the login was joined")
	}
}

func TestClaudeTeamOrganizationSignInChecksProfileAndJoins(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			tm, f, _ := teamFixture(t)
			org := teamOrg
			if mismatch {
				org = teamID(70)
			}
			fakeClaudeLogin(t, fakeClaudeAccount{email: "new@example.test", orgUUID: org, account: teamID(5), plan: "team", refresh: "fixture-new-oauth"}, "", false)
			st, err := StartSignInWith("claude", SignInOptions{OrganizationID: teamOrg})
			if err != nil {
				t.Fatal(err)
			}
			err = SubmitSignInCallback(st.ID, "the-code#st")
			st = waitDone(t, st.ID)
			joined := slices.Contains(tm.status().Config.Members, teamID(5))
			if mismatch {
				if err == nil || st.State != "failed" || joined || !strings.Contains(st.Error, "selected organization") {
					t.Fatalf("foreign login accepted: %s, joined=%v, error=%v", st.State, joined, err)
				}
			} else if err != nil || st.State != "done" || !joined || !tm.quotaDue[teamID(5)] {
				t.Fatalf("organization login not joined and queued: %s, joined=%v, error=%v", st.State, joined, err)
			}
			if len(f.writes) != 0 {
				t.Fatal("login changed a seat")
			}
		})
	}
}

func TestClaudeTeamExistingStandaloneLoginAdoptedOnce(t *testing.T) {
	tm, f, _ := teamFixture(t)
	l := teamNewLogin(5, teamOrg)
	if err := writeLogins(append(readLogins(), l)); err != nil {
		t.Fatal(err)
	}
	tm.quotaDue = map[string]bool{}
	if err := tm.adoptSavedLogins(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(tm.status().Config.Members, teamID(5)) || !tm.quotaDue[teamID(5)] || len(tm.quotaDue) != 1 {
		t.Fatal("existing standalone account was not adopted, or the whole pool was re-read")
	}
	// A later deliberate removal must not be undone on the next start.
	tm.cfg.Members = slices.DeleteFunc(slices.Clone(tm.cfg.Members), func(id string) bool { return id == teamID(5) })
	if err := tm.adoptSavedLogins(); err != nil || slices.Contains(tm.cfg.Members, teamID(5)) || len(f.writes) != 0 {
		t.Fatal("migration repeated or changed cloud seats", err)
	}
	var disk claudeTeamDisk
	b, err := os.ReadFile(tm.statePath())
	if err != nil || json.Unmarshal(b, &disk) != nil || !disk.MembershipInitialized {
		t.Fatal("one-time adoption was not persisted", err)
	}
}
