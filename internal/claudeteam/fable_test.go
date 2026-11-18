package claudeteam

import (
 "context"
 "errors"
 "math"
 "slices"
 "testing"
 "time"
)

func TestFableExhaustionNeedsRealModelDemand(t *testing.T) {
 now:=time.Now();limits:=func(fable float64)[]Limit{return []Limit{{Kind:"five_hour",Used:.2,ResetsAt:now.Add(time.Hour).Unix()},{Kind:"seven_day",Used:.83,ResetsAt:now.Add(24*time.Hour).Unix()},{Kind:"seven_day_overage_included",Used:fable,ResetsAt:now.Add(24*time.Hour).Unix()}}}
 c:=Config{OrganizationID:"org",Members:[]string{"a","b","c"},Threshold:100,Routing:"order"}
 members:=[]Candidate{{ID:"a",Tier:"team_tier_1",Limits:limits(1)},{ID:"b",Tier:"unassigned",Limits:limits(1)},{ID:"c",Tier:"unassigned",Limits:limits(.21)}}
 if from,_,_:=Choose(c,members,"",nil,now);from!=""{t.Fatal("an idle Fable window moved a seat serving Opus")}
 from,to,err:=ChooseForModel(c,members,"",nil,now,"claude-fable-5-2")
 if err!=nil||from!="a"||to!="c"{t.Fatalf("Fable handoff: %s -> %s: %v",from,to,err)}
 if SpentForModel(limits(1),100,now,"claude-opus-5-5"){t.Fatal("Fable quota disabled Opus")}
 if SpentForModel(limits(.44),100,now,"claude-fable-5-2"){t.Fatal("83% global week falsely exhausted Fable")}
 members=append(members,Candidate{ID:"d",Tier:"team_tier_1",Limits:limits(.1)});c.Members=append(c.Members,"d")
 if from,_,_:=ChooseForModel(c,members,"",nil,now,"claude-fable-5-2");from!=""{t.Fatal("an existing Premium account could already serve Fable")}
}

func TestFableWindowMustBeKnownAndExpiredWindowsDoNotBlock(t *testing.T) {
 now:=time.Now();base:=[]Limit{{Kind:"five_hour",Used:0},{Kind:"seven_day",Used:0}}
 if QuotaKnownForModel(base,"claude-fable-5-2"){t.Fatal("missing Fable window treated as known")}
 for _,used:=range []float64{math.NaN(),math.Inf(1),-1,1.1}{if QuotaKnownForModel(append(base,Limit{Kind:"seven_day_overage_included",Used:used,ResetsAt:now.Add(time.Hour).Unix()}),"claude-fable-5-2"){t.Fatal("invalid Fable window treated as known")}}
 ws:=append(base,Limit{Kind:"seven_day_overage_included",Used:1,ResetsAt:now.Add(-time.Second).Unix()})
 if SpentForModel(ws,100,now,"claude-fable-5-2"){t.Fatal("elapsed weekly window blocked a reset account")}
 if !QuotaKnownForModel(append(base,Limit{Kind:"seven_day_overage_included",Used:0}),"claude-fable-5-2"){t.Fatal("unstarted zero window rejected")}
}

func TestAssignmentFailureDoesNotCycleOnAdministratorOutage(t *testing.T) {
 for _,status:=range []int{401,403,429,500,503}{e:=&TransferFailure{Outcome:"rolled_back",Failure:Failure{Phase:"assign",Status:status}};if e.TryAnother(){t.Fatalf("cycled accounts for administrator HTTP %d",status)}}
 if (&TransferFailure{Outcome:"uncertain",Failure:Failure{Phase:"assign",Status:400}}).TryAnother(){t.Fatal("unconfirmed rollback permits another write")}
}

func TestAssignmentFailureSurvivesConfirmedRollback(t *testing.T) {
 tx,admin,p,_:=transactionFixture();admin.reject="b";var journal Pending;var result Event
 tx.Record=func(v Pending)error{journal=v;return nil}
 tx.Complete=func(_ []Seat,v Pending,outcome string)error{result=Event{To:v.To,Result:outcome,Failure:v.Failure};return nil}
 err:=tx.Transfer(context.Background(),admin.seats,p);var failed *TransferFailure
 if !errors.As(err,&failed)||!failed.TryAnother(){t.Fatalf("failure swallowed: %v",err)}
 if result.Result!="rolled_back"||result.Failure==nil||result.Failure.Phase!="assign"||result.Failure.Status!=400{t.Fatalf("lost failure: %+v",result)}
 if journal.Failure==nil||journal.Failure.Status!=400{t.Fatal("failed write was not journalled")}
 if !Expected(admin.seats,"a","b","original",p.Baseline){t.Fatal("rollback was not confirmed")}
}

func TestRepeatedRolledBackTargetIsSkippedUntilCooldownExpires(t *testing.T) {
 now:=time.Now();var history []Event
 for i:=0;i<30;i++{history=append(history,Event{At:now.Add(-time.Duration(30-i)*time.Second),From:"n39",To:"n35",Result:"rolled_back"})}
 c:=Config{OrganizationID:"org",Members:[]string{"n39","n35","next"},Threshold:100,Routing:"order"}
 members:=[]Candidate{{ID:"n39",Tier:"team_tier_1",Limits:[]Limit{{Kind:"five_hour",Used:1,ResetsAt:now.Add(3*time.Hour).Unix()}}},{ID:"n35",Tier:"unassigned"},{ID:"next",Tier:"unassigned"}}
 _,to,err:=Choose(c,members,"",history,now);if err!=nil||to!="next"{t.Fatalf("reselected failed standby: %s %v",to,err)}
 if _,cool:=CoolingTarget(history,"n35",now.Add(16*time.Minute));cool{t.Fatal("cooldown never expires")}
 history=append(history,Event{At:now,To:"n35",Result:"assigned"})
 if _,cool:=CoolingTarget(history,"n35",now);cool{t.Fatal("confirmed success did not reset failure")}
 if at,n:=FailureCooldown(nil,"n35",now);n!=1||!at.Equal(now.Add(15*time.Minute)){t.Fatal(at,n)}
 if slices.Contains(PremiumSet([]Seat{{"a","unassigned"}}),"a"){t.Fatal("standby fabricated a Premium seat")}
}
