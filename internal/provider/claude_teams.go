package provider

// Each organization is a normal subscription, with independent routing picks
// in providers.json and a private controller/recovery journal. The original
// files stay in place so an upgrade cannot lose an in-progress seat transfer.
import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const claudeTeamPrefix = "claude-team-"

var claudeTeams = struct {
	sync.Mutex
	path    string
	other   map[string]*claudeTeamController
	ctx     context.Context
	started map[*claudeTeamController]bool
	wg      *sync.WaitGroup
}{other: map[string]*claudeTeamController{}, started: map[*claudeTeamController]bool{}}

// claudeTeamControllers is a memory-only snapshot, safe on the inference path.
func claudeTeamControllers() []*claudeTeamController {
	claudeTeams.Lock()
	defer claudeTeams.Unlock()
	out := []*claudeTeamController{claudeTeam}
	if claudeTeams.path == claudeTeamPath() {
		keys := make([]string, 0, len(claudeTeams.other))
		for k := range claudeTeams.other {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			out = append(out, claudeTeams.other[k])
		}
	}
	return out
}

func discoverClaudeTeams() {
	claudeTeams.Lock()
	defer claudeTeams.Unlock()
	path := claudeTeamPath()
	if claudeTeams.path != path {
		claudeTeams.path, claudeTeams.other = path, map[string]*claudeTeamController{}
	}
	root := filepath.Join(filepath.Dir(Path()), "claude-teams")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() || !claudeTeamUUID.MatchString(d.Name()) {
			continue
		}
		if _, ok := claudeTeams.other[d.Name()]; ok {
			continue
		}
		t := newClaudeTeamController()
		t.folder = filepath.Join(root, d.Name())
		c, err := t.readConfig()
		if err != nil || c.OrganizationID != d.Name() {
			// A damaged organization's members are quarantined, not silently
			// returned to the standalone provider. No switch or seat is written.
			c = ClaudeTeamConfig{OrganizationID: d.Name(), Threshold: 100}
			t.err = "Claude Team configuration is unreadable or belongs to another organization"
			for _, l := range readLogins() {
				if id, ok := savedSessionIdentity(l); ok && id.Agent == "claude" && id.OrganizationID == d.Name() {
					c.Members = append(c.Members, strings.ToLower(id.AccountID))
					t.users[strings.ToLower(l.User)] = strings.ToLower(id.AccountID)
				}
			}
		}
		t.cfg = c
		claudeTeams.other[d.Name()] = t
	}
}

func claudeTeamForOrg(org string, create bool) (*claudeTeamController, error) {
	org = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(org, claudeTeamPrefix)))
	if org != "" && !claudeTeamUUID.MatchString(org) {
		return nil, errors.New("invalid Claude Team organization UUID")
	}
	discoverClaudeTeams()
	for _, t := range claudeTeamControllers() {
		t.mu.Lock()
		c := t.cfg
		t.mu.Unlock()
		if c.OrganizationID == "" {
			c, _ = t.readConfig()
		}
		if c.OrganizationID == org && org != "" {
			return t, nil
		}
	}
	if !create {
		return nil, errors.New("Claude Team subscription not found")
	}
	old, err := claudeTeam.readConfig()
	if err != nil {
		return nil, err
	}
	if old.OrganizationID == "" {
		return claudeTeam, nil
	}
	claudeTeams.Lock()
	defer claudeTeams.Unlock()
	if t := claudeTeams.other[org]; t != nil {
		return t, nil
	}
	t := newClaudeTeamController()
	t.folder = filepath.Join(filepath.Dir(Path()), "claude-teams", org)
	claudeTeams.other[org] = t
	return t, nil
}

func claudeTeamForUser(user string) *claudeTeamController {
	for _, t := range claudeTeamControllers() {
		t.mu.Lock()
		id := t.users[strings.ToLower(user)]
		found := t.cfg.Enabled && slices.Contains(t.cfg.Members, id)
		t.mu.Unlock()
		if found {
			return t
		}
	}
	return claudeTeam
}

func startClaudeTeam(t *claudeTeamController) {
	claudeTeams.Lock()
	defer claudeTeams.Unlock()
	if claudeTeams.ctx == nil || claudeTeams.ctx.Err() != nil || claudeTeams.started[t] {
		return
	}
	claudeTeams.started[t] = true
	ctx := claudeTeams.ctx
	wg := claudeTeams.wg
	wg.Add(1)
	go func() { defer wg.Done(); t.keep(ctx) }()
}

func KeepClaudeTeam(ctx context.Context) {
	discoverClaudeTeams()
	wg := &sync.WaitGroup{}
	for {
		claudeTeams.Lock()
		if claudeTeams.ctx != nil && claudeTeams.ctx.Err() == nil {
			claudeTeams.Unlock()
			return
		}
		previous := claudeTeams.wg
		claudeTeams.Unlock()
		// A fast gateway restart must release the old controller's file locks
		// before its replacement starts. A cancelled owner can add no new work.
		if previous != nil {
			previous.Wait()
		}
		claudeTeams.Lock()
		if claudeTeams.wg != previous || claudeTeams.ctx != nil && claudeTeams.ctx.Err() == nil {
			claudeTeams.Unlock()
			continue
		}
		claudeTeams.ctx, claudeTeams.started, claudeTeams.wg = ctx, map[*claudeTeamController]bool{}, wg
		claudeTeams.Unlock()
		break
	}
	for _, t := range claudeTeamControllers() {
		startClaudeTeam(t)
	}
	<-ctx.Done()
	// Accepted transactions finish/recover before their process locks go away.
	claudeTeams.Lock()
	if claudeTeams.wg == wg {
		claudeTeams.ctx = nil
	} // serialize dynamic starts with shutdown
	claudeTeams.Unlock()
	wg.Wait()
}

func ClaudeTeamSubscriptions() []ClaudeTeamView {
	discoverClaudeTeams()
	var out []ClaudeTeamView
	for _, t := range claudeTeamControllers() {
		v := t.status()
		if v.Config.OrganizationID != "" {
			out = append(out, v)
		}
	}
	return out
}

func ClaudeTeamDraft() ClaudeTeamView {
	v := ClaudeTeamView{Config: ClaudeTeamConfig{Threshold: 100}, Members: []ClaudeTeamMember{}, History: []ClaudeTeamEvent{}}
	for _, l := range readLogins() {
		id, ok := savedSessionIdentity(l)
		if ok && id.Agent == "claude" {
			v.Members = append(v.Members, ClaudeTeamMember{AccountID: id.AccountID, OrganizationID: id.OrganizationID, User: l.User})
		}
	}
	return v
}

func ClaudeTeamOrganization(org string) (ClaudeTeamView, error) {
	t, err := claudeTeamForOrg(org, false)
	if err != nil {
		return ClaudeTeamView{}, err
	}
	return t.status(), nil
}

func RotateClaudeTeamOrganization(org, from string) error {
	t, err := claudeTeamForOrg(org, false)
	if err != nil {
		return err
	}
	return t.rotate(from)
}

func AwaitClaudeTeamUser(ctx context.Context, user string) bool {
	return claudeTeamForUser(user).await(ctx)
}
func ClaudeTeamUserReset(user string) (time.Time, bool) { return claudeTeamForUser(user).reset() }

// ClaudeTeamOf identifies the subscription without asking a controller or API.
func ClaudeTeamOf(p Provider) string {
	org, ok := strings.CutPrefix(p.ID, claudeTeamPrefix)
	if ok && claudeTeamUUID.MatchString(org) {
		return org
	}
	return ""
}

func (t *claudeTeamController) routeEnabled(c ClaudeTeamConfig) bool {
	f := load()
	for _, p := range f.Providers {
		if p.ID == claudeTeamPrefix+c.OrganizationID {
			return !p.Off && !p.Hidden
		}
	}
	if t == claudeTeam {
		for _, p := range f.Providers {
			if p.ID == "claude" {
				return !p.Off && !p.Hidden
			}
		}
	}
	return true
}

// ClaudeTeamLoginOwned includes disabled pools: their accounts still belong
// to their organization, not to the standalone Claude Code subscription.
func ClaudeTeamLoginOwned(user string) bool { return claudeLoginInTeam(user) }

func claudeTeamRoutingOn(user string) bool {
	for _, t := range claudeTeamControllers() {
		t.mu.Lock()
		c, id := t.cfg, t.users[strings.ToLower(user)]
		t.mu.Unlock()
		if c.Enabled && (user == "" || slices.Contains(c.Members, id)) && t.routeEnabled(c) {
			return true
		}
	}
	return false
}

func claudeTeamProviders(base Provider) []Provider {
	var out []Provider
	for _, v := range ClaudeTeamSubscriptions() {
		ids, err := teamIdentities(v.Config)
		if err != nil {
			continue
		}
		var first savedLogin
		for _, id := range v.Config.Members {
			l := ids[id]
			if first.User == "" || l.On && !l.TeamOff && !l.Paused && l.Lapsed == "" {
				first = l
			}
			if l.On && !l.TeamOff && !l.Paused && l.Lapsed == "" {
				break
			}
		}
		if first.User == "" {
			continue
		}
		p := base
		p.ID, p.Name = claudeTeamPrefix+v.Config.OrganizationID, v.Config.Name
		if p.Name == "" {
			p.Name = "Claude Team · " + v.Config.OrganizationID[:8]
		}
		p.Icon = "claude-color"
		p.Routing = v.Config.Routing
		if t, _ := claudeTeamForOrg(v.Config.OrganizationID, false); t == claudeTeam {
			p.Was = []string{"claude"}
		}
		p.Account = claudeTeamAccount(base.Account, first)
		out = append(out, p)
	}
	return out
}

func claudeTeamObservedAt(user string) *time.Time {
	t := claudeTeamForUser(user)
	t.mu.Lock()
	at := t.disk.Observed[t.users[strings.ToLower(user)]]
	t.mu.Unlock()
	if at.IsZero() {
		return nil
	}
	return &at
}

func claudeTeamAccount(base *Account, l savedLogin) *Account {
	a := *base
	a.User, a.Plan, a.standIn = l.User, l.Plan, false
	user := l.User
	a.token = func(context.Context) (string, error) { return claudeSavedDir(user) }
	return &a
}

func claudeTeamLoginBelongs(org, user string) bool {
	for _, t := range claudeTeamControllers() {
		t.mu.Lock()
		id := t.users[strings.ToLower(user)]
		ok := t.cfg.OrganizationID == org && slices.Contains(t.cfg.Members, id)
		t.mu.Unlock()
		if ok {
			return true
		}
	}
	// Catalog and GUI can be read before the background controller starts.
	t, err := claudeTeamForOrg(org, false)
	if err != nil {
		return false
	}
	c, err := t.readConfig()
	if err != nil {
		return false
	}
	ids, err := teamIdentities(c)
	if err != nil {
		return false
	}
	for _, id := range c.Members {
		if strings.EqualFold(ids[id].User, user) {
			return true
		}
	}
	return false
}

func claudeLoginInTeam(user string) bool {
	for _, t := range claudeTeamControllers() {
		t.mu.Lock()
		c := t.cfg
		t.mu.Unlock()
		if c.OrganizationID == "" {
			c, _ = t.readConfig()
		}
		if c.OrganizationID != "" && claudeTeamLoginBelongs(c.OrganizationID, user) {
			return true
		}
	}
	return false
}

// TeamRouteOn also guards the single-candidate fallback, which ordinarily
// resurrects Claude Code's own paused account if no alternatives remain.
func (p Provider) TeamRouteOn() bool {
	if p.Account == nil || p.Account.Agent != "claude" {
		return true
	}
	if ClaudeTeamOf(p) == "" && !ClaudeTeamManaged(p.Account.User) {
		return true
	}
	t := claudeTeamForUser(p.Account.User)
	t.mu.Lock()
	id := t.users[strings.ToLower(p.Account.User)]
	assigned := t.cfg.Enabled && slices.Contains(t.cfg.Members, id) && (t.seats[id] == "team_tier_1" || t.legacyRoutes[id]) && !t.closed[id]
	t.mu.Unlock()
	if !assigned {
		return false
	}
	for _, l := range readLogins() {
		if l.Agent == "claude" && strings.EqualFold(l.User, p.Account.User) {
			return l.On && !l.TeamOff && !l.Paused && l.Lapsed == ""
		}
	}
	return false
}

// ClaudeTeamRouteError explains an unavailable control plane without making
// it a preflight check for healthy inference. Its messages never contain tokens.
func ClaudeTeamRouteError(p Provider) string {
	org := ClaudeTeamOf(p)
	if org == "" {
		return ""
	}
	t, err := claudeTeamForOrg(org, false)
	if err != nil {
		return "Claude Team subscription state is unavailable"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != "" {
		return "Claude Team has no eligible inference account: " + t.err
	}
	if t.disk.Pending != nil {
		return "Claude Team seat transfer is awaiting reconciliation; no unverified account was enabled"
	}
	return "Claude Team has no eligible inference account; waiting for confirmed seat assignments"
}

// Team providers enumerate only their own assigned accounts. Claude Code's
// currently signed-in account is neither a preferred route nor a fallback.
func (p Provider) teamAlsoOn() []Provider {
	org := ClaudeTeamOf(p)
	var out []Provider
	for _, l := range readLogins() {
		if !l.On || l.TeamOff || l.Paused || l.Lapsed != "" || strings.EqualFold(l.User, p.Account.User) {
			continue
		}
		if !claudeTeamLoginBelongs(org, l.User) {
			continue
		}
		q := p
		q.Account = claudeTeamAccount(p.Account, l)
		if q.TeamRouteOn() {
			out = append(out, q)
		}
	}
	return out
}

// TeamQuota is the last real observation, never an inferred zero after reset.
// An unassigned account cannot refresh its quota until it owns a seat again.
func claudeTeamQuota(user string) ([]QuotaWindow, bool) {
	t := claudeTeamForUser(user)
	t.mu.Lock()
	id := t.users[strings.ToLower(user)]
	managed := t.cfg.Enabled && slices.Contains(t.cfg.Members, id)
	ls := slices.Clone(t.disk.Limits[id])
	t.mu.Unlock()
	if !managed || len(ls) == 0 {
		return nil, false
	}
	return claudeLimitWindows(ls), true
}

func claudeLimitWindows(ls []ClaudeLimit) []QuotaWindow {
	ws := []QuotaWindow{}
	for _, l := range ls {
		for _, k := range claudeKinds {
			if l.Kind != k.kind {
				continue
			}
			w := QuotaWindow{Name: k.name, Used: l.Used * 100, Span: k.span, Model: k.model}
			if l.ResetsAt > 0 {
				at := time.Unix(l.ResetsAt, 0)
				w.ResetsAt = &at
			}
			ws = append(ws, w)
			break
		}
	}
	return ws
}

// /usage is a local command, not an inference prompt. Failed or incomplete
// collections retain the last complete observation, including its reset times.
func (t *claudeTeamController) refreshQuota(ctx context.Context, id string) error {
	user := t.userOf(id)
	var ws []QuotaWindow
	var err error
	claudeCLIUsage.RLock()
	available := claudeCLIUsage.f != nil
	claudeCLIUsage.RUnlock()
	if !available {
		err = errClaudeCannotRun
	} else if user == "" {
		err = errors.New("saved Team identity unavailable")
	} else {
		ws, err = readSavedClaudeUsage(ctx, user)
	}
	if err == nil && !completeClaudeTeamQuota(ws) {
		err = errors.New("incomplete allowance windows")
	}
	t.mu.Lock()
	if t.disk.QuotaErrors == nil {
		t.disk.QuotaErrors = map[string]string{}
	}
	if err != nil {
		// Never expose helper stderr or credentials through the member API.
		t.disk.QuotaErrors[id] = "Quota collection failed; retry while this account has Premium access"
	} else {
		fresh := claudeTeamWindowLimits(ws)
		// /usage may omit a model-specific window supplied by response
		// headers. Do not erase those independent restrictions.
		for _, previous := range t.disk.Limits[id] {
			if !slices.ContainsFunc(fresh, func(w ClaudeLimit) bool { return w.Kind == previous.Kind }) {
				fresh = append(fresh, previous)
			}
		}
		t.disk.Limits[id] = fresh
		if t.disk.Observed == nil {
			t.disk.Observed = map[string]time.Time{}
		}
		t.disk.Observed[id] = time.Now().UTC()
		delete(t.disk.QuotaErrors, id)
	}
	t.signalLocked()
	t.mu.Unlock()
	if err == nil {
		noteClaudeTeamWindows(user, ws)
	}
	if e := t.persist(); e != nil {
		t.mu.Lock()
		t.disk.QuotaErrors[id] = "Could not save usage; retry collection"
		t.signalLocked()
		t.mu.Unlock()
		return e
	}
	if err != nil {
		return errors.New("Quota collection failed; retry while this account has Premium access")
	}
	return nil
}

func claudeTeamWindowLimits(ws []QuotaWindow) []ClaudeLimit {
	var ls []ClaudeLimit
	for _, w := range ws {
		for _, kind := range claudeKinds {
			if kind.name != w.Name {
				continue
			}
			l := ClaudeLimit{Kind: kind.kind, Used: w.Used / 100}
			if w.ResetsAt != nil {
				l.ResetsAt = w.ResetsAt.Unix()
			}
			ls = append(ls, l)
		}
	}
	return ls
}

func noteClaudeTeamWindows(user string, ws []QuotaWindow) {
	NoteClaudeLimits(user, claudeTeamWindowLimits(ws))
}
