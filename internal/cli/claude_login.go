package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/claudelogin"
)

const claudeLoginUsage = `magpie claude-login — keep Claude's official login (experimental)
  magpie claude-login                  configuration, listener and public CA status
  magpie claude-login prepare          generate a private local, host-restricted CA; does not install trust
  magpie claude-login map <exact Claude model> <provider/model|group/id>
  magpie claude-login unmap <exact Claude model>
  magpie claude-login on|off           enable/disable selected inference routing
  magpie claude-login usage-organization <UUID|none>
  magpie claude-login prepare-usage    prepare separate claude.ai quota CA; does not install trust
  magpie claude-login usage gateway|official  choose local Code quota handling

Only api.anthropic.com /v1/messages and /v1/messages/count_tokens are routed.
Optional gateway usage answers local Code quota checks; all Code models then
need mappings. Official subscription and OAuth checks remain unchanged.
The official app and its login aren't patched. Trust the generated public CA
before enabling this mode. Desktop's native egress proxy is wired reversibly.
Restart Desktop after enabling/disabling. No hosts or system proxy edits.`

func claudeLoginCmd(args []string) error {
	if len(args) == 0 {
		return showClaudeLogin()
	}
	if len(args) == 1 && (args[0] == "prepare" || args[0] == "prepare-usage") {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		prepare := claudelogin.EnsureAuthority
		dir := claudelogin.CertDir()
		if args[0] == "prepare-usage" {
			prepare = claudelogin.EnsureWebAuthority
			dir = claudelogin.WebCertDir()
		}
		if _, err := prepare(ctx, dir); err != nil {
			return err
		}
		fmt.Println("Public CA:", filepath.Join(dir, "ca.pem"))
		fmt.Println("Not installed or trusted. The private authority remains on this computer.")
		return showClaudeLogin()
	}
	c, err := claudelogin.Load()
	if err != nil {
		return err
	}
	switch {
	case len(args) == 2 && args[0] == "usage-organization":
		c.UsageOrganization = args[1]
		if args[1] == "none" {
			c.UsageOrganization = ""
		}
	case len(args) == 3 && args[0] == "map":
		if c.Models == nil {
			c.Models = map[string]string{}
		}
		c.Models[args[1]] = args[2]
	case len(args) == 2 && args[0] == "unmap":
		delete(c.Models, args[1])
	case len(args) == 1 && args[0] == "on":
		if !claudelogin.ReadCertificate().Trusted {
			return fmt.Errorf("trust this installation's public CA before enabling Claude login routing; magpie claude-login prepare")
		}
		c.Enabled = true
	case len(args) == 1 && args[0] == "off":
		c.Enabled = false
	case len(args) == 2 && args[0] == "usage" && (args[1] == "gateway" || args[1] == "official"):
		c.GatewayUsage = args[1] == "gateway"
	default:
		return fmt.Errorf("%s", claudeLoginUsage)
	}
	if err := agent.SetDesktopLogin(c); err != nil {
		return err
	}
	return showClaudeLogin()
}

func showClaudeLogin() error {
	c, err := claudelogin.Load()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		Config           claudelogin.Config            `json:"config"`
		Certificate      claudelogin.CertificateStatus `json:"certificate"`
		UsageCertificate claudelogin.CertificateStatus `json:"usageCertificate"`
	}{c, claudelogin.ReadCertificate(), claudelogin.ReadWebCertificate()}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
