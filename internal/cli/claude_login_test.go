package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/claudelogin"
)

func TestClaudeLoginHelpAndTrustGate(t *testing.T) {
	home := helpHome(t)
	before := tree(t, home)
	output, err := runOut(t, "claude-login", "--help")
	if err != nil || !strings.Contains(output, "magpie claude-login") {
		t.Fatalf("%s %v", output, err)
	}
	after := tree(t, home)
	if len(after) != len(before) {
		t.Fatal("help changed files")
	}
	if _, err := runOut(t, "claude-login", "on"); err == nil {
		t.Fatal("untrusted CA enabled")
	}
	if _, err := os.Stat(claudelogin.Path()); !os.IsNotExist(err) {
		t.Fatal("failed enable wrote config")
	}
	if _, err := runOut(t, "claude-login", "map", "claude-opus-5-5", "group/opus"); err != nil {
		t.Fatal(err)
	}
	c, err := claudelogin.Load()
	if err != nil || c.Enabled || c.Models["claude-opus-5-5"] != "group/opus" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := runOut(t, "claude-login", "unmap", "claude-opus-5-5"); err != nil {
		t.Fatal(err)
	}
	c, err = claudelogin.Load()
	if err != nil || len(c.Models) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestClaudeLoginUsageChoicePreservesMappings(t *testing.T) {
	helpHome(t)
	if _, err := runOut(t, "claude-login", "map", "claude-opus-5-5", "group/opus"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"gateway", "official"} {
		if _, err := runOut(t, "claude-login", "usage", mode); err != nil {
			t.Fatal(err)
		}
		c, err := claudelogin.Load()
		if err != nil || c.GatewayUsage != (mode == "gateway") || c.Enabled || c.Models["claude-opus-5-5"] != "group/opus" {
			t.Fatalf("choice lost configuration: %+v %v", c, err)
		}
	}
}
