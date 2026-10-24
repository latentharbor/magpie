package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/claudelogin"
)

func claudeLoginRoutes(mux *http.ServeMux) {
	read := func(w http.ResponseWriter, r *http.Request) {
		c, err := claudelogin.Load()
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"config": c, "runtime": claudelogin.ReadStatus(), "certificate": claudelogin.ReadCertificate(), "usageCertificate": claudelogin.ReadWebCertificate(), "wired": agent.DesktopLoginWired(), "supported": runtime.GOOS == "darwin"})
	}
	mux.HandleFunc("GET /api/claude-login", read)
	mux.HandleFunc("POST /api/claude-login/prepare", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if _, err := claudelogin.EnsureAuthority(ctx, claudelogin.CertDir()); err != nil {
			fail(w, err)
			return
		}
		read(w, r)
	})
	mux.HandleFunc("POST /api/claude-login/prepare-usage", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if _, err := claudelogin.EnsureWebAuthority(ctx, claudelogin.WebCertDir()); err != nil {
			fail(w, err)
			return
		}
		read(w, r)
	})
	mux.HandleFunc("POST /api/claude-login", func(w http.ResponseWriter, r *http.Request) {
		var next claudelogin.Config
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&next); err != nil {
			fail(w, err)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			fail(w, fmt.Errorf("expected a single Claude login configuration"))
			return
		}
		if err := next.Validate(); err != nil {
			fail(w, err)
			return
		}
		if err := agent.SetDesktopLogin(next); err != nil {
			fail(w, err)
			return
		}
		read(w, r)
	})
}
