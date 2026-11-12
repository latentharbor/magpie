package claudeteam

import (
	"errors"
	"slices"
	"time"


)

// Candidate contains observations, never credentials. The host computes the
// router's quota weight once; the same order serves inference and standby.
type Candidate struct {
	ID, Tier string
	Lapsed, Unavailable, Reliable bool
	Limits []Limit
	Weight Weight
}

func Spent(ws []Limit, threshold float64, now time.Time) bool {
	for _, w := range ws {
		if w.Kind != "five_hour" && w.Kind != "seven_day" { continue }
		if w.Used*100 >= threshold && (w.ResetsAt == 0 || w.ResetsAt > now.Unix()) { return true }
	}
	return false
}

func Choose(c Config, members []Candidate, manual string, history []Event, now time.Time) (string,string,error) {
	from,to:="",""
	var candidates []Candidate
	var weights []Weight
	for _, s:=range members {
		if !slices.Contains(c.Members,s.ID) {continue}
		if s.Tier=="team_tier_1" && (manual!="" && manual==s.ID || manual=="" && (s.Lapsed || Spent(s.Limits,c.Threshold,now))) && from=="" {from=s.ID}
		if s.Tier!="unassigned" || s.Unavailable || s.Lapsed || Spent(s.Limits,c.Threshold,now) {continue}
		candidates=append(candidates,s);weights=append(weights,s.Weight)
	}
	var standby Candidate
	if len(candidates)>0 {
		at:=0
		if c.Routing=="" {
			order:=Order(weights);at=order[0]
			for _,i:=range order {if candidates[i].Reliable && weights[i].Known {at=i;break}}
		} else {
			at=routeOrder(c.Routing,"claude-team-"+c.OrganizationID+"/standby",weights)[0]
		}
		standby=candidates[at];to=standby.ID
	}
	if from=="" && to!="" && manual=="" && c.Routing=="" && standby.Weight.Known && standby.Reliable {
		recent:=slices.ContainsFunc(history,func(e Event)bool{return e.Result=="assigned" && now.Sub(e.At)<15*time.Minute})
		if !recent {
			var latest Weight
			for _,s:=range members {
				if s.Tier!="team_tier_1" || !slices.Contains(c.Members,s.ID) || s.Lapsed || !QuotaKnown(s.Limits) {continue}
				active:=s.Weight
				// Renewal rebalance compares expiry, independent of ordinary
				// routing's utilization bands, exactly as the standby policy.
				active.Known=true;active.Used=0
				if Order([]Weight{active,standby.Weight})[0]!=1 || !ExpiryWorthwhile(active,standby.Weight,now) {continue}
				if from=="" || Order([]Weight{active,latest})[0]==1 {from,latest=s.ID,active}
			}
		}
	}
	if manual!="" && from=="" {return "","",errors.New("the chosen source has no Premium seat in this pool")}
	if from!="" && to=="" {return from,"",errors.New("all Claude Team alternatives are cooling down, signed out or unknown; see the earliest reset")}
	return from,to,nil
}
