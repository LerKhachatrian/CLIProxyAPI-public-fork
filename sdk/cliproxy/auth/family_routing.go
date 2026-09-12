package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const familySelectionMetadataKey = "cliproxy.family_selection"

// FamilyRoutingConfig controls bounded local routing state and acquisition.
// Concurrency is an operator safety setting, not a provider throughput claim.
type FamilyRoutingConfig struct {
	StatePath               string
	MaxConcurrentPerAccount int
	MaxQueuedPerAccount     int
	AdmissionTimeout        time.Duration
	IdleRetention           time.Duration
	MaxFamilies             int
	MaxMembers              int
}

func NormalizeFamilyRoutingConfig(cfg FamilyRoutingConfig) FamilyRoutingConfig {
	if cfg.MaxConcurrentPerAccount <= 0 {
		cfg.MaxConcurrentPerAccount = 4
	}
	cfg.MaxConcurrentPerAccount = min(cfg.MaxConcurrentPerAccount, 64)
	if cfg.MaxQueuedPerAccount <= 0 {
		cfg.MaxQueuedPerAccount = 32
	}
	cfg.MaxQueuedPerAccount = min(cfg.MaxQueuedPerAccount, 256)
	if cfg.AdmissionTimeout <= 0 {
		cfg.AdmissionTimeout = 2 * time.Minute
	}
	cfg.AdmissionTimeout = max(time.Second, min(cfg.AdmissionTimeout, 10*time.Minute))
	if cfg.IdleRetention <= 0 {
		cfg.IdleRetention = 30 * 24 * time.Hour
	}
	cfg.IdleRetention = max(24*time.Hour, min(cfg.IdleRetention, 366*24*time.Hour))
	if cfg.MaxFamilies <= 0 {
		cfg.MaxFamilies = 16384
	}
	cfg.MaxFamilies = min(cfg.MaxFamilies, 16384)
	if cfg.MaxMembers <= 0 {
		cfg.MaxMembers = 65536
	}
	cfg.MaxMembers = min(cfg.MaxMembers, 65536)
	return cfg
}

type familyEntry struct {
	Root       string    `json:"root"`
	AuthIndex  string    `json:"auth_index,omitempty"`
	Identity   string    `json:"account_identity,omitempty"`
	Generation uint64    `json:"generation"`
	LastSeen   time.Time `json:"last_seen"`
	authID     string
	epoch      uint64
}

type familyMember struct {
	Family      string `json:"family"`
	Parent      string `json:"parent,omitempty"`
	ParentKnown bool   `json:"parent_known"`
}

type familySelection struct {
	router     *familyRouter
	family     string
	authID     string
	authIndex  string
	identity   string
	epoch      uint64
	generation uint64
	sequence   uint64
	model      string
}

type familyRouter struct {
	mu           sync.Mutex
	cfg          FamilyRoutingConfig
	families     map[string]*familyEntry
	members      map[string]familyMember
	exhausted    map[string]familyExhaustion
	accounts     map[string]*familyAccountAdmission
	quotaSource  func(*Auth) FamilyQuotaObservation
	lookup       func(string, string, string) (*Auth, bool, bool)
	clock        func() time.Time
	cursor       uint64
	sequence     uint64
	durable      uint64
	changed      chan struct{}
	writePending chan struct{}
	stopWriter   chan struct{}
	writerDone   chan struct{}
	store        *familyStateStore
	stateErr     error
	closed       bool
	stopOnce     sync.Once
	lastPrune    time.Time
}

type FamilyRoutingStatus struct {
	Families                int  `json:"families"`
	Members                 int  `json:"members"`
	AssignedFamilies        int  `json:"assigned_families"`
	InFlight                int  `json:"in_flight"`
	Queued                  int  `json:"queued"`
	WeeklyExcludedAccounts  int  `json:"weekly_excluded_accounts"`
	UnknownQuotaAccounts    int  `json:"unknown_quota_accounts"`
	MaxConcurrentPerAccount int  `json:"max_concurrent_per_account"`
	MaxQueuedPerAccount     int  `json:"max_queued_per_account"`
	PersistenceHealthy      bool `json:"persistence_healthy"`
}

type familyRoutingError struct {
	cause    *Error
	reason   string
	reselect bool
}

func (e *familyRoutingError) Error() string         { return e.reason + ": " + e.cause.Message }
func (e *familyRoutingError) Unwrap() error         { return e.cause }
func (e *familyRoutingError) StatusCode() int       { return e.cause.HTTPStatus }
func (e *familyRoutingError) IsRequestScoped() bool { return !e.reselect }

func familyRequestError(reason, message string, status int) error {
	return &familyRoutingError{reason: reason, cause: &Error{Code: ErrorCodeRequestScoped, Message: message, HTTPStatus: status}}
}

func familyReselectError() error {
	return &familyRoutingError{reason: "family_reselect", reselect: true,
		cause: &Error{Code: ErrorCodeConnectionLifecycle, HTTPStatus: http.StatusTooManyRequests, Retryable: true,
			Message: "family assignment changed before the upstream attempt; retry with the complete request"}}
}

func isFamilyReselect(err error) bool {
	var routingErr *familyRoutingError
	return errors.As(err, &routingErr) && routingErr.reselect
}

// IsFamilyReselectError lets a pinned downstream WebSocket release the old
// account even when selection rejected it before a provider attempt.
func IsFamilyReselectError(err error) bool { return isFamilyReselect(err) }

func isFamilySelector(selector Selector) bool {
	affinity, ok := selector.(*SessionAffinitySelector)
	return ok && affinity.family != nil
}

// An admission invalidation consumed no credential attempt. Retry only a
// complete, unpinned request, with a separate bound against reset churn.
func retryFamilySelection(err error, authID string, opts cliproxyexecutor.Options, tried, attempted map[string]struct{}, count *int) bool {
	if !isFamilyReselect(err) || hasUpstreamExecutionAttempt(err) || pinnedAuthIDFromMetadata(opts.Metadata) != "" || *count >= 8 {
		return false
	}
	*count++
	delete(tried, authID)
	delete(attempted, authID)
	return true
}

func familyTokenFromOptions(opts cliproxyexecutor.Options) *familySelection {
	token, _ := opts.Metadata[familySelectionMetadataKey].(*familySelection)
	return token
}

func newFamilyRouter(cfg FamilyRoutingConfig) *familyRouter {
	r := &familyRouter{cfg: NormalizeFamilyRoutingConfig(cfg), families: map[string]*familyEntry{},
		members: map[string]familyMember{}, exhausted: map[string]familyExhaustion{},
		accounts: map[string]*familyAccountAdmission{}, clock: time.Now, changed: make(chan struct{}),
		writePending: make(chan struct{}, 1), stopWriter: make(chan struct{}), writerDone: make(chan struct{})}
	r.store, r.stateErr = openFamilyState(r.cfg.StatePath)
	if r.stateErr == nil {
		r.stateErr = r.loadState()
	}
	if r.stateErr != nil {
		if r.store != nil {
			_ = r.store.close()
		}
		close(r.writerDone)
	} else {
		go r.runWriter()
	}
	return r
}

func (s *SessionAffinitySelector) FamilyRoutingEnabled() bool {
	return s != nil && s.family != nil
}

func (r *familyRouter) attach(quota func(*Auth) FamilyQuotaObservation, lookup func(string, string, string) (*Auth, bool, bool)) {
	r.mu.Lock()
	r.quotaSource, r.lookup = quota, lookup
	r.mu.Unlock()
}

func (r *familyRouter) reconfigure(cfg FamilyRoutingConfig) bool {
	cfg = NormalizeFamilyRoutingConfig(cfg)
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg.StatePath != r.cfg.StatePath || cfg.MaxFamilies < len(r.families) || cfg.MaxMembers < len(r.members) || r.closed {
		return false
	}
	r.cfg = cfg
	for _, account := range r.accounts {
		r.dispatchLocked(account)
	}
	return true
}

func (r *familyRouter) unavailableLocked() error {
	if r.closed {
		return familyRequestError("family_router_stopped", "family routing is stopping; retry on the active router", http.StatusServiceUnavailable)
	}
	if r.stateErr != nil {
		return familyRequestError("family_persistence_unavailable", "family routing state is unavailable; no upstream request sent", http.StatusServiceUnavailable)
	}
	if r.quotaSource == nil || r.lookup == nil {
		return familyRequestError("family_quota_unavailable", "family routing requires the typed account quota projection", http.StatusServiceUnavailable)
	}
	return nil
}

func (r *familyRouter) pick(ctx context.Context, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	identity, errIdentity := parseFamilyRequestIdentity(opts)
	if errIdentity != nil {
		return nil, errIdentity
	}
	if opts.Metadata == nil {
		return nil, familyRequestError("family_metadata_required", "family selection requires request metadata", http.StatusInternalServerError)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if errUnavailable := r.unavailableLocked(); errUnavailable != nil {
		return nil, errUnavailable
	}
	now := r.clock()
	r.pruneLocked(now)
	key, entry, errFamily := r.resolveFamilyLocked(identity, now)
	if errFamily != nil {
		return nil, errFamily
	}
	available := make([]*Auth, 0, len(auths))
	for _, candidate := range auths {
		if candidate == nil || !strings.EqualFold(candidate.Provider, "codex") {
			continue
		}
		current, supported, blocked := r.lookup(candidate.Index, candidate.CodexAccountIdentity(), model)
		if current == nil || current.RegistrationEpoch != candidate.RegistrationEpoch || !supported ||
			current.Disabled || current.Status == StatusDisabled {
			continue
		}
		if blocked {
			continue
		}
		eligible, errQuota := r.quotaEligibleLocked(current, now)
		if errQuota != nil {
			return nil, errQuota
		}
		if eligible {
			available = append(available, current)
		}
	}
	var selected *Auth
	if entry.Identity != "" {
		for _, candidate := range available {
			if candidate.Index == entry.AuthIndex && candidate.CodexAccountIdentity() == entry.Identity {
				selected = candidate
				break
			}
		}
		if selected == nil {
			current, _, blocked := r.lookup(entry.AuthIndex, entry.Identity, model)
			if current != nil && !current.Disabled && current.Status != StatusDisabled {
				eligible, errQuota := r.quotaEligibleLocked(current, now)
				if errQuota != nil {
					return nil, errQuota
				}
				if eligible && !blocked {
					// A healthy family owner omitted by model, tier, policy, or a
					// connection pin is not permission to split the family.
					if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
						return nil, familyReselectError()
					}
					return nil, familyRequestError("family_account_incompatible", "the family's account is incompatible with this model or request policy", http.StatusConflict)
				}
			}
			r.clearAssignmentLocked(key, entry)
		}
	}
	if selected == nil {
		if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
			// An account-local continuation cannot establish a new generation.
			// Release the pin and require a complete replay on the next socket.
			return nil, familyReselectError()
		}
		if len(available) == 0 {
			return nil, familyRequestError("family_no_eligible_accounts", "no eligible Codex account is available for this family", http.StatusServiceUnavailable)
		}
		sort.Slice(available, func(i, j int) bool { return available[i].Index < available[j].Index })
		selected = available[r.cursor%uint64(len(available))]
		r.cursor++
		entry.AuthIndex, entry.Identity, entry.authID = selected.Index, selected.CodexAccountIdentity(), selected.ID
		entry.Generation++
		r.markDirtyLocked()
	}
	entry.authID = selected.ID
	if entry.epoch != 0 && entry.epoch != selected.RegistrationEpoch {
		entry.Generation++
		r.cancelFamilyWaitersLocked(key, familyReselectError())
		r.markDirtyLocked()
	}
	entry.epoch = selected.RegistrationEpoch
	if now.Sub(entry.LastSeen) >= time.Hour {
		entry.LastSeen = now.UTC()
		r.markDirtyLocked()
	}
	token := &familySelection{router: r, family: key, authID: selected.ID, authIndex: selected.Index,
		identity: selected.CodexAccountIdentity(), epoch: selected.RegistrationEpoch, generation: entry.Generation,
		sequence: r.sequence, model: model}
	opts.Metadata[familySelectionMetadataKey] = token
	return selected, nil
}

func (r *familyRouter) resolveFamilyLocked(identity familyRequestIdentity, now time.Time) (string, *familyEntry, error) {
	thread := familyHash("codex-thread", identity.thread)
	parent := ""
	if identity.parent != "" {
		parent = familyHash("codex-thread", identity.parent)
	}
	key, root := "", ""
	if identity.family != "" {
		key, root = familyHash("codex-family", identity.family), familyHash("codex-thread", identity.family)
	} else if member, ok := r.members[thread]; ok {
		key = member.Family
		root = r.families[key].Root
	} else if parent != "" {
		if member, ok := r.members[parent]; ok {
			key = member.Family
			root = r.families[key].Root
		} else {
			return "", nil, familyRequestError("family_identity_incomplete", "a cold descendant requires its root family Session-Id", http.StatusConflict)
		}
	} else {
		key, root = familyHash("codex-family", identity.thread), thread
	}
	if thread == root && parent != "" {
		return "", nil, familyRequestError("family_identity_conflict", "a family root cannot declare a parent", http.StatusConflict)
	}
	proposed := map[string]familyMember{root: {Family: key, ParentKnown: true}}
	if parent != "" && parent != root {
		proposed[parent] = familyMember{Family: key}
	}
	proposed[thread] = familyMember{Family: key, Parent: parent, ParentKnown: thread == root || parent != ""}
	newMembers := 0
	for id, member := range proposed {
		if old, exists := r.members[id]; exists {
			if old.Family != key || old.ParentKnown && member.ParentKnown && old.Parent != member.Parent {
				return "", nil, familyRequestError("family_identity_conflict", "logical thread membership or parent identity conflicts with retained lineage", http.StatusConflict)
			}
			if old.ParentKnown {
				proposed[id] = old
			}
		} else {
			newMembers++
		}
	}
	for ancestor, visited := parent, map[string]bool{thread: true}; ancestor != ""; {
		if visited[ancestor] {
			return "", nil, familyRequestError("family_identity_conflict", "cyclic family ancestry", http.StatusConflict)
		}
		visited[ancestor] = true
		member, ok := proposed[ancestor]
		if !ok {
			member = r.members[ancestor]
		}
		ancestor = member.Parent
	}
	entry := r.families[key]
	if entry == nil && len(r.families) >= r.cfg.MaxFamilies || len(r.members)+newMembers > r.cfg.MaxMembers {
		return "", nil, familyRequestError("family_state_capacity", "retained family state is full; active membership was preserved", http.StatusServiceUnavailable)
	}
	changed := false
	if entry == nil {
		entry = &familyEntry{Root: root, Generation: 1, LastSeen: now.UTC()}
		r.families[key] = entry
		changed = true
	} else if entry.Root != root {
		return "", nil, familyRequestError("family_identity_conflict", "retained family root conflicts with the request", http.StatusConflict)
	}
	for id, member := range proposed {
		if old, ok := r.members[id]; !ok || old != member {
			r.members[id] = member
			changed = true
		}
	}
	if changed {
		r.markDirtyLocked()
	}
	return key, entry, nil
}

func (r *familyRouter) clearAssignmentLocked(key string, entry *familyEntry) {
	if entry.Identity == "" {
		return
	}
	entry.AuthIndex, entry.Identity, entry.authID = "", "", ""
	entry.epoch = 0
	entry.Generation++
	r.cancelFamilyWaitersLocked(key, familyReselectError())
	r.markDirtyLocked()
}

func (r *familyRouter) resetAssignments() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cleared := 0
	if r.closed {
		return cleared
	}
	for key, entry := range r.families {
		if entry.Identity != "" {
			cleared++
			r.clearAssignmentLocked(key, entry)
		}
	}
	return cleared
}

func (r *familyRouter) invalidateAuth(authID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for key, entry := range r.families {
		if entry.authID == authID {
			r.clearAssignmentLocked(key, entry)
		}
	}
}

func (r *familyRouter) onResult(token *familySelection, result Result) {
	if result.Success || result.Error == nil || shouldSkipCredentialCooldown(result.Error) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	entry := r.families[token.family]
	if entry != nil && entry.Generation == token.generation && entry.Identity == token.identity && result.AuthID == token.authID {
		r.clearAssignmentLocked(token.family, entry)
	}
}

func (r *familyRouter) pruneLocked(now time.Time) {
	if !r.lastPrune.IsZero() && now.Sub(r.lastPrune) < time.Hour {
		return
	}
	r.lastPrune = now
	removed := map[string]bool{}
	for key, entry := range r.families {
		if now.Sub(entry.LastSeen) < r.cfg.IdleRetention || r.familyBusyLocked(key) {
			continue
		}
		delete(r.families, key)
		removed[key] = true
	}
	for id, member := range r.members {
		if removed[member.Family] {
			delete(r.members, id)
		}
	}
	if len(removed) > 0 {
		r.markDirtyLocked()
	}
	for identity, exhausted := range r.exhausted {
		if !exhausted.ResetAt.After(now) {
			delete(r.exhausted, identity)
			r.markDirtyLocked()
		}
	}
}

func (r *familyRouter) status() FamilyRoutingStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := FamilyRoutingStatus{Families: len(r.families), Members: len(r.members),
		MaxConcurrentPerAccount: r.cfg.MaxConcurrentPerAccount, MaxQueuedPerAccount: r.cfg.MaxQueuedPerAccount,
		PersistenceHealthy: r.stateErr == nil && !r.closed}
	for _, family := range r.families {
		if family.Identity != "" {
			status.AssignedFamilies++
		}
	}
	for _, account := range r.accounts {
		status.InFlight += account.inFlight
		status.Queued += account.queued
	}
	for _, evidence := range r.exhausted {
		if evidence.ResetAt.After(r.clock()) {
			status.WeeklyExcludedAccounts++
		}
	}
	return status
}
