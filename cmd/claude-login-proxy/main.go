package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/yetone/magpie/internal/claudeproxy"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("claude-login-proxy", flag.ContinueOnError)
	path := fs.String("config", claudeproxy.DefaultPath(), "private configuration file")
	from := fs.String("from", "", "existing Magpie configuration directory")
	gateway := fs.String("gateway", "", "NAS gateway origin")
	keyFile := fs.String("key-file", "-", "device key file, or - for stdin")
	provider := fs.String("provider", "", "Claude provider ID on NAS")
	organization := fs.String("organization", "", "official Desktop usage organization UUID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "version":
		fmt.Println(version)
		return nil
	case "serve":
		return claudeproxy.Run(ctx, *path, version)
	case "status":
		v, err := claudeproxy.Status(ctx, *path)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(v)
	case "import-magpie":
		return claudeproxy.ImportMagpie(*from, *path)
	case "certificates":
		v, err := claudeproxy.Certificates(*path)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(v)
	case "preferences":
		v, err := claudeproxy.ReadPreferences(*path)
		if err != nil { return err }
		return json.NewEncoder(os.Stdout).Encode(v)
	case "save-preferences":
		var p claudeproxy.Preferences
		if err := decodeInput(&p); err != nil { return err }
		v, err := claudeproxy.SavePreferences(ctx, *path, p)
		if err != nil { return err }
		return json.NewEncoder(os.Stdout).Encode(v)
	case "probe-server":
		var p claudeproxy.ServerPreference
		if err := decodeInput(&p); err != nil { return err }
		v, err := claudeproxy.ProbeServer(ctx, *path, p)
		if err != nil { return err }
		return json.NewEncoder(os.Stdout).Encode(v)
	case "init":
		var b []byte
		var err error
		if *keyFile == "-" {
			b, err = io.ReadAll(io.LimitReader(os.Stdin, 4097))
		} else {
			b, err = os.ReadFile(*keyFile)
		}
		if err != nil || len(b) > 4096 {
			return fmt.Errorf("cannot read device key")
		}
		return claudeproxy.Initialize(ctx, *path, *gateway, strings.TrimSpace(string(b)), *provider, *organization)
	case "mcp-image":
		return claudeproxy.ImageMCP(ctx, *path, os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("commands: serve, status, version, import-magpie, init, certificates, mcp-image, preferences, save-preferences, probe-server")
	}
}

func decodeInput(v any) error {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
	if err != nil || len(b) > 1<<20 || json.Unmarshal(b, v) != nil {
		return fmt.Errorf("invalid settings input")
	}
	return nil
}
