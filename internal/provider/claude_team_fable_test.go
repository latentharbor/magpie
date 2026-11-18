package provider

import (
 "context"
 "encoding/json"
 "errors"
 "slices"
 "strings"
 "testing"
 "time"
)

func teamFableLimits(five,fable float64) []ClaudeLimit {
 now:=time.Now()
 return []ClaudeLimit{{Kind:"five_hour",Used:five,ResetsAt:now.Add(3*time.Hour).Unix()},{Kind:"seven_day",Used:.83,ResetsAt:now.Add(3*24*time.Hour).Unix()},{Kind:"seven_day_overage_included",Used:fable,ResetsAt:now.Add(3*24*time.Hour).Unix()}}
}

func TestClaudeTeamFableDemandMovesOnlyABlockedModel(t *testing.T) {
 tm,f,c:=teamFixture(t)
 old:=UsageClaudeVia(func(context.Context,string)(string,error){return "",errors.New("fixture quota unavailable")});t.Cleanup(func(){UsageClaudeVia(old)})
 f.seats[2].SeatTier="unassigned"
 if err:=tm.synchronize(c,f.seats);err!=nil{t.Fatal(err)}
 for n,used:=range map[int]float64{2:1,3:1,4:.21}{tm.note(tm.userOf(teamID(n)),teamFableLimits(.2,used))}
 if err:=tm.reconcile(context.Background(),"");err!=nil{t.Fatal(err)}
 if len(f.writes)!=0 {t.Fatal("idle Fable exhaustion moved an Opus-capable seat")}
 if ObserveClaudeTeamModel("u2@example.com · Team","claude-opus-5-5"){t.Fatal("Fable cap exhausted Opus")}
 if !ObserveClaudeTeamModel("u2@example.com · Team","claude-fable-5-2"){t.Fatal("real Fable demand did not report exhaustion")}
 if err:=tm.reconcile(context.Background(),"");err!=nil{t.Fatal(err)}
 if !slices.Equal(f.writes,[]string{teamID(2)+":unassigned",teamID(4)+":team_tier_1"}){t.Fatalf("Fable picked another Fable-full account: %v",f.writes)}
 e:=tm.disk.History[0];if e.Reason!="model_exhausted"||e.Model!="claude-fable-5-2"{t.Fatalf("missing model decision: %+v",e)}
 if tm.disk.Limits[teamID(2)][1].Used!=.83{t.Fatal("fabricated global weekly exhaustion")}
}

func TestClaudeTeamFailedTargetCoolsAcrossRestartAndHistoryEviction(t *testing.T) {
 tm,f,c:=teamFixture(t)
 old:=UsageClaudeVia(func(context.Context,string)(string,error){return "",errors.New("fixture quota unavailable")});t.Cleanup(func(){UsageClaudeVia(old)})
 f.seats[2].SeatTier="unassigned";f.failTarget=true
 if err:=tm.synchronize(c,f.seats);err!=nil{t.Fatal(err)}
 for _,n:=range []int{2,3,4}{five:=.1;if n==2{five=1};tm.note(tm.userOf(teamID(n)),teamFableLimits(five,.2))}
 if err:=tm.reconcile(context.Background(),"");err!=nil{t.Fatal(err)}
 want:=[]string{teamID(2)+":unassigned",teamID(3)+":team_tier_1",teamID(2)+":team_tier_1",teamID(2)+":unassigned",teamID(4)+":team_tier_1"}
 if !slices.Equal(f.writes,want){t.Fatalf("did not recover and try the next target: %v",f.writes)}
 failed:=tm.disk.History[0]
 if failed.Result!="rolled_back"||failed.Failure==nil||failed.Failure.Status!=400||failed.Failure.Phase!="assign"||!failed.RetryAt.After(time.Now()){t.Fatalf("missing safe failure/cooldown: %+v",failed)}
 for range 31 {tm.disk.History=append(tm.disk.History,ClaudeTeamEvent{At:time.Now(),To:teamID(4),Result:"assigned"})}
 tm.disk.History=slices.Clone(tm.disk.History[len(tm.disk.History)-30:])
 if err:=tm.persist();err!=nil{t.Fatal(err)}
 next:=newClaudeTeamController();next.cfg=c
 if err:=next.loadState(c);err!=nil{t.Fatal(err)}
 view:=next.status();found:=false
 for _,m:=range view.Members{if m.AccountID==teamID(3){found=true;if m.RetryAt==nil||m.HandoffFailure==nil||m.HandoffFailure.Status!=400||m.State!="handoff_cooling"{t.Fatalf("restart lost failure: %+v",m)}}}
 if !found{t.Fatal("failed member disappeared")}
 b,_:=json.Marshal(view);if strings.Contains(string(b),"fixture-private-body")||strings.Contains(string(b),c.SessionKey){t.Fatal("vendor body or session key leaked")}
 if err:=tm.reconcile(context.Background(),"");err!=nil{t.Fatal(err)}
 if !slices.Equal(f.writes,want){t.Fatal("cooled target was retried")}
}
