package provider

// The Team control plane only edits existing members' Premium assignments.
// Inference continues through the genuine Claude Code subscription bridge.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var claudeTeamUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$`)

// The members web endpoint rejects Go's default agent (403). This web-client
// header was checked with a real sessionKey-only GET; no Desktop transport
// or clearance cookie is needed for the verified request.
const claudeTeamWebAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

type claudeTeamSeat struct {
	AccountID string `json:"accountId"`
	SeatTier  string `json:"seatTier"`
}

type claudeTeamAdmin interface {
	Members(context.Context) ([]claudeTeamSeat, error)
	SetTier(context.Context, string, string) error
}

type claudeTeamHTTP struct {
	org, key string
	base     string
	client   *http.Client
}

var newClaudeTeamAdmin = func(c ClaudeTeamConfig) claudeTeamAdmin {
	return &claudeTeamHTTP{org: c.OrganizationID, key: c.SessionKey, base: "https://claude.ai", client: &http.Client{
		Transport:     http.DefaultTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *claudeTeamHTTP) call(ctx context.Context, method, path string, payload any, dst any) error {
	if !claudeTeamUUID.MatchString(c.org) {
		return errors.New("invalid Claude Team organization UUID")
	}
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/organizations/"+c.org+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("could not construct Claude Team request")
	}
	req.AddCookie(&http.Cookie{Name: "sessionKey", Value: c.key})
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", claudeTeamWebAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Referer", "https://claude.ai/admin-settings/members")
	req.Header.Set("anthropic-client-platform", "web_claude_ai")
	req.Header.Set("anthropic-client-version", "1.0.0")
	res, err := c.client.Do(req)
	if err != nil {
		// Never expose request headers, cookies, response bodies or transport errors.
		return errors.New("Claude Team request did not complete; its result is unknown")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("Claude Team administration returned HTTP %d; check the administrator session and web verification", res.StatusCode)
	}
	if dst == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 || json.Unmarshal(b, dst) != nil {
		return errors.New("Claude Team members response is unreadable; assignments were not changed")
	}
	return nil
}

func (c *claudeTeamHTTP) Members(ctx context.Context) ([]claudeTeamSeat, error) {
	var out []claudeTeamSeat
	seen := map[string]bool{}
	for page := range 100 {
		var raw json.RawMessage
		path := "/members_v2?offset=" + strconv.Itoa(page*15) + "&limit=15&types%5B%5D=member"
		if err := c.call(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}
		var rows []json.RawMessage
		var envelope struct {
			Members, Data, Items []json.RawMessage
			More                 *bool `json:"has_more"`
			Pagination           struct {
				More *bool `json:"has_more"`
			}
		}
		if json.Unmarshal(raw, &rows) != nil {
			if json.Unmarshal(raw, &envelope) != nil {
				return nil, errors.New("Claude Team members format changed")
			}
			switch {
			case envelope.Members != nil:
				rows = envelope.Members
			case envelope.Data != nil:
				rows = envelope.Data
			case envelope.Items != nil:
				rows = envelope.Items
			default:
				return nil, errors.New("Claude Team members format changed")
			}
		}
		before := len(out)
		for _, raw := range rows {
			var row struct {
				Member        json.RawMessage
				AccountUUID   string `json:"account_uuid"`
				AccountID     string `json:"account_id"`
				UUID, ID      string
				SeatTier      string `json:"seat_tier"`
				Account, User struct{ UUID, ID string }
			}
			if json.Unmarshal(raw, &row) != nil {
				return nil, errors.New("Claude Team member is unreadable")
			}
			if len(row.Member) > 0 && string(row.Member) != "null" {
				if json.Unmarshal(row.Member, &row) != nil {
					return nil, errors.New("Claude Team member is unreadable")
				}
			}
			id := ""
			for _, v := range []string{row.AccountUUID, row.AccountID, row.Account.UUID, row.Account.ID, row.User.UUID, row.User.ID, row.UUID, row.ID} {
				if v != "" {
					id = strings.ToLower(v)
					break
				}
			}
			if !claudeTeamUUID.MatchString(id) || row.SeatTier == "" || seen[id] {
				return nil, errors.New("Claude Team member IDs or seat tiers are ambiguous")
			}
			seen[id] = true
			out = append(out, claudeTeamSeat{AccountID: id, SeatTier: row.SeatTier})
		}
		more := envelope.More
		if more == nil {
			more = envelope.Pagination.More
		}
		if more != nil && !*more || more == nil && len(rows) < 15 {
			return out, nil
		}
		if len(out) == before {
			return nil, errors.New("Claude Team pagination did not advance")
		}
	}
	return nil, errors.New("Claude Team member list exceeds the safety limit")
}

func (c *claudeTeamHTTP) SetTier(ctx context.Context, id, tier string) error {
	if !claudeTeamUUID.MatchString(id) || !slices.Contains([]string{"unassigned", "team_tier_1"}, tier) {
		return errors.New("only existing members' Premium assignments can be changed")
	}
	return c.call(ctx, http.MethodPut, "/members/"+url.PathEscape(id), map[string]string{"seat_tier": tier}, nil)
}
