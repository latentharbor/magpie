package claudeteam

import (
 "errors"
 "fmt"
 "time"
)

var errFailureJournal=errors.New("Claude Team failure journal could not be saved; automatic writes stopped")
func targetFailure(e Event) bool {
 return e.Result=="rolled_back"&&e.Failure==nil || e.Failure!=nil&&(e.Failure.Phase=="assign"||e.Failure.Phase=="confirm_assignment"||e.Failure.Phase=="recovery")
}

// TransferFailure means assignments were safely reconciled, but the requested
// handoff did not succeed. A successful rollback is never a successful handoff.
type TransferFailure struct { Outcome string; Failure Failure }
func (e *TransferFailure) Error() string { return fmt.Sprintf("Claude Team handoff %s: %s (%s)",e.Outcome,e.Failure.Message,e.Failure.Phase) }
func (e *TransferFailure) TryAnother() bool {
 return (e.Outcome=="rolled_back"||e.Outcome=="unchanged") &&
  (e.Failure.Phase=="assign"||e.Failure.Phase=="confirm_assignment") &&
  (e.Failure.Status==0||e.Failure.Status==400||e.Failure.Status==404||e.Failure.Status==409||e.Failure.Status==422)
}
func safeFailure(phase string,err error) *Failure {
 f:=&Failure{Phase:phase,Message:"seat update result could not be confirmed"}
 var remote *AdminError
 if errors.As(err,&remote) { f.Status=remote.Status;f.Message=remote.Error() }
 return f
}

// Failed targets rest longer than the five-minute background pass. Repeated
// failure backs off to one hour; a confirmed assignment resets this history.
func FailureCooldown(history []Event,target string,now time.Time) (time.Time,int) {
 n:=1
 for i:=len(history)-1;i>=0;i-- {
  e:=history[i];if e.To!=target { continue };if e.Result=="assigned" { break }
  if targetFailure(e) {
   if e.Failures>0 { n=min(e.Failures,999)+1;break };n=min(n+1,1000)
  }
 }
 delay:=15*time.Minute<<min(n-1,2)
 return now.Add(delay),n
}
func CoolingTarget(history []Event,target string,now time.Time) (Event,bool) {
 for i:=len(history)-1;i>=0;i-- {
  e:=history[i];if e.To!=target { continue };if e.Result=="assigned" { return Event{},false }
  if !targetFailure(e) {continue}
  at:=e.RetryAt;if at.IsZero(){at=e.At.Add(15*time.Minute)}
  if now.Before(at){e.RetryAt=at;return e,true};return Event{},false
 }
 return Event{},false
}
