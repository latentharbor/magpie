package gui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func claudeTeamRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/claude-team/quota", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			OrganizationID string `json:"organizationId"`
			AccountID      string `json:"accountId"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			fail(w, err)
			return
		}
		if err := provider.RetryClaudeTeamQuota(in.OrganizationID, in.AccountID); err != nil {
			fail(w, err)
			return
		}
		v, _ := provider.ClaudeTeamOrganization(in.OrganizationID)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, v)
	})
	mux.HandleFunc("GET /api/claude-team/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, provider.ClaudeTeamSubscriptions())
	})
	for _, path := range []string{"config", "members"} {
		mux.HandleFunc("GET /api/claude-team/"+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if org := r.URL.Query().Get("organizationId"); org != "" {
				v, err := provider.ClaudeTeamOrganization(org)
				if err != nil {
					fail(w, err)
					return
				}
				writeJSON(w, v)
			} else if r.URL.Query().Get("new") == "1" {
				writeJSON(w, provider.ClaudeTeamDraft())
			} else {
				writeJSON(w, provider.ClaudeTeamStatus())
			}
		})
	}
	mux.HandleFunc("POST /api/claude-team/config", func(w http.ResponseWriter, r *http.Request) {
		var c provider.ClaudeTeamConfig
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&c); err != nil {
			fail(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := provider.SaveClaudeTeam(ctx, c); err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		v, err := provider.ClaudeTeamOrganization(c.OrganizationID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, v)
	})
	mux.HandleFunc("POST /api/claude-team/rotate", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			From           string `json:"from"`
			OrganizationID string `json:"organizationId"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			fail(w, err)
			return
		}
		rotate := func() error {
			if in.OrganizationID != "" {
				return provider.RotateClaudeTeamOrganization(in.OrganizationID, in.From)
			}
			return provider.RotateClaudeTeam(in.From)
		}
		if err := rotate(); err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		if in.OrganizationID != "" {
			v, _ := provider.ClaudeTeamOrganization(in.OrganizationID)
			writeJSON(w, v)
		} else {
			writeJSON(w, provider.ClaudeTeamStatus())
		}
	})
}
