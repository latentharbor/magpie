package claudelogin

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// RemoteTarget is supplied by the host's configured Remote magpie provider.
// Its key is an inference key for that gateway, never an official credential.
// It is read for each request and is not copied into the login configuration.
type RemoteTarget struct {
	URL   *url.URL
	Key   string
	Model string
}

type RemoteOptions struct {
	Resolve func(context.Context, string) (*RemoteTarget, error)
	// Begin holds process-owned proxy streams across update/quit decisions.
	Begin func() func()
}

func (p *Proxy) remoteTarget(ctx context.Context, c Config, selected string) (*RemoteTarget, error) {
	if !c.RemoteInference || selected == "" {
		return nil, nil
	}
	if p.Remote.Resolve == nil {
		return nil, fmt.Errorf("remote inference is unavailable")
	}
	r, err := p.Remote.Resolve(ctx, selected)
	if err != nil || r == nil {
		return r, err // a local model continues through the local gateway
	}
	if r.URL == nil || (r.URL.Scheme != "http" && r.URL.Scheme != "https") || r.URL.Hostname() == "" || r.URL.User != nil || r.URL.RawQuery != "" || r.URL.Fragment != "" || r.Key == "" || strings.ContainsAny(r.Key, "\r\n") || r.Model == "" || p.Remote.Begin == nil {
		return nil, fmt.Errorf("remote inference configuration is invalid")
	}
	return r, nil
}
