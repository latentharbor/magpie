package provider

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestClaudeTeamRemovedMembersDoNotBlockAdministratorRenewal(t *testing.T) {
	tm, admin, c := teamFixture(t)
	tm.note("u4@example.com · Team", []ClaudeLimit{{Kind: "seven_day", Used: .4, ResetsAt: time.Now().Add(time.Hour).Unix()}})
	admin.mu.Lock()
	admin.seats = slices.DeleteFunc(admin.seats, func(s claudeTeamSeat) bool { return s.AccountID == teamID(3) || s.AccountID == teamID(4) })
	admin.mu.Unlock()
	v, err := CreateClaudeTeam(context.Background(), "fixture-renewed-administrator", c.OrganizationID)
	if err != nil {
		t.Fatal("removed saved members prevented administrator renewal", err)
	}
	saved, err := tm.readConfig()
	if err != nil || saved.SessionKey != "fixture-renewed-administrator" || !slices.Equal(saved.Members, c.Members) {
		t.Fatal("renewal lost the credential or the saved account pool")
	}
	for _, id := range []string{teamID(3), teamID(4)} {
		i := slices.IndexFunc(v.Members, func(m ClaudeTeamMember) bool { return m.AccountID == id })
		if i < 0 || !v.Members[i].Selected || v.Members[i].State != "absent" || v.Members[i].On || v.Members[i].SeatTier != "absent" {
			t.Fatal("removed account was lost, admitted, or described as Unassigned")
		}
	}
	if v.Seats != 1 || v.InUse != 1 {
		t.Fatal("removed Premium remained in the usable seat count", v.Seats, v.InUse)
	}
	if release, ok := tm.track("u4@example.com · Team"); ok {
		release()
		t.Fatal("stale inference plan admitted a removed member")
	}
	if release, ok := tm.track("u2@example.com · Team"); !ok {
		t.Fatal("a removed member blocked the remaining Premium")
	} else {
		release()
	}
	if len(tm.disk.Limits[teamID(4)]) != 1 || tm.disk.Limits[teamID(4)][0].Used != .4 || tm.disk.Observed[teamID(4)].IsZero() {
		t.Fatal("removal discarded historical usage")
	}
	for _, l := range readLogins() {
		id, _ := savedSessionIdentity(l)
		if (id.AccountID == teamID(3) || id.AccountID == teamID(4)) && (l.On || !l.TeamOff || len(l.Auth) == 0) {
			t.Fatal("removed member lost credentials or retained inference switches")
		}
	}
	admin.mu.Lock()
	writes := len(admin.writes)
	admin.mu.Unlock()
	if writes != 0 || tm.disk.Seats[teamID(4)] != "absent" {
		t.Fatal("renewing the administrator wrote a seat or failed to persist absence")
	}
	// Rejoining is authoritative membership, not a fabricated new account.
	admin.mu.Lock()
	admin.seats = append(admin.seats, claudeTeamSeat{AccountID: teamID(4), SeatTier: "team_tier_1"})
	seats := slices.Clone(admin.seats)
	admin.mu.Unlock()
	if err := tm.synchronize(c, seats); err != nil {
		t.Fatal(err)
	}
	if release, ok := tm.track("u4@example.com · Team"); !ok {
		t.Fatal("confirmed rejoin did not restore admission")
	} else {
		release()
	}
}

func TestClaudeTeamVerifiedPremiumCountIsDynamic(t *testing.T) {
	for _, count := range []int{1, 3, 5, 8} {
		t.Run(teamID(count), func(t *testing.T) {
			tm, admin, c := teamFixture(t)
			ls := readLogins()
			seats := []claudeTeamSeat{{AccountID: teamID(2), SeatTier: "team_tier_1"}, {AccountID: teamID(3), SeatTier: "unassigned"}, {AccountID: teamID(4), SeatTier: "unassigned"}}
			for n := 0; n < count-1; n++ {
				id := teamID(20 + n)
				profile, _ := json.Marshal(map[string]string{"emailAddress": id + "@example.test", "accountUuid": id, "organizationUuid": c.OrganizationID})
				l := ls[0]
				l.ID, l.User, l.Profile = "", id+"@example.test · Team", profile
				ls = append(ls, l)
				c.Members = append(c.Members, id)
				seats = append(seats, claudeTeamSeat{AccountID: id, SeatTier: "team_tier_1"})
			}
			// An account outside the pool must never inflate its capacity.
			seats = append(seats, claudeTeamSeat{AccountID: teamID(99), SeatTier: "team_tier_1"})
			if err := writeLogins(ls); err != nil {
				t.Fatal(err)
			}
			admin.mu.Lock()
			admin.seats = seats
			admin.mu.Unlock()
			if err := tm.save(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			v := tm.status()
			if v.Seats != count || v.InUse != count {
				t.Fatal("verified Premium count was fixed or included an outside member", count, v.Seats, v.InUse)
			}
			if len(admin.writes) != 0 {
				t.Fatal("discovering Premium capacity allocated a seat")
			}
		})
	}
}
