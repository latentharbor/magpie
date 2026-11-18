package claudeteam

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type invitationFixture struct {
	t *testing.T
	seats []Seat
	writes []string
	pending Pending
	completed, restored, ready int
	accept, lostReply bool
}

func (f *invitationFixture) Members(context.Context) ([]Seat, error) { return slices.Clone(f.seats), nil }
func (f *invitationFixture) SetTier(_ context.Context, id, tier string) error {
	if f.pending.Stage != "releasing" || id != "a" || tier != "unassigned" { f.t.Fatal("release was not journaled or targeted the wrong member") }
	f.writes = append(f.writes, "release:a")
	for i := range f.seats { if f.seats[i].AccountID == id { f.seats[i].SeatTier = tier } }
	return nil
}
func (f *invitationFixture) RemoveMember(_ context.Context, id string) error {
	if f.pending.Stage != "removing" || id != "a" { f.t.Fatal("removal was not journaled or removed the target instead of the source") }
	f.writes = append(f.writes, "remove:a")
	f.seats = slices.DeleteFunc(f.seats, func(s Seat) bool { return s.AccountID == id })
	return nil
}
func (f *invitationFixture) InvitePremium(_ context.Context, email string) error {
	if f.pending.Stage != "inviting" || email != "replacement@example.test" { f.t.Fatal("invitation was not journaled or used another saved identity") }
	f.writes = append(f.writes, "invite:b")
	if f.accept { f.seats = append(f.seats, Seat{AccountID:"b", SeatTier:"team_tier_1", Role:"user"}) }
	if f.lostReply { return errors.New("fixture lost invitation reply") }
	return nil
}

func invitationTest(t *testing.T) (*invitationFixture, InvitationTransaction, Pending) {
	t.Helper()
	f := &invitationFixture{t:t, seats:[]Seat{{AccountID:"a",SeatTier:"team_tier_1",Role:"user"},{AccountID:"outside",SeatTier:"team_tier_1",Role:"user"}}}
	c := Config{OrganizationID:"org", Members:[]string{"a","b"}, ReplacementMode:WeeklyInvitation, Threshold:100}
	p := Pending{Kind:WeeklyInvitation, OrganizationID:c.OrganizationID,From:"a",To:"b",Baseline:PremiumSet(f.seats),InviteEmail:"replacement@example.test"}
	tx := InvitationTransaction{Membership:f, Transaction:Transaction{Config:c, Admin:f,
		Ready:func(string) error { f.ready++; return nil },
		Close:func(string,string) error { return nil }, Drain:func(context.Context,string) error{return nil},
		Restore:func([]Seat)error{f.restored++;return nil},
		Record:func(p Pending)error{f.pending=p;return nil},
		Complete:func(_ []Seat,_ Pending,result string)error{if result!="assigned"{t.Fatal("unexpected completion",result)};f.completed++;return nil},
	}}
	return f,tx,p
}

func TestInvitationRemovesOnlySourceAndWaitsWithoutRepeatingWrites(t *testing.T) {
	f, tx, p := invitationTest(t)
	if err := tx.Transfer(context.Background(), f.seats, p); !errors.Is(err, ErrInvitationPending) { t.Fatal("unaccepted invitation was enabled",err) }
	if !slices.Equal(f.writes,[]string{"release:a","remove:a","invite:b"}) || f.completed!=0 || f.ready!=0 { t.Fatal("incorrect source/remove/invite order",f.writes) }
	for range 50 {
		if err:=tx.Recover(context.Background(),f.pending); !errors.Is(err,ErrInvitationPending) { t.Fatal(err) }
	}
	if len(f.writes)!=3 || f.restored!=51 { t.Fatal("polling repeated a cloud write or failed to retain other Premium accounts") }
	f.seats=append(f.seats,Seat{AccountID:"b",SeatTier:"team_tier_1",Role:"user"})
	if err:=tx.Recover(context.Background(),f.pending);err!=nil{t.Fatal(err)}
	if f.completed!=1 || f.ready!=1 || len(f.writes)!=3 { t.Fatal("verified acceptance did not complete through credential readiness") }
}

func TestInvitationLostReplyCannotRepeatInviteOrRestoreRemovedSource(t *testing.T) {
	f, tx, p := invitationTest(t); f.lostReply=true
	if err:=tx.Transfer(context.Background(),f.seats,p);err==nil{t.Fatal("uncertain invitation was reported successful")}
	if f.pending.Failure==nil || f.pending.Failure.Phase!="invite" {t.Fatal("lost reply was not retained")}
	for range 50 { _=tx.Recover(context.Background(),f.pending) }
	if len(f.writes)!=3 || f.completed!=0 {t.Fatal("uncertain POST was retried or removed source was upgraded")}
	f.seats=append(f.seats,Seat{AccountID:"b",SeatTier:"team_tier_1",Role:"user"})
	if err:=tx.Recover(context.Background(),f.pending);err!=nil || f.completed!=1{t.Fatal("later authoritative acceptance could not resolve a lost reply",err)}
}

func TestInvitationRefusesPresentTargetOwnerAndInvalidIdentity(t *testing.T) {
	for _, scenario:=range []string{"existing target","owner","unknown role","invalid email","foreign organization","outside capacity changed"} {
		t.Run(scenario,func(t *testing.T){
			f,tx,p:=invitationTest(t)
			switch scenario {
			case "existing target":f.seats=append(f.seats,Seat{AccountID:"b",SeatTier:"unassigned",Role:"user"})
			case "owner":f.seats[0].Role="primary_owner"
			case "unknown role":f.seats[0].Role=""
			case "invalid email":p.InviteEmail="Another person <other@example.test>"
			case "foreign organization":p.OrganizationID="another"
			case "outside capacity changed":f.seats=append(f.seats,Seat{AccountID:"another",SeatTier:"team_tier_1",Role:"user"})
			}
			if err:=tx.Transfer(context.Background(),f.seats,p);err==nil || len(f.writes)!=0{t.Fatal("unsafe invitation made a cloud write",scenario)}
		})
	}
}

func TestWeeklyInvitationWaitsForWholeDynamicPremiumBatch(t *testing.T) {
	now:=time.Now()
	for _, count:=range []int{1,3,5,8} {
		c:=Config{Threshold:100};var seats []Seat;limits:=map[string][]Limit{}
		for n:=range count {
			id:=strings.Repeat("a",n+1);c.Members=append(c.Members,id);seats=append(seats,Seat{AccountID:id,SeatTier:"team_tier_1"})
			limits[id]=[]Limit{{Kind:"five_hour",Used:1,ResetsAt:now.Add(time.Hour).Unix()},{Kind:"seven_day",Used:1,ResetsAt:now.Add(24*time.Hour).Unix()},{Kind:"seven_day_overage_included",Used:1,ResetsAt:now.Add(24*time.Hour).Unix()}}
		}
		// Unselected Premium capacity does not hold up the selected cohort.
		seats=append(seats,Seat{AccountID:"outside",SeatTier:"team_tier_1"})
		if len(WeeklyBatch(c,seats,limits,now))!=count{t.Fatal("batch size was fixed",count)}
		limits[c.Members[0]][1].Used=.5
		if len(WeeklyBatch(c,seats,limits,now))!=0{t.Fatal("5h/Fable exhaustion replaced members with a usable global week")}
		limits[c.Members[0]][1].Used=1;limits[c.Members[0]][1].ResetsAt=now.Add(-time.Minute).Unix()
		if len(WeeklyBatch(c,seats,limits,now))!=0{t.Fatal("expired weekly observation evicted a reset member")}
		delete(limits,c.Members[0])
		if len(WeeklyBatch(c,seats,limits,now))!=0{t.Fatal("unknown weekly usage was treated as exhausted")}
	}
}

func TestInvitationHTTPUsesCapturedPayloadAndDisablesSeatPurchase(t *testing.T) {
	const org="00000000-0000-4000-8000-000000000001"
	const source="00000000-0000-4000-8000-000000000002"
	var calls []string
	var mu sync.Mutex
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		mu.Lock()
		calls=append(calls,r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method==http.MethodPost {
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body)!=nil || string(body["allow_seat_purchase"])!="false" {t.Error("invitation could purchase seats")}
			var invites []map[string]string
			if json.Unmarshal(body["invites"],&invites)!=nil || len(invites)!=1 || invites[0]["email_address"]!="replacement@example.test" || invites[0]["role"]!="user" || invites[0]["seat_tier"]!="team_tier_1" || invites[0]["invite_source"]!="admin_manual" || invites[0]["invite_surface"]!="members_page" {t.Error("invitation changed captured request semantics")}
		} else if r.Method!=http.MethodDelete {t.Error("unexpected mutation",r.Method)}
		w.WriteHeader(http.StatusNoContent)
	}));defer server.Close()
	client:=&HTTP{Org:org,Key:"fixture",Base:server.URL,Client:server.Client()}
	if err:=client.RemoveMember(context.Background(),source);err!=nil{t.Fatal(err)}
	if err:=client.InvitePremium(context.Background(),"replacement@example.test");err!=nil{t.Fatal(err)}
	mu.Lock()
	defer mu.Unlock()
	if len(calls)!=2 || calls[0]!="DELETE /api/organizations/"+org+"/members/"+source || calls[1]!="POST /api/organizations/"+org+"/invites/bulk" {t.Fatal(calls)}
	if err:=client.InvitePremium(context.Background(),"not an email");err==nil || len(calls)!=2{t.Fatal("invalid identity reached network")}
}
