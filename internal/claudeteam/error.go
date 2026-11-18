package claudeteam

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/lazyre"
)

var claudeTeamRayID = lazyre.New(`^[0-9a-fA-F]{16}-[A-Z]{3}$`)
var claudeTeamRequestID = lazyre.New(`^(req_[a-zA-Z0-9]{8,64}|[0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12})$`)

type AdminError struct {
	Status    int
	message   string
	Kind      string
	RetryDate string
}

func (e *AdminError) Error() string { return e.message }

// Only fixed classifications and validated correlation IDs leave this function.
// A web error can contain credentials or account information, including in a
// JSON message, HTML title, redirect or an arbitrarily named vendor error type.
func ResponseError(method string, res *http.Response) error {
	const limit = 64 << 10
	body, readErr := io.ReadAll(io.LimitReader(res.Body, limit+1))
	complete := readErr == nil && len(body) <= limit
	if len(body) > limit {
		body = body[:limit]
	}
	format := "other"
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(res.Header.Get("Content-Type"), ";")[0]))
	switch contentType {
	case "application/json":
		format = "JSON"
	case "text/html":
		format = "HTML"
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	kind := ""
	if complete && json.Unmarshal(body, &envelope) == nil {
		format = "JSON"
		kind = envelope.Error.Type
		if kind == "" {
			kind = envelope.Type
		}
	}
	// cf-ray and Server: cloudflare occur on ordinary responses too. They
	// identify the edge, not a challenge. Require a challenge header or both
	// known challenge-page markers before attributing a failure to Cloudflare.
	lower := strings.ToLower(string(body))
	challenge := strings.EqualFold(res.Header.Get("cf-mitigated"), "challenge") ||
		(format == "HTML" && strings.Contains(lower, "/cdn-cgi/challenge-platform/") &&
			(strings.Contains(lower, "<title>just a moment") || strings.Contains(lower, "<title>attention required")))
	reason := "request rejected; cause is unclassified"
	code, retryDate := "", ""
	switch {
	case challenge:
		reason = "Cloudflare browser verification required; seat rotation is paused and existing inference accounts are retained. A new sessionKey alone may not resolve this challenge"
	case res.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(envelope.Error.Message), "weekly limit on moving this seat type between people"):
		code = "seat_movement_limit"
		reason = "organization weekly seat-movement limit reached; inference allowance does not grant another seat move. Existing assignments and inference credentials are retained"
		const prefix = "You can try again after "
		if at := strings.Index(envelope.Error.Message, prefix); at >= 0 {
			tail := envelope.Error.Message[at+len(prefix):]
			if len(tail) >= 16 && tail[10:16] == " (UTC)" {
				if date, err := time.Parse("2006-01-02", tail[:10]); err == nil && date.Format("2006-01-02") == tail[:10] {
					retryDate = tail[:10]
					reason += "; retry after " + retryDate + " (UTC)"
				}
			}
		}
	case res.StatusCode == http.StatusUnauthorized || kind == "authentication_error" ||
		res.StatusCode == http.StatusForbidden && kind == "permission_error" && strings.EqualFold(strings.TrimSpace(envelope.Error.Message), "Invalid authorization"):
		reason = "administrator authentication rejected; renew the administrator session"
	case kind == "permission_error":
		reason = "administrator permission denied; check organization management access"
	case res.StatusCode == http.StatusForbidden:
		reason = "unclassified forbidden response; neither a browser challenge nor a session failure was confirmed"
	case res.StatusCode == http.StatusTooManyRequests:
		reason = "administration rate limited; this does not identify an inference quota limit"
	}
	operation := "administration"
	switch method {
	case http.MethodGet:
		operation = "member list"
	case http.MethodPut:
		operation = "seat update"
	case http.MethodPost:
		operation = "member invitation"
	case http.MethodDelete:
		operation = "member removal"
	}
	detail := fmt.Sprintf("HTTP %d, %s, response=%s", res.StatusCode, operation, format)
	if !complete {
		detail += ", body incomplete"
	}
	if id := res.Header.Get("request-id"); claudeTeamRequestID.MatchString(id) {
		detail += ", request-id=" + id
	} else if id := res.Header.Get("x-request-id"); claudeTeamRequestID.MatchString(id) {
		detail += ", request-id=" + id
	}
	if id := res.Header.Get("cf-ray"); claudeTeamRayID.MatchString(id) {
		detail += ", cf-ray=" + id
	}
	return &AdminError{Status: res.StatusCode, message: fmt.Sprintf("Claude Team %s (%s)", reason, detail), Kind: code, RetryDate: retryDate}
}
