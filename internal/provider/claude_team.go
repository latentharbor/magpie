package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/routes"
)

// ClaudeTeamConfig owns one organization's subscription and seat controller.
// Inference still runs through the ordinary Claude Code bridge.
type ClaudeTeamConfig struct {
	Name           string   `json:"name,omitempty"`
	Routing        string   `json:"routing,omitempty"`
	Enabled        bool     `json:"enabled"`
	OrganizationID string   `json:"organizationId"`
	SessionKey     string   `json:"sessionKey,omitempty"`
	Members        []string `json:"members"`
	Threshold      float64  `json:"threshold"`
}

type ClaudeTeamMember struct {
	AccountID      string        `json:"accountId"`
	OrganizationID string        `json:"organizationId"`
	User           string        `json:"user"`
	Selected       bool          `json:"selected"`
	On             bool          `json:"on"`
	SeatTier       string        `json:"seatTier"`
	State          string        `json:"state"`
	Limits         []ClaudeLimit `json:"limits"`
	Windows        []QuotaWindow `json:"windows"`
	Lapsed         bool          `json:"lapsed"`
	Active         int           `json:"active"`
	ObservedAt     *time.Time    `json:"observedAt,omitempty"`
	QuotaError     string        `json:"quotaError,omitempty"`
	QuotaPending   bool          `json:"quotaPending,omitempty"`
}

type ClaudeTeamEvent struct {
	At     time.Time `json:"at"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Result string    `json:"result"`
}

type ClaudeTeamView struct {
	Config     ClaudeTeamConfig   `json:"config"`
	SessionSet bool               `json:"sessionSet"`
	Members    []ClaudeTeamMember `json:"members"`
	Seats      int                `json:"seats"`
	InUse      int                `json:"inUse"`
	Rotating   bool               `json:"rotating"`
	Running    bool               `json:"running"`
	Error      string             `json:"error,omitempty"`
	NextReset  *time.Time         `json:"nextReset,omitempty"`
	History    []ClaudeTeamEvent  `json:"history"`
}

type claudeTeamFlags struct{ On, Paused, TeamOff bool }
type claudeTeamPending struct {
	OrganizationID string `json:"organizationId"`
	From, To       string
	Baseline       []string
}
type claudeTeamDisk struct {
	MembershipInitialized bool                       `json:"membershipInitialized,omitempty"`
	OrganizationID        string                     `json:"organizationId"`
	Seats                 map[string]string          `json:"seats,omitempty"`
	Limits                map[string][]ClaudeLimit   `json:"limits"`
	Observed              map[string]time.Time       `json:"observed,omitempty"`
	QuotaErrors           map[string]string          `json:"quotaErrors,omitempty"`
	Original              map[string]claudeTeamFlags `json:"original"`
	Pending               *claudeTeamPending         `json:"pending,omitempty"`
	History               []ClaudeTeamEvent          `json:"history"`
}

type claudeTeamController struct {
	folder                       string     // empty for the original, backwards-compatible subscription
	op                           sync.Mutex // transactions and configuration edits, never taken by a request
	observation                  sync.Mutex // publish quota cache and controller snapshots together; no network under this lock
	mu                           sync.Mutex
	cfg                          ClaudeTeamConfig
	disk                         claudeTeamDisk
	seats                        map[string]string
	legacyRoutes                 map[string]bool // last controller-owned switches, not verified cloud seats
	active                       map[string]int
	closed                       map[string]bool
	users                        map[string]string
	wake                         chan struct{}
	changed                      chan struct{}
	running, requested, rotating bool
	err                          string
	manual                       string
	loaded                       bool
	path                         string
	waiting                      int
	quotaDue                     map[string]bool
	quotaAttempted               map[string]bool
	quotaBusy                    string
	quotaChecks                  map[string]*claudeTeamQuotaCheck
}

func newClaudeTeamController() *claudeTeamController {
	return &claudeTeamController{wake: make(chan struct{}, 1), changed: make(chan struct{}),
		active: map[string]int{}, closed: map[string]bool{}, users: map[string]string{}, seats: map[string]string{}, quotaDue: map[string]bool{}, quotaAttempted: map[string]bool{},
		disk: claudeTeamDisk{Limits: map[string][]ClaudeLimit{}, Original: map[string]claudeTeamFlags{}}}
}

var claudeTeam = newClaudeTeamController()

func claudeTeamPath() string { return filepath.Join(filepath.Dir(Path()), "claude-team.json") }
func claudeTeamStatePath() string {
	return filepath.Join(filepath.Dir(Path()), "claude-team-state.json")
}

func (t *claudeTeamController) configPath() string {
	if t.folder == "" {
		return claudeTeamPath()
	}
	return filepath.Join(t.folder, "config.json")
}
func (t *claudeTeamController) statePath() string {
	if t.folder == "" {
		return claudeTeamStatePath()
	}
	return filepath.Join(t.folder, "state.json")
}

func readClaudeTeamConfig() (ClaudeTeamConfig, error) {
	return claudeTeam.readConfig()
}
func (t *claudeTeamController) readConfig() (ClaudeTeamConfig, error) {
	c := ClaudeTeamConfig{Threshold: 100}
	b, err := os.ReadFile(t.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, errors.New("Claude Team configuration is unreadable; last routing state was kept")
	}
	if t.folder != "" && c.OrganizationID != filepath.Base(t.folder) {
		return c, errors.New("Claude Team configuration belongs to another organization")
	}
	if c.Threshold == 0 {
		c.Threshold = 100
	}
	return c, nil
}

// teamIdentities matches saved profiles, not display email addresses, so two
// organizations of one email cannot share a seat by accident.
func teamIdentities(c ClaudeTeamConfig) (map[string]savedLogin, error) {
	out := map[string]savedLogin{}
	for _, l := range readLogins() {
		id, ok := savedSessionIdentity(l)
		if !ok || id.Agent != "claude" || !strings.EqualFold(id.OrganizationID, c.OrganizationID) {
			continue
		}
		key := strings.ToLower(id.AccountID)
		if _, exists := out[key]; exists {
			return nil, errors.New("duplicate Claude Team account identity")
		}
		out[key] = l
	}
	for _, id := range c.Members {
		if _, ok := out[id]; !ok {
			return nil, errors.New("a selected member has no saved Claude login in this organization")
		}
	}
	return out, nil
}

func validateClaudeTeam(c *ClaudeTeamConfig) error {
	c.OrganizationID = strings.ToLower(strings.TrimSpace(c.OrganizationID))
	c.SessionKey = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.SessionKey), "sessionKey="))
	if c.Threshold == 0 {
		c.Threshold = 100
	}
	if !claudeTeamUUID.MatchString(c.OrganizationID) || math.IsNaN(c.Threshold) || math.IsInf(c.Threshold, 0) || c.Threshold < 1 || c.Threshold > 100 {
		return errors.New("Claude Team needs an organization UUID and a threshold between 1 and 100")
	}
	if len(c.SessionKey) > 8192 || strings.ContainsAny(c.SessionKey, ";\r\n\t ") {
		return errors.New("paste only the administrator sessionKey value")
	}
	if c.Enabled && (c.SessionKey == "" || len(c.Members) < 2) {
		return errors.New("Claude Team needs an administrator session and at least two saved accounts")
	}
	seen := map[string]bool{}
	for i, id := range c.Members {
		id = strings.ToLower(strings.TrimSpace(id))
		if !claudeTeamUUID.MatchString(id) || seen[id] {
			return errors.New("invalid or duplicate Claude Team member UUID")
		}
		seen[id], c.Members[i] = true, id
	}
	_, err := teamIdentities(*c)
	return err
}

func (t *claudeTeamController) signalLocked() { close(t.changed); t.changed = make(chan struct{}) }
func (t *claudeTeamController) notify() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *claudeTeamController) persist() error {
	t.mu.Lock()
	b, err := json.MarshalIndent(t.disk, "", "  ")
	t.mu.Unlock()
	if err != nil {
		return err
	}
	return writePrivate(t.statePath(), append(b, '\n'))
}

// loadState runs under op. A corrupt recovery journal must never be replaced
// with an empty one: it may describe a PUT whose reply was lost.
func (t *claudeTeamController) loadState(c ClaudeTeamConfig) error {
	t.mu.Lock()
	if t.loaded {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()
	b, err := os.ReadFile(t.statePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("Claude Team recovery state cannot be read")
	}
	if err == nil {
		var state claudeTeamDisk
		if json.Unmarshal(b, &state) != nil || state.OrganizationID != c.OrganizationID {
			return errors.New("Claude Team recovery state is unreadable or belongs to another organization; automatic writes stopped")
		}
		if state.Limits == nil {
			state.Limits = map[string][]ClaudeLimit{}
		}
		if state.Original == nil {
			state.Original = map[string]claudeTeamFlags{}
		}
		t.mu.Lock()
		t.disk = state
		t.mu.Unlock()
	}
	t.mu.Lock()
	if t.disk.Observed == nil {
		t.disk.Observed = map[string]time.Time{}
	}
	t.loaded = true
	t.path = t.configPath()
	// An uncertain transfer must be reconciled before any cached assignment
	// can admit a request. Never infer that a released seat is still assigned.
	if t.disk.Pending == nil && c.Enabled {
		for id, tier := range t.disk.Seats {
			if slices.Contains(c.Members, id) && (tier == "team_tier_1" || tier == "unassigned") {
				t.seats[id] = tier
			}
		}
	}
	t.mu.Unlock()
	// Older journals have no seat snapshot. Preserve only the switches this
	// controller already owned, with a matching saved identity. These are
	// inference candidates, never claimed as freshly verified Premium seats.
	ids, idErr := teamIdentities(c)
	t.mu.Lock()
	if idErr == nil && c.Enabled && t.disk.Pending == nil && t.disk.Seats == nil {
		t.legacyRoutes = map[string]bool{}
		for _, id := range c.Members {
			l := ids[id]
			_, owned := t.disk.Original[id]
			if owned && l.On && !l.TeamOff && !l.Paused && l.Lapsed == "" {
				t.legacyRoutes[id] = true
			}
		}
	}
	t.mu.Unlock()
	return nil
}

func (t *claudeTeamController) selectPath() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path == "" || t.path == t.configPath() {
		return nil
	}
	if t.running {
		return errors.New("stop the Claude Team controller before changing the configuration directory")
	}
	t.cfg = ClaudeTeamConfig{}
	t.disk = claudeTeamDisk{Limits: map[string][]ClaudeLimit{}, Original: map[string]claudeTeamFlags{}}
	t.seats, t.users, t.closed = map[string]string{}, map[string]string{}, map[string]bool{}
	t.quotaDue, t.quotaAttempted, t.quotaBusy = map[string]bool{}, map[string]bool{}, ""
	t.quotaChecks = nil
	t.legacyRoutes = nil
	t.loaded, t.requested, t.rotating, t.manual, t.err = false, false, false, "", ""
	return nil
}

func (t *claudeTeamController) identities(c ClaudeTeamConfig) (map[string]savedLogin, error) {
	ids, err := teamIdentities(c)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	users := map[string]string{}
	for user, id := range t.users {
		if t.active[id] > 0 {
			users[user] = id
		}
	}
	for id, l := range ids {
		users[strings.ToLower(l.User)] = id
	}
	t.users = users
	t.mu.Unlock()
	return ids, nil
}

// SaveClaudeTeam never returns a session credential. An empty sessionKey keeps
// the existing one only for the same organization. Activation verifies members
// before changing any login switch; it never allocates an additional seat.
func SaveClaudeTeam(ctx context.Context, c ClaudeTeamConfig) error {
	t, err := claudeTeamForOrg(c.OrganizationID, true)
	if err != nil {
		return err
	}
	// Creating an organization adopts its already saved accounts as well as
	// those selected by an older editor. Editing an existing pool still keeps
	// deliberate exclusions; new logins join from addLogin.
	if old, err := t.readConfig(); err == nil && old.OrganizationID == "" {
		c.Members = slices.Clone(c.Members)
		for _, l := range readLogins() {
			id, ok := savedSessionIdentity(l)
			if ok && id.Agent == "claude" && strings.EqualFold(id.OrganizationID, strings.TrimSpace(c.OrganizationID)) && claudeTeamUUID.MatchString(id.AccountID) && !slices.Contains(c.Members, strings.ToLower(id.AccountID)) {
				c.Members = append(c.Members, strings.ToLower(id.AccountID))
			}
		}
	}
	if err := t.save(ctx, c); err != nil {
		return err
	}
	if p, err := Find(claudeTeamPrefix + strings.ToLower(strings.TrimSpace(c.OrganizationID))); err == nil {
		p.Routing = c.Routing
		if err := Save(*p); err != nil {
			return err
		}
	}
	startClaudeTeam(t)
	return nil
}
func (t *claudeTeamController) save(ctx context.Context, c ClaudeTeamConfig) error {
	t.op.Lock()
	defer t.op.Unlock()
	if err := t.selectPath(); err != nil {
		return err
	}
	t.mu.Lock()
	running := t.running
	t.mu.Unlock()
	if !running {
		unlock, held := filelock.Lock(t.configPath()+".controller.lock", 0)
		if !held {
			return errors.New("another process owns the Claude Team controller; use its configuration API")
		}
		defer unlock()
	}
	t.mu.Lock()
	old := t.cfg
	t.mu.Unlock()
	if old.OrganizationID == "" {
		var err error
		old, err = t.readConfig()
		if err != nil {
			return err
		}
	}
	if err := t.loadState(old); err != nil {
		return err
	}
	t.mu.Lock()
	pending := t.disk.Pending != nil
	oldFlags := len(t.disk.Original) != 0
	t.mu.Unlock()
	if c.SessionKey == "" && strings.EqualFold(c.OrganizationID, old.OrganizationID) {
		c.SessionKey = old.SessionKey
	}
	if err := validateClaudeTeam(&c); err != nil {
		return err
	}
	if pending {
		// A session can expire between release and verification. Permit
		// credential renewal, but never rewrite the plan being recovered.
		if c.Enabled != old.Enabled || c.OrganizationID != old.OrganizationID || c.Threshold != old.Threshold || !slices.Equal(c.Members, old.Members) {
			return errors.New("reconcile the unfinished Claude Team transfer before changing its pool")
		}
		if _, err := newClaudeTeamAdmin(c).Members(ctx); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(c, "", "  ")
		if err := writePrivate(t.configPath(), append(b, '\n')); err != nil {
			return err
		}
		t.mu.Lock()
		t.cfg = c
		t.mu.Unlock()
		t.notify()
		return nil
	}
	if (old.Enabled || oldFlags) && !strings.EqualFold(c.OrganizationID, old.OrganizationID) {
		return errors.New("disable the old Claude Team before changing organizations")
	}
	var seats []claudeTeamSeat
	if c.Enabled {
		var err error
		seats, err = newClaudeTeamAdmin(c).Members(ctx)
		if err != nil {
			return err
		}
		if err := teamMembersPresent(c, seats); err != nil {
			return err
		}
	}
	identities, err := teamIdentities(c)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if t.disk.OrganizationID != "" && t.disk.OrganizationID != c.OrganizationID {
		t.disk = claudeTeamDisk{Limits: map[string][]ClaudeLimit{}, Original: map[string]claudeTeamFlags{}}
	}
	t.disk.OrganizationID = c.OrganizationID
	for _, id := range c.Members {
		if !c.Enabled {
			break
		}
		if _, ok := t.disk.Original[id]; !ok {
			l := identities[id]
			t.disk.Original[id] = claudeTeamFlags{l.On, l.Paused, l.TeamOff}
		}
	}
	t.mu.Unlock()
	// Record restoration flags before touching logins, including the crash gap.
	if err := t.persist(); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := writePrivate(t.configPath(), append(b, '\n')); err != nil {
		return err
	}
	t.mu.Lock()
	t.cfg, t.err = c, ""
	t.mu.Unlock()
	if _, err := t.identities(c); err != nil {
		return err
	}
	for _, id := range c.Members {
		t.note(identities[id].User, KeptClaudeLimits(identities[id].User))
	}
	if err := t.restoreRemoved(c); err != nil {
		return err
	}
	if c.Enabled {
		if err := t.synchronize(c, seats); err != nil {
			return err
		}
	}
	// Joining the pool is a capture event even if no internal transfer took
	// place. The controller performs the read off this configuration request.
	t.mu.Lock()
	for _, id := range c.Members {
		if !slices.Contains(old.Members, id) {
			t.queueQuotaLocked(id)
		}
	}
	t.mu.Unlock()
	t.notify()
	return nil
}

func teamMembersPresent(c ClaudeTeamConfig, seats []claudeTeamSeat) error {
	for _, id := range c.Members {
		if !slices.ContainsFunc(seats, func(s claudeTeamSeat) bool {
			return s.AccountID == id && (s.SeatTier == "team_tier_1" || s.SeatTier == "unassigned")
		}) {
			return errors.New("a selected Claude Team member is missing or has another seat type")
		}
	}
	return nil
}

// restoreRemoved restores only switches owned by this controller, never a
// credential, another provider or an organization member outside the pool.
func (t *claudeTeamController) restoreRemoved(c ClaudeTeamConfig) error {
	t.mu.Lock()
	original := map[string]claudeTeamFlags{}
	for id, f := range t.disk.Original {
		if !c.Enabled || !slices.Contains(c.Members, id) {
			original[id] = f
		}
	}
	t.mu.Unlock()
	if len(original) == 0 {
		return nil
	}
	loginsMu.Lock()
	ls := readLogins()
	for i, l := range ls {
		id, ok := savedSessionIdentity(l)
		if ok && id.Agent == "claude" && strings.EqualFold(id.OrganizationID, c.OrganizationID) {
			if f, ok := original[strings.ToLower(id.AccountID)]; ok {
				ls[i].On, ls[i].Paused, ls[i].TeamOff = f.On, f.Paused, f.TeamOff
			}
		}
	}
	err := writeLogins(ls)
	loginsMu.Unlock()
	if err != nil {
		return err
	}
	t.mu.Lock()
	for id := range original {
		delete(t.disk.Original, id)
		delete(t.closed, id)
	}
	t.signalLocked()
	t.mu.Unlock()
	return t.persist()
}

func (t *claudeTeamController) route(c ClaudeTeamConfig, states map[string]bool) error {
	// Mark closed in memory first: an already-planned request cannot enter
	// between the login write and the source's drain counter being observed.
	t.mu.Lock()
	for id, on := range states {
		t.closed[id] = !on
	}
	t.signalLocked()
	t.mu.Unlock()
	loginsMu.Lock()
	ls := readLogins()
	dirty := false
	for i, l := range ls {
		id, ok := savedSessionIdentity(l)
		if !ok || id.Agent != "claude" || !strings.EqualFold(id.OrganizationID, c.OrganizationID) {
			continue
		}
		if on, ok := states[strings.ToLower(id.AccountID)]; ok && (l.On != on || l.TeamOff == on || l.Paused) {
			ls[i].On, ls[i].TeamOff, ls[i].Paused, dirty = on, !on, false, true
		}
	}
	var err error
	if dirty {
		err = writeLogins(ls)
	}
	loginsMu.Unlock()
	return err
}

func (t *claudeTeamController) synchronize(c ClaudeTeamConfig, seats []claudeTeamSeat) error {
	if err := teamMembersPresent(c, seats); err != nil {
		return err
	}
	states := map[string]bool{}
	t.mu.Lock()
	t.disk.Seats = map[string]string{}
	t.legacyRoutes = nil
	for _, s := range seats {
		previous := t.seats[s.AccountID]
		t.seats[s.AccountID] = s.SeatTier
		if slices.Contains(c.Members, s.AccountID) {
			if s.SeatTier == "team_tier_1" && (previous != s.SeatTier || !t.quotaAttempted[s.AccountID] && len(t.disk.Limits[s.AccountID]) == 0) {
				t.queueQuotaLocked(s.AccountID)
			}
			t.disk.Seats[s.AccountID] = s.SeatTier
			states[s.AccountID] = s.SeatTier == "team_tier_1"
		}
	}
	t.mu.Unlock()
	if err := t.persist(); err != nil {
		return err
	}
	return t.route(c, states)
}

// TrackClaudeTeamRequest is local request accounting, not a control-plane
// check. No file, quota endpoint, helper or network is consulted. The boolean
// closes the stale-candidate race once the background controller drains a user.
func TrackClaudeTeamRequest(user string) (release func(), admitted bool) {
	return claudeTeamForUser(user).track(user)
}
func (t *claudeTeamController) track(user string) (func(), bool) {
	t.mu.Lock()
	id, managed := t.users[strings.ToLower(user)]
	if !t.cfg.Enabled || !managed || !slices.Contains(t.cfg.Members, id) {
		t.mu.Unlock()
		return func() {}, true
	}
	if t.closed[id] {
		t.mu.Unlock()
		return func() {}, false
	}
	t.active[id]++
	t.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { t.mu.Lock(); t.active[id]--; t.signalLocked(); t.mu.Unlock() }) }, true
}

func (t *claudeTeamController) note(user string, ls []ClaudeLimit) {
	t.mu.Lock()
	id := t.users[strings.ToLower(user)]
	if !t.cfg.Enabled || !slices.Contains(t.cfg.Members, id) {
		t.mu.Unlock()
		return
	}
	ws := slices.Clone(t.disk.Limits[id])
	for _, l := range ls {
		if !slices.ContainsFunc(claudeKinds, func(k struct {
			kind, name, model string
			span              time.Duration
		}) bool {
			return k.kind == l.Kind
		}) || math.IsNaN(l.Used) || l.Used < 0 || l.Used > 1 {
			continue
		}
		if i := slices.IndexFunc(ws, func(w ClaudeLimit) bool { return w.Kind == l.Kind }); i >= 0 {
			ws[i] = l
		} else {
			ws = append(ws, l)
		}
	}
	t.disk.Limits[id] = ws
	if len(ls) > 0 {
		if t.disk.Observed == nil {
			t.disk.Observed = map[string]time.Time{}
		}
		t.disk.Observed[id] = time.Now()
	}
	spent := teamSpent(ws, t.cfg.Threshold, time.Now())
	if spent {
		t.requested = true
	}
	t.signalLocked()
	t.mu.Unlock()
	if spent {
		t.notify()
	}
}

func teamSpent(ws []ClaudeLimit, threshold float64, now time.Time) bool {
	for _, w := range ws {
		if (w.Kind == "five_hour" || w.Kind == "seven_day") && w.Used*100 >= threshold && (w.ResetsAt == 0 || w.ResetsAt > now.Unix()) {
			return true
		}
	}
	return false
}

// A confirmed sign-out closes stale admissions immediately and wakes the
// existing background transfer. Call only after releasing credential locks;
// temporary refresh/read errors and stale credential refusals never enter here.
func noteClaudeTeamSignInFailure(user string) {
	t := claudeTeamForUser(user)
	t.mu.Lock()
	id := t.users[strings.ToLower(user)]
	wake := t.cfg.Enabled && slices.Contains(t.cfg.Members, id) &&
		t.seats[id] == "team_tier_1" && !t.closed[id]
	if wake {
		t.closed[id], t.requested = true, true
		t.signalLocked()
	}
	t.mu.Unlock()
	if wake {
		t.notify()
	}
}

func teamBack(ws []ClaudeLimit, threshold float64, now time.Time) (time.Time, bool) {
	var at time.Time
	for _, w := range ws {
		if (w.Kind != "five_hour" && w.Kind != "seven_day") || w.Used*100 < threshold || w.ResetsAt > 0 && w.ResetsAt <= now.Unix() {
			continue
		}
		if w.ResetsAt <= 0 {
			return time.Time{}, false
		}
		if next := time.Unix(w.ResetsAt, 0); next.After(at) {
			at = next
		}
	}
	return at, !at.IsZero()
}

func (t *claudeTeamController) waitDrained(ctx context.Context, id string) error {
	for {
		t.mu.Lock()
		n, ch := t.active[id], t.changed
		t.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("Claude Team drain timed out; no assignment was changed")
		case <-ch:
		}
	}
}

// AwaitClaudeTeamRotation is used only for an uncommitted failed request or
// an empty route during a handoff. It never waits for an allowance reset.
func AwaitClaudeTeamRotation(ctx context.Context) bool {
	waited := false
	for _, t := range claudeTeamControllers() {
		waited = t.await(ctx) || waited
	}
	return waited
}
func (t *claudeTeamController) await(ctx context.Context) bool {
	t.mu.Lock()
	if t.waiting >= 128 {
		t.mu.Unlock()
		return false
	}
	t.waiting++
	t.mu.Unlock()
	defer func() { t.mu.Lock(); t.waiting--; t.mu.Unlock() }()
	waited := false
	for {
		t.mu.Lock()
		busy, ch := t.cfg.Enabled && t.running && (t.requested || t.rotating), t.changed
		t.mu.Unlock()
		if !busy {
			return waited
		}
		waited = true
		select {
		case <-ctx.Done():
			return false
		case <-ch:
		}
	}
}

// ClaudeTeamManaged is a cached local fact, for holding only the refusal of a
// managed account. Normal, already-streaming replies keep their existing path.
func ClaudeTeamManaged(user string) bool {
	t := claudeTeamForUser(user)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg.Enabled && slices.Contains(t.cfg.Members, t.users[strings.ToLower(user)])
}

func ClaudeTeamStatusEnabled() bool {
	for _, t := range claudeTeamControllers() {
		t.mu.Lock()
		on := t.cfg.Enabled
		t.mu.Unlock()
		if on {
			return true
		}
	}
	return false
}

// ClaudeTeamExhausted narrows an otherwise generic 429 to a global allowance
// window actually observed on this account, not a transient per-minute limit.
func ClaudeTeamExhausted(user string) bool {
	t := claudeTeamForUser(user)
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.users[strings.ToLower(user)]
	return t.cfg.Enabled && slices.Contains(t.cfg.Members, id) && teamSpent(t.disk.Limits[id], t.cfg.Threshold, time.Now())
}

// ClaudeTeamReset is known only when every usable pool member is actually
// observed exhausted. It never guesses a reset for an unknown window.
func ClaudeTeamReset() (time.Time, bool) {
	var earliest time.Time
	for _, t := range claudeTeamControllers() {
		back, ok := t.reset()
		if !ok {
			return time.Time{}, false
		}
		if earliest.IsZero() || back.Before(earliest) {
			earliest = back
		}
	}
	return earliest, !earliest.IsZero()
}
func (t *claudeTeamController) reset() (time.Time, bool) {
	v := t.status()
	if !v.Config.Enabled {
		return time.Time{}, false
	}
	var earliest time.Time
	for _, m := range v.Members {
		if !m.Selected || m.Lapsed {
			continue
		}
		back, ok := teamBack(m.Limits, v.Config.Threshold, time.Now())
		if !ok {
			return time.Time{}, false
		}
		if earliest.IsZero() || back.Before(earliest) {
			earliest = back
		}
	}
	return earliest, !earliest.IsZero()
}

func claudeTeamParked(user string) ([]ClaudeLimit, bool) {
	t := claudeTeamForUser(user)
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.users[strings.ToLower(user)]
	parked := t.cfg.Enabled && slices.Contains(t.cfg.Members, id) && t.closed[id]
	return slices.Clone(t.disk.Limits[id]), parked
}

func premiumSet(seats []claudeTeamSeat) []string {
	var out []string
	for _, s := range seats {
		if s.SeatTier == "team_tier_1" {
			out = append(out, s.AccountID)
		}
	}
	slices.Sort(out)
	return out
}

func teamExpected(seats []claudeTeamSeat, from, to, phase string, baseline []string) bool {
	states := map[string]string{}
	for _, s := range seats {
		states[s.AccountID] = s.SeatTier
	}
	want := slices.Clone(baseline)
	if phase != "original" {
		want = slices.DeleteFunc(want, func(s string) bool { return s == from })
	}
	if phase == "assigned" {
		want = append(want, to)
		slices.Sort(want)
	}
	f, n := "team_tier_1", "unassigned"
	if phase != "original" {
		f = "unassigned"
	}
	if phase == "assigned" {
		n = "team_tier_1"
	}
	return states[from] == f && states[to] == n && slices.Equal(premiumSet(seats), want)
}

func teamVerify(ctx context.Context, client claudeTeamAdmin, p claudeTeamPending, phase string) ([]claudeTeamSeat, error) {
	for range 5 {
		seats, err := client.Members(ctx)
		if err != nil {
			return nil, err
		}
		if teamExpected(seats, p.From, p.To, phase, p.Baseline) {
			return seats, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, errors.New("Claude Team assignment differs from the verified plan; automatic writes stopped")
}

func (t *claudeTeamController) finish(c ClaudeTeamConfig, seats []claudeTeamSeat, p claudeTeamPending, result string) error {
	if err := t.synchronize(c, seats); err != nil {
		return err
	}
	t.mu.Lock()
	t.disk.History = append(t.disk.History, ClaudeTeamEvent{time.Now().UTC(), p.From, p.To, result})
	if len(t.disk.History) > 30 {
		t.disk.History = slices.Clone(t.disk.History[len(t.disk.History)-30:])
	}
	t.disk.Pending = nil
	t.mu.Unlock()
	if err := t.persist(); err != nil {
		return err
	}
	// Clear quota rests, not genuine revoked/expired credential marks.
	if result == "assigned" {
		tellRenewed("claude", t.userOf(p.To))
	}
	return nil
}

func (t *claudeTeamController) userOf(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for user, account := range t.users {
		if account == id {
			return user
		}
	}
	return ""
}

// recover never repeats an uncertain PUT. A fresh complete member snapshot
// proves success, no change, or a still-free seat safe to restore. Any other
// state, including a change outside the pool, requires operator attention.
func (t *claudeTeamController) recover(ctx context.Context, c ClaudeTeamConfig, client claudeTeamAdmin, p claudeTeamPending) error {
	if p.OrganizationID != c.OrganizationID || p.From == p.To || !slices.Contains(c.Members, p.From) || !slices.Contains(c.Members, p.To) || !slices.Contains(p.Baseline, p.From) || slices.Contains(p.Baseline, p.To) {
		return errors.New("unfinished Claude Team transfer does not match the configured pool")
	}
	seats, err := client.Members(ctx)
	if err != nil {
		return err
	}
	switch {
	case teamExpected(seats, p.From, p.To, "assigned", p.Baseline):
		return t.finish(c, seats, p, "assigned")
	case teamExpected(seats, p.From, p.To, "original", p.Baseline):
		return t.finish(c, seats, p, "unchanged")
	case teamExpected(seats, p.From, p.To, "released", p.Baseline):
		_ = client.SetTier(ctx, p.From, "team_tier_1")
		seats, err = teamVerify(ctx, client, p, "original")
		if err != nil {
			return err
		}
		return t.finish(c, seats, p, "rolled_back")
	default:
		return errors.New("Claude Team assignments changed concurrently; check the organization before retrying")
	}
}

func (t *claudeTeamController) transfer(ctx context.Context, c ClaudeTeamConfig, client claudeTeamAdmin, seats []claudeTeamSeat, from, to string) error {
	if err := t.route(c, map[string]bool{from: false, to: false}); err != nil {
		return err
	}
	if err := t.waitDrained(ctx, from); err != nil {
		_ = t.synchronize(c, seats)
		return err
	}
	p := claudeTeamPending{OrganizationID: c.OrganizationID, From: from, To: to, Baseline: premiumSet(seats)}
	if _, err := teamVerify(ctx, client, p, "original"); err != nil {
		return err
	}
	t.mu.Lock()
	t.disk.Pending = &p
	t.mu.Unlock()
	if err := t.persist(); err != nil {
		return err
	}
	_ = client.SetTier(ctx, from, "unassigned")
	if _, err := teamVerify(ctx, client, p, "released"); err != nil {
		return t.recover(ctx, c, client, p)
	}
	_ = client.SetTier(ctx, to, "team_tier_1")
	seats, err := teamVerify(ctx, client, p, "assigned")
	if err != nil {
		return t.recover(ctx, c, client, p)
	}
	return t.finish(c, seats, p, "assigned")
}

func (t *claudeTeamController) choose(c ClaudeTeamConfig, seats []claudeTeamSeat, manual string) (string, string, error) {
	identities, err := teamIdentities(c)
	if err != nil {
		return "", "", err
	}
	// The provider's routing pick is also the standby-selection policy.
	for _, p := range load().Providers {
		if p.ID == claudeTeamPrefix+c.OrganizationID {
			c.Routing = p.Routing
			break
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	from, to := "", ""
	var candidates []string
	var weights []routes.Seat
	for _, s := range seats {
		if !slices.Contains(c.Members, s.AccountID) {
			continue
		}
		if s.SeatTier == "team_tier_1" && (manual != "" && manual == s.AccountID ||
			manual == "" && (identities[s.AccountID].Lapsed != "" || teamSpent(t.disk.Limits[s.AccountID], c.Threshold, now))) && from == "" {
			from = s.AccountID
		}
		if s.SeatTier != "unassigned" || identities[s.AccountID].Lapsed != "" || teamSpent(t.disk.Limits[s.AccountID], c.Threshold, now) {
			continue
		}
		ws := claudeLimitWindows(t.disk.Limits[s.AccountID])
		a := allowanceOf(ws, now)
		used, _ := a.For("", now)
		pace, _ := a.Pace("", now)
		known := len(ws) > 0
		if !known {
			pace = FreshPace
		}
		candidates = append(candidates, s.AccountID)
		if c.Routing == "" {
			known = claudeTeamQuotaKnown(t.disk.Limits[s.AccountID])
		}
		weights = append(weights, routes.Seat{Key: s.AccountID, Account: true, Known: known, Learns: !known, Used: used, Pace: pace, Soon: a.Renewal("", now), Weight: 1})
	}
	if len(candidates) > 0 {
		to = candidates[0]
		if c.Routing == "" {
			to = candidates[ClaudeTeamSmart(weights)[0]]
		} else if strategy, ok := routes.Find(c.Routing); ok {
			if order := routes.Apply(strategy, claudeTeamPrefix+c.OrganizationID+"/standby", weights); len(order) > 0 {
				to = candidates[order[0]]
			}
		}
	}
	// Smart also spends a parked account's expiring allowance before it is
	// lost, rather than waiting for an unrelated active account to run dry.
	// Only complete observations justify a voluntary transfer; threshold and
	// manual handoffs above remain authoritative. The normal drain is reused.
	if from == "" && to != "" && manual == "" && c.Routing == "" {
		var latest routes.Seat
		standby := weights[slices.Index(candidates, to)]
		if standby.Known {
			for _, s := range seats {
				ls := t.disk.Limits[s.AccountID]
				if s.SeatTier != "team_tier_1" || !slices.Contains(c.Members, s.AccountID) || identities[s.AccountID].Lapsed != "" || !claudeTeamQuotaKnown(ls) {
					continue
				}
				active := routes.Seat{Key: s.AccountID, Known: true, Soon: allowanceOf(claudeLimitWindows(ls), now).Renewal("", now)}
				if ClaudeTeamSmart([]routes.Seat{active, standby})[0] != 1 {
					continue
				}
				if from == "" || ClaudeTeamSmart([]routes.Seat{active, latest})[0] == 1 {
					from, latest = s.AccountID, active
				}
			}
		}
	}
	if manual != "" && from == "" {
		return "", "", errors.New("the chosen source has no Premium seat in this pool")
	}
	if from != "" && to == "" {
		return from, "", errors.New("all Claude Team alternatives are cooling down, signed out or unknown; see the earliest reset")
	}
	return from, to, nil
}

func (t *claudeTeamController) reconcile(ctx context.Context, manual string) error {
	t.mu.Lock()
	c, pending := t.cfg, t.disk.Pending
	t.rotating = true
	t.signalLocked()
	t.mu.Unlock()
	defer func() { t.mu.Lock(); t.rotating, t.requested = false, false; t.signalLocked(); t.mu.Unlock() }()
	if !c.Enabled {
		return t.restoreRemoved(c)
	}
	identities, err := t.identities(c)
	if err != nil {
		return err
	}
	// A manually created configuration also needs a restoration journal
	// before the first switch write, just as activation through the GUI does.
	t.mu.Lock()
	t.disk.OrganizationID = c.OrganizationID
	dirty := false
	for _, id := range c.Members {
		if _, ok := t.disk.Original[id]; !ok {
			l := identities[id]
			t.disk.Original[id] = claudeTeamFlags{l.On, l.Paused, l.TeamOff}
			dirty = true
		}
	}
	t.mu.Unlock()
	if dirty {
		if err := t.persist(); err != nil {
			return err
		}
	}
	client := newClaudeTeamAdmin(c)
	if pending != nil {
		return t.recover(ctx, c, client, *pending)
	}
	seats, err := client.Members(ctx)
	if err != nil {
		return err
	} // keep known login switches on transient API failure
	if err := teamMembersPresent(c, seats); err != nil {
		return err
	}
	if err := t.synchronize(c, seats); err != nil {
		return err
	}
	if manual == "" && !t.routeEnabled(c) {
		t.collectQuotaDue(ctx)
		return nil // a disabled/removed subscription must not rotate in the background
	}
	// Read newly observed assignments before stale snapshots can move them.
	t.collectQuotaDue(ctx)
	from, to, err := t.choose(c, seats, manual)
	if err != nil {
		return err
	}
	if from == "" || to == "" {
		return nil
	}
	return t.transfer(ctx, c, client, seats, from, to)
}

// RotateClaudeTeam asks the gateway's one background controller to move one
// source seat. A client disconnect does not interrupt an accepted transaction.
func RotateClaudeTeam(from string) error {
	return claudeTeam.rotate(from)
}
func (t *claudeTeamController) rotate(from string) error {
	t.mu.Lock()
	if !t.running || !t.cfg.Enabled || t.rotating || t.requested {
		t.mu.Unlock()
		return errors.New("Claude Team controller is disabled, unavailable or already rotating")
	}
	if from != "" && !slices.Contains(t.cfg.Members, from) {
		t.mu.Unlock()
		return errors.New("source is outside the Claude Team pool")
	}
	if from == "" {
		for _, id := range t.cfg.Members {
			if t.seats[id] == "team_tier_1" {
				from = id
				break
			}
		}
		if from == "" {
			t.mu.Unlock()
			return errors.New("no verified Premium seat in the Claude Team pool")
		}
	}
	t.requested = true
	// Manual work is owned by the controller loop, not an HTTP goroutine.
	t.manual = from
	t.mu.Unlock()
	t.notify()
	return nil
}

// KeepClaudeTeam is owned by the gateway process. Neither GUI polls nor a
// normal inference request call Members or change a seat.
func (t *claudeTeamController) keep(ctx context.Context) {
	t.mu.Lock()
	t.err = "waiting for the previous Claude Team controller to release this configuration"
	t.signalLocked()
	t.mu.Unlock()
	unlock, lockErr := filelock.LockContext(ctx, t.configPath()+".controller.lock")
	if lockErr != nil {
		t.mu.Lock()
		if ctx.Err() == nil {
			t.err = "Claude Team controller lock: " + lockErr.Error()
		} else {
			t.err = ""
		}
		t.signalLocked()
		t.mu.Unlock()
		return
	}
	defer unlock()
	if ctx.Err() != nil {
		return
	}
	t.op.Lock()
	if err := t.selectPath(); err != nil {
		t.op.Unlock()
		return
	}
	c, err := t.readConfig()
	if err != nil {
		t.mu.Lock()
		c = t.cfg // retain the quarantined organization instead of resurrecting it as standalone Claude
		t.mu.Unlock()
	}
	if err == nil && c.OrganizationID != "" {
		err = validateClaudeTeam(&c)
	}
	t.mu.Lock()
	t.running, t.cfg = true, c
	// Discovery may have cached a journal before the old owner finished its
	// last transaction. Only state read under our process lock is authoritative.
	t.loaded = false
	t.seats = map[string]string{}
	t.legacyRoutes = nil
	if err != nil {
		t.err, t.cfg.Enabled = err.Error(), false
	}
	t.mu.Unlock()
	if err == nil {
		err = t.loadState(c)
	}
	if err != nil {
		t.mu.Lock()
		t.err, t.cfg.Enabled, t.running = err.Error(), false, false
		t.mu.Unlock()
		t.op.Unlock()
		return
	}
	if identities, idErr := teamIdentities(c); idErr == nil {
		t.mu.Lock()
		for id, l := range identities {
			t.users[strings.ToLower(l.User)] = id
			t.closed[id] = l.TeamOff
		}
		t.mu.Unlock()
	}
	t.mu.Lock()
	t.err = ""
	t.signalLocked()
	t.mu.Unlock()
	t.op.Unlock()
	defer func() { t.mu.Lock(); t.running = false; t.signalLocked(); t.mu.Unlock() }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-t.wake:
		}
		if ctx.Err() != nil {
			return
		}
		joinErr := t.adoptSavedLogins()
		t.op.Lock()
		t.mu.Lock()
		manual := t.manual
		t.manual = ""
		t.mu.Unlock()
		// Once accepted, transfer reconciliation has an independent bounded
		// lifetime; shutdown waits for it before releasing the controller lock.
		t.collectQuotaDue(ctx)
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		err := t.reconcile(work, manual)
		cancel()
		if err == nil {
			err = joinErr
		}
		// New logins/members and externally observed assignments share the
		// same capture queue as internal assignments, independent of history.
		t.collectQuotaDue(ctx)
		if saveErr := t.persist(); err == nil {
			err = saveErr
		}
		t.mu.Lock()
		t.err = ""
		if err != nil {
			t.err = err.Error()
		}
		t.mu.Unlock()
		t.op.Unlock()
		timer.Reset(5 * time.Minute)
	}
}

func ClaudeTeamStatus() ClaudeTeamView {
	return claudeTeam.status()
}
func (t *claudeTeamController) status() ClaudeTeamView {
	if err := t.selectPath(); err != nil {
		return ClaudeTeamView{Error: err.Error(), Members: []ClaudeTeamMember{}, History: []ClaudeTeamEvent{}}
	}
	t.mu.Lock()
	c := t.cfg
	v := ClaudeTeamView{Config: c, SessionSet: c.SessionKey != "", Running: t.running, Rotating: t.rotating || t.requested, Error: t.err,
		Members: []ClaudeTeamMember{}, History: slices.Clone(t.disk.History)}
	v.Config.SessionKey = ""
	seats, limits, active := map[string]string{}, map[string][]ClaudeLimit{}, map[string]int{}
	for id, s := range t.seats {
		seats[id] = s
	}
	for id, ls := range t.disk.Limits {
		limits[id] = slices.Clone(ls)
	}
	observed := make(map[string]time.Time, len(t.disk.Observed))
	for id, at := range t.disk.Observed {
		observed[id] = at
	}
	for id, n := range t.active {
		active[id] = n
	}
	quotaErrors := make(map[string]string, len(t.disk.QuotaErrors))
	quotaPending := make(map[string]bool, len(t.quotaDue)+1)
	for id, e := range t.disk.QuotaErrors {
		quotaErrors[id] = e
	}
	for id := range t.quotaDue {
		quotaPending[id] = true
	}
	if t.quotaBusy != "" {
		quotaPending[t.quotaBusy] = true
	}
	t.mu.Unlock()
	if c.OrganizationID == "" {
		if disk, err := t.readConfig(); err == nil {
			c = disk
			v.Config = c
			v.SessionSet = c.SessionKey != ""
			v.Config.SessionKey = ""
		} else {
			v.Error = err.Error()
		}
	}
	for _, l := range readLogins() {
		id, ok := savedSessionIdentity(l)
		if !ok || id.Agent != "claude" || c.OrganizationID != "" && !strings.EqualFold(id.OrganizationID, c.OrganizationID) {
			continue
		}
		account := strings.ToLower(id.AccountID)
		m := ClaudeTeamMember{AccountID: account, OrganizationID: id.OrganizationID, User: l.User, Selected: slices.Contains(c.Members, account), On: l.On && !l.TeamOff, SeatTier: seats[account], Limits: limits[account], Lapsed: l.Lapsed != "", Active: active[account], State: "ready"}
		m.Windows = claudeTeamWindowsAt(m.Limits, time.Now())
		m.QuotaError, m.QuotaPending = quotaErrors[account], quotaPending[account]
		if at := observed[account]; !at.IsZero() {
			m.ObservedAt = &at
		}
		if m.Lapsed {
			m.State = "signed_out"
		} else if teamSpent(m.Limits, c.Threshold, time.Now()) {
			m.State = "cooling"
		} else if len(m.Limits) == 0 {
			m.State = "unknown"
		}
		if m.Selected && m.SeatTier == "team_tier_1" {
			v.Seats++
			if m.On && !m.Lapsed {
				v.InUse++
			}
			if !m.Lapsed {
				m.State = "premium"
			}
		}
		if m.Selected {
			if back, ok := teamBack(m.Limits, c.Threshold, time.Now()); ok && (v.NextReset == nil || back.Before(*v.NextReset)) {
				v.NextReset = &back
			}
		}
		v.Members = append(v.Members, m)
	}
	slices.SortStableFunc(v.Members, func(a, b ClaudeTeamMember) int {
		rank := func(m ClaudeTeamMember) int {
			if m.State == "premium" {
				return 0
			}
			if m.State == "ready" || m.State == "unknown" {
				return 1
			}
			return 2
		}
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a.User, b.User)
	})
	return v
}

func init() {
	OnRenewed(func(agent, user string) {
		if agent == "claude" && ClaudeTeamManaged(user) {
			t := claudeTeamForUser(user)
			t.note(user, KeptClaudeLimits(user))
			t.notify()
		}
	})
}
