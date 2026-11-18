// Package claudeteam coordinates existing Team seats independently of a
// provider's inference transport, credentials, GUI and account store.
package claudeteam

import (
	"fmt"
	"time"
)

type Config struct {
	Name           string   `json:"name,omitempty"`
	Routing        string   `json:"routing,omitempty"`
	Enabled        bool     `json:"enabled"`
	OrganizationID string   `json:"organizationId"`
	SessionKey     string   `json:"sessionKey,omitempty"`
	Members        []string `json:"members"`
	Threshold      float64  `json:"threshold"`
}

type Limit struct {
	Kind     string  `json:"kind"`
	Used     float64 `json:"used"`
	ResetsAt int64   `json:"resetsAt"`
}

// Pending keeps the existing journal shape so an interrupted transfer from
// an older installation is recovered before any new assignment is attempted.
type Pending struct {
	OrganizationID       string `json:"organizationId"`
	From, To             string
	Baseline             []string
	Reason               string
	FromLimits, ToLimits []Limit
	Model                string   `json:",omitempty"`
	Failure              *Failure `json:",omitempty"`
	RecoveryFailure      *Failure `json:",omitempty"`
}

// Failure contains only a fixed diagnostic or an already-sanitized AdminError.
// Raw transport errors and vendor bodies never enter the journal or member UI.
type Failure struct {
	Phase     string `json:"phase"`
	Status    int    `json:"status,omitempty"`
	Message   string `json:"message"`
	Kind      string `json:"kind,omitempty"`
	RetryDate string `json:"retryDate,omitempty"`
}

// A vendor seat-movement cap is separate from inference allowance. A date
// without a clock is kept as a UTC date, never presented as a known instant.
type MovementLimit struct {
	RetryDate string `json:"retryDate,omitempty"`
	Seats     int    `json:"seats"`
}

type AutomaticPause struct {
	Until   int64    `json:"until,omitempty"`
	Seats   int      `json:"seats"`
	Reason  string   `json:"reason,omitempty"`
	Failure *Failure `json:"failure,omitempty"`
}

func (p AutomaticPause) Active(now time.Time) bool { return p.Until <= 0 || now.Unix() < p.Until }
func (p AutomaticPause) Error() string {
	return "Claude Team automatic seat transfer paused after a failed handoff and rollback; another inference refusal cannot revoke and re-upgrade the same account again. Existing Premium accounts remain available. Inspect the failure and explicitly retry rotation if appropriate"
}

func (m MovementLimit) Active(now time.Time) bool {
	date, err := time.Parse("2006-01-02", m.RetryDate)
	return err != nil || now.UTC().Before(date.AddDate(0, 0, 1))
}

func (m MovementLimit) Error() string {
	if m.RetryDate != "" {
		return fmt.Sprintf("Claude Team organization weekly seat-movement limit reached; automatic seat changes paused until after %s (UTC). Current Premium accounts can continue serving their available models; refresh after a capacity change or explicitly retry a rotation", m.RetryDate)
	}
	return "Claude Team organization weekly seat-movement limit reached; automatic seat changes paused. Current Premium accounts remain available; refresh after a capacity change or explicitly retry a rotation"
}

type Event struct {
	At              time.Time `json:"at"`
	From            string    `json:"from"`
	To              string    `json:"to"`
	Result          string    `json:"result"`
	Reason          string    `json:"reason,omitempty"`
	FromLimits      []Limit   `json:"fromLimits,omitempty"`
	ToLimits        []Limit   `json:"toLimits,omitempty"`
	Model           string    `json:"model,omitempty"`
	Failure         *Failure  `json:"failure,omitempty"`
	RecoveryFailure *Failure  `json:"recoveryFailure,omitempty"`
	RetryAt         time.Time `json:"retryAt,omitempty"`
	Failures        int       `json:"failures,omitempty"`
}
