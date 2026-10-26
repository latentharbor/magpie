package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var claudeTeamRayID = regexp.MustCompile(`^[0-9a-fA-F]{16}-[A-Z]{3}$`)
var claudeTeamRequestID = regexp.MustCompile(`^(req_[a-zA-Z0-9]{8,64}|[0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12})$`)

// Only fixed classifications and validated correlation IDs leave this function.
// A web error can contain credentials or account information, including in a
// JSON message, HTML title, redirect or an arbitrarily named vendor error type.
func claudeTeamResponseError(method string, res *http.Response) error {
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
			Type string `json:"type"`
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
	switch {
	case challenge:
		reason = "Cloudflare browser verification required; seat rotation is paused and existing inference accounts are retained. A new sessionKey alone may not resolve this challenge"
	case res.StatusCode == http.StatusUnauthorized || kind == "authentication_error":
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
	return fmt.Errorf("Claude Team %s (%s)", reason, detail)
}
