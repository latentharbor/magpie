// Package claudeteam coordinates existing Team seats independently of a
// provider's inference transport, credentials, GUI and account store.
package claudeteam

import "time"

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
	Model string `json:",omitempty"`
	Failure *Failure `json:",omitempty"`
	RecoveryFailure *Failure `json:",omitempty"`
}

// Failure contains only a fixed diagnostic or an already-sanitized AdminError.
// Raw transport errors and vendor bodies never enter the journal or member UI.
type Failure struct {
 Phase string `json:"phase"`
 Status int `json:"status,omitempty"`
 Message string `json:"message"`
}

type Event struct {
	At         time.Time `json:"at"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Result     string    `json:"result"`
	Reason     string    `json:"reason,omitempty"`
	FromLimits []Limit   `json:"fromLimits,omitempty"`
	ToLimits   []Limit   `json:"toLimits,omitempty"`
	Model string `json:"model,omitempty"`
	Failure *Failure `json:"failure,omitempty"`
	RecoveryFailure *Failure `json:"recoveryFailure,omitempty"`
	RetryAt time.Time `json:"retryAt,omitempty"`
	Failures int `json:"failures,omitempty"`
}
