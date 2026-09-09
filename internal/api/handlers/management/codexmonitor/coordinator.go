package codexmonitor

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

type Coordinator struct {
	mu           sync.Mutex
	store        Store
	state        control
	entries      map[string]*Entry
	dirty        map[string]bool
	lastFlush    time.Time
	inFlight     bool
	inFlightKey  string
	inFlightLane string
	closed       bool
	clock        func() time.Time
	random       func(int64) int64
	storageError bool
}

func New(store Store) (*Coordinator, error) {
	state, entries, err := store.load()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return &Coordinator{store: store, state: state, entries: entries, dirty: map[string]bool{}, clock: time.Now, random: rand.Int64N}, nil
}

// Close never releases the OS lock while a request still owns provider I/O.
// Server shutdown first drains HTTP handlers; an abrupt process exit releases
// the lock through the OS and leaves pre-dispatch attempt deadlines durable.
func (c *Coordinator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight {
		return errors.New("monitor check still active")
	}
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.flush(true, c.clock())
	errClose := c.store.Close()
	if err != nil {
		return err
	}
	return errClose
}

func (c *Coordinator) jitter(base time.Duration, low, high float64) time.Duration {
	span := max(int64(float64(base)*(high-low)), 1)
	return time.Duration(float64(base)*low) + time.Duration(c.random(span))
}

func (c *Coordinator) reconcile(ids []Identity, p Policy, now time.Time) error {
	if c.closed || len(ids) > MaxAccounts {
		return errors.New("monitor unavailable or account limit exceeded")
	}
	keys, indexes := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		_, duplicate := keys[id.Key]
		if !cacheKey.MatchString(id.Key) || id.AuthIndex == "" || len(id.AuthIndex) > 160 || duplicate || indexes[id.AuthIndex] {
			return errors.New("monitor account identity missing or ambiguous")
		}
		keys[id.Key], indexes[id.AuthIndex] = id.Enabled, true
	}
	for key := range c.entries {
		if !keys[key] {
			// Drop authority in memory even when deletion cannot yet be persisted.
			c.entries[key].Usage, c.entries[key].Bank = nil, nil
			if err := c.store.removeEntry(key); err != nil {
				c.storageError = true
				return errors.New("monitor cache invalidation failed")
			}
			delete(c.entries, key)
			delete(c.dirty, key)
		}
	}
	for _, id := range ids {
		if !id.Enabled {
			continue
		}
		e := c.entries[id.Key]
		if e == nil {
			e = &Entry{Schema: 1, Key: id.Key, Active: id.Active}
			delay := c.jitter(24*time.Hour, 0, 1)
			if id.Active {
				delay = c.jitter(5*time.Minute, .2, 1)
			}
			e.UsageSchedule.DueAt = now.Add(delay)
			e.ResetSchedule.DueAt = now.Add(c.jitter(24*time.Hour, 0, 1))
			c.entries[id.Key], c.dirty[id.Key] = e, true
		}
		if id.Active && !e.Active {
			soon := now.Add(c.jitter(5*time.Minute, .2, 1))
			if soon.Before(e.UsageSchedule.DueAt) {
				e.UsageSchedule.DueAt = soon
			}
		}
		if e.Active != id.Active {
			e.Active, c.dirty[id.Key] = id.Active, true
		}
		if u := id.Passive; useful(u) && !u.ObservedAt.After(now) && u.ObservedAt.After(e.InvalidatedAt) && now.Sub(u.ObservedAt) <= 48*time.Hour &&
			(e.Usage == nil || u.ObservedAt.After(e.Usage.ObservedAt)) {
			e.Usage = cloneUsage(u, now)
			if useful(e.Usage) && (e.UsageSchedule.ErrorAt.IsZero() || u.ObservedAt.After(e.UsageSchedule.ErrorAt)) {
				// Passive success supplies a new reading, not permission to bypass a
				// separately observed monitoring/provider Retry-After deadline.
				e.UsageSchedule.Error = ""
				e.UsageSchedule.ErrorAt = time.Time{}
				e.UsageSchedule.DueAt = u.ObservedAt.Add(c.usageDelay(id.Active, p))
				observeRecovery(e, u.ObservedAt)
			}
			c.dirty[id.Key] = true
		}
		for _, lane := range []*Lane{&e.UsageSchedule, &e.ResetSchedule} {
			if !lane.PendingAt.IsZero() && now.Sub(lane.PendingAt) > MaxPendingAge {
				lane.PendingAt = time.Time{}
				lane.Error = "refresh_expired"
				lane.ErrorAt = now
				c.dirty[id.Key] = true
			}
		}
	}
	return c.flush(false, now)
}

func (c *Coordinator) usageDelay(active bool, p Policy) time.Duration {
	if active {
		seconds := p.UsageSeconds
		if seconds == 0 {
			seconds = 1800
		}
		return c.jitter(time.Duration(seconds)*time.Second, 2.0/3, 4.0/3)
	}
	return c.jitter(24*time.Hour, 5.0/6, 7.0/6)
}

func (c *Coordinator) flush(force bool, now time.Time) error {
	if !force && !c.storageError && !c.lastFlush.IsZero() && now.Sub(c.lastFlush) < time.Minute {
		return nil
	}
	for key := range c.dirty {
		if err := c.store.saveEntry(c.entries[key]); err != nil {
			c.storageError = true
			return errors.New("monitor persistence unavailable; provider checks paused")
		}
		delete(c.dirty, key)
	}
	c.storageError = false
	c.lastFlush = now
	return nil
}

func (c *Coordinator) Snapshot(ids []Identity, policy Policy) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if err := c.reconcile(ids, NormalizePolicy(policy), now); err != nil {
		return Snapshot{}, err
	}
	return c.snapshot(ids, now), nil
}

type candidate struct {
	id   Identity
	lane string
	due  time.Time
}

func (c *Coordinator) queue(ids []Identity, req Request, now time.Time) error {
	if req.Refresh == "" {
		if req.AuthIndex != "" {
			return errors.New("manual target requires a refresh kind")
		}
		return nil
	}
	if req.Refresh != UsageLane && req.Refresh != ResetLane {
		return errors.New("refresh must be usage or resets")
	}
	matched := req.AuthIndex == ""
	// A whole-lane batch remains one intent until its last account completes.
	// Otherwise clicks after the first minute could requeue early accounts and
	// indefinitely extend a paced batch. Targeted refreshes coalesce per account.
	if req.AuthIndex == "" {
		if c.inFlightLane == req.Refresh {
			return nil
		}
		for _, e := range c.entries {
			lane := e.UsageSchedule
			if req.Refresh == ResetLane {
				lane = e.ResetSchedule
			}
			if !lane.PendingAt.IsZero() {
				return nil
			}
		}
	}
	for _, id := range ids {
		if req.AuthIndex != "" && id.AuthIndex != req.AuthIndex {
			continue
		}
		matched = true
		if !id.Enabled || id.Blocked || id.RetryAt.After(now) {
			continue
		}
		e := c.entries[id.Key]
		if e.RetryAt.After(now) || (c.inFlightKey == id.Key && c.inFlightLane == req.Refresh) {
			continue
		}
		lane := &e.UsageSchedule
		if req.Refresh == ResetLane {
			lane = &e.ResetSchedule
		}
		if lane.PendingAt.IsZero() && !lane.LastAttempt.Add(time.Minute).After(now) && !lane.RetryAt.After(now) {
			lane.PendingAt = now
			c.dirty[id.Key] = true
		}
	}
	if !matched {
		return errors.New("manual target no longer exists")
	}
	return c.flush(true, now)
}

func (c *Coordinator) selectDue(ids []Identity, p Policy, now time.Time) *candidate {
	var candidates []candidate
	for _, id := range ids {
		if !id.Enabled || id.Blocked || id.RetryAt.After(now) {
			continue
		}
		e := c.entries[id.Key]
		if e.RetryAt.After(now) {
			continue
		}
		for _, kind := range []string{UsageLane, ResetLane} {
			lane, seconds := e.UsageSchedule, p.UsageSeconds
			if kind == ResetLane {
				lane, seconds = e.ResetSchedule, p.ResetSeconds
			}
			if lane.RetryAt.After(now) || lane.LastAttempt.Add(time.Minute).After(now) {
				continue
			}
			due := lane.DueAt
			if !lane.PendingAt.IsZero() {
				due = lane.PendingAt
			} else if seconds == 0 {
				continue
			} else if kind == ResetLane {
				// A manual attempt also resets automatic eligibility. The daily
				// floor is not shortened by failure, restart or another client.
				floor := lane.LastAttempt.Add(time.Duration(max(86400, seconds)) * time.Second)
				if floor.After(due) {
					due = floor
				}
			}
			if !due.After(now) {
				candidates = append(candidates, candidate{id, kind, due})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.due.Equal(b.due) {
			return a.due.Before(b.due)
		}
		if a.id.Priority != b.id.Priority {
			return a.id.Priority > b.id.Priority
		}
		if a.id.Key != b.id.Key {
			return a.id.Key < b.id.Key
		}
		return a.lane < b.lane
	})
	if len(candidates) == 0 {
		return nil
	}
	return &candidates[0]
}

// Step performs zero or one provider read. Concurrent callers never wait on the
// provider mutex: they receive the shared cached snapshot with in_flight=true.
func (c *Coordinator) Step(ctx context.Context, ids []Identity, req Request, fetch Fetch) (Snapshot, error) {
	c.mu.Lock()
	now, p := c.clock(), NormalizePolicy(req.Policy)
	if err := c.reconcile(ids, p, now); err != nil {
		c.mu.Unlock()
		return Snapshot{}, err
	}
	if err := c.queue(ids, req, now); err != nil {
		c.mu.Unlock()
		return Snapshot{}, err
	}
	var starts []time.Time
	for _, started := range c.state.Starts {
		if started.After(now.Add(-time.Minute)) {
			starts = append(starts, started)
		}
	}
	selected := c.selectDue(ids, p, now)
	if c.inFlight || c.state.NextStart.After(now) || len(starts) >= 6 || selected == nil || ctx.Err() != nil {
		snapshot := c.snapshot(ids, now)
		c.mu.Unlock()
		return snapshot, nil
	}
	e := c.entries[selected.id.Key]
	lane := &e.UsageSchedule
	delay := c.usageDelay(selected.id.Active, p)
	if selected.lane == ResetLane {
		lane = &e.ResetSchedule
		delay = c.jitter(time.Duration(max(86400, p.ResetSeconds))*time.Second, 1, 13.0/12)
	}
	lane.LastAttempt, lane.DueAt = now, now.Add(delay)
	lane.PendingAt, lane.Error = time.Time{}, "interrupted_check"
	c.dirty[e.Key] = true
	// Persist the account claim first. Any later failure may lose an opportunity
	// to check, but cannot create an unrecorded provider attempt after a crash.
	if err := c.flush(true, now); err != nil {
		c.mu.Unlock()
		return Snapshot{}, err
	}
	c.state.Starts, c.state.NextStart = append(starts, now), now.Add(time.Duration(p.GapSeconds)*time.Second)
	if err := c.store.saveControl(c.state); err != nil {
		c.storageError = true
		c.mu.Unlock()
		return Snapshot{}, errors.New("monitor budget persistence unavailable; no provider request sent")
	}
	c.inFlight, c.inFlightKey, c.inFlightLane = true, selected.id.Key, selected.lane
	c.mu.Unlock()
	result := safeFetch(ctx, fetch, selected.id, selected.lane)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight, c.inFlightKey, c.inFlightLane = false, "", ""
	now = c.clock()
	if current := c.entries[e.Key]; current == e {
		c.applyResult(e, lane, selected.lane, result, now)
		c.dirty[e.Key] = true
		if err := c.flush(true, now); err != nil {
			snapshot := c.snapshot(ids, now)
			snapshot.Error = "persistence_unavailable"
			return snapshot, nil
		}
	}
	snapshot := c.snapshot(ids, now)
	snapshot.Attempted = selected.lane
	return snapshot, nil
}

// An internal adapter panic must not leave the lifetime owner permanently busy.
// The already durable attempt is retained, no request is replayed, and neither
// the panic value nor an upstream body is exposed as diagnostics.
func safeFetch(ctx context.Context, fetch Fetch, id Identity, kind string) (result Result) {
	defer func() {
		if recover() != nil {
			result = Result{Status: httpStatusUnavailable}
		}
	}()
	return fetch(ctx, id, kind)
}

const httpStatusUnavailable = 503

func (c *Coordinator) applyResult(e *Entry, lane *Lane, kind string, r Result, now time.Time) {
	if kind == UsageLane && r.Usage != nil && !r.Usage.ObservedAt.After(e.InvalidatedAt) ||
		kind == ResetLane && r.Bank != nil && !r.Bank.ObservedAt.After(e.InvalidatedAt) {
		return // an in-flight pre-action response cannot restore invalidated state
	}
	valid := r.Status >= 200 && r.Status < 300
	if kind == UsageLane {
		valid = valid && useful(r.Usage) && !r.Usage.ObservedAt.After(now)
		if valid && (e.Usage == nil || !r.Usage.ObservedAt.Before(e.Usage.ObservedAt)) {
			e.Usage = cloneUsage(r.Usage, now)
		}
	} else {
		grants := 0
		for key, other := range c.entries {
			if key != e.Key && other.Bank != nil {
				grants += len(other.Bank.Credits)
			}
		}
		valid = valid && r.Bank != nil && len(r.Bank.Credits)+grants <= MaxGrants && !r.Bank.ObservedAt.After(now)
		if valid && (e.Bank == nil || !r.Bank.ObservedAt.Before(e.Bank.ObservedAt)) {
			e.Bank = cloneBank(r.Bank, now)
		}
	}
	if valid {
		observedAt := time.Time{}
		if kind == UsageLane {
			observedAt = r.Usage.ObservedAt
		} else {
			observedAt = r.Bank.ObservedAt
		}
		if !lane.ErrorAt.IsZero() && !observedAt.After(lane.ErrorAt) {
			return // a delayed read is not recovery from a newer failure
		}
		lane.Error, lane.Failures, lane.RetryAt, lane.ErrorAt = "", 0, time.Time{}, time.Time{}
		observeRecovery(e, observedAt)
		return
	}
	lane.ErrorAt = now
	lane.Failures = min(10, lane.Failures+1)
	initial := time.Minute
	lane.Error = "provider_unavailable"
	switch r.Status {
	case 401:
		initial, lane.Error = 30*time.Minute, "oauth_rejected"
	case 403:
		initial, lane.Error = 30*time.Minute, "provider_blocked"
	case 429:
		initial, lane.Error = 5*time.Minute, "rate_limited"
	case 200:
		lane.Error = "invalid_observation"
	}
	backoff := min(30*time.Minute, initial*time.Duration(1<<min(5, lane.Failures-1)))
	lane.RetryAt = now.Add(backoff)
	if r.RetryAt.After(lane.RetryAt) {
		lane.RetryAt = r.RetryAt
	}
	if r.Status == 401 || r.Status == 403 || r.Status == 429 || r.RetryAt.After(now) {
		// An account-level rejection/rate limit applies to both endpoints. Keep
		// their observation/error histories independent, but never probe the
		// other endpoint to work around a blocked monitoring request.
		if lane.RetryAt.After(e.RetryAt) {
			e.RetryAt = lane.RetryAt
		}
	}
	if kind == UsageLane && lane.RetryAt.After(lane.DueAt) {
		lane.DueAt = lane.RetryAt
	}
}

// A newer successful observation disproves an older credential rejection, but
// says nothing about the other lane's balance. Preserve its error/age, attempt
// history and every cooldown floor; only stop incorrectly demanding OAuth again.
func observeRecovery(e *Entry, observedAt time.Time) {
	for _, lane := range []*Lane{&e.UsageSchedule, &e.ResetSchedule} {
		if lane.Error == "oauth_rejected" && !lane.ErrorAt.IsZero() && observedAt.After(lane.ErrorAt) {
			lane.Error = "observation_needed"
		}
	}
}

// ObserveRead adopts a fixed-route, identity-verified explicit management read.
// It never performs I/O to the provider or replays an action. Explicit action
// preflights keep their freshness/consent contract and count toward the next
// automatic eligibility floor, without becoming an automatic queue.
func (c *Coordinator) ObserveRead(ids []Identity, key, kind string, result Result) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, p := c.clock(), NormalizePolicy(Policy{UsageSeconds: 1800, ResetSeconds: 86400})
	if kind != UsageLane && kind != ResetLane {
		return errors.New("unknown observation lane")
	}
	if err := c.reconcile(ids, p, now); err != nil {
		return err
	}
	e := c.entries[key]
	if e == nil {
		return errors.New("observation identity no longer available")
	}
	lane, delay := &e.UsageSchedule, c.usageDelay(e.Active, p)
	if kind == ResetLane {
		lane, delay = &e.ResetSchedule, c.jitter(24*time.Hour, 1, 13.0/12)
	}
	c.applyResult(e, lane, kind, result, now)
	// Do not erase any already queued explicit intent. Its normal duplicate
	// floor still coalesces a simultaneous read with the manual batch.
	lane.LastAttempt, lane.DueAt = now, now.Add(delay)
	c.dirty[key] = true
	return c.flush(true, now)
}

// InvalidateAction is called before dispatch and after every outcome (including
// transport uncertainty). Persisting this barrier before dispatch prevents a
// crash/restart or delayed passive response from reviving pre-action balances.
// Cadence and cooldowns are retained. No credit outcome is inferred here.
func (c *Coordinator) InvalidateAction(ids []Identity, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if err := c.reconcile(ids, Policy{}, now); err != nil {
		return err
	}
	e := c.entries[key]
	if e == nil {
		return errors.New("action identity no longer available")
	}
	e.Usage, e.Bank = nil, nil
	if now.After(e.InvalidatedAt) {
		e.InvalidatedAt = now
	}
	e.UsageSchedule.Error, e.ResetSchedule.Error = "action_changed", "action_changed"
	e.UsageSchedule.ErrorAt, e.ResetSchedule.ErrorAt = now, now
	c.dirty[key] = true
	return c.flush(true, now)
}

func cloneUsage(u *Usage, now time.Time) *Usage {
	if u == nil || now.Before(u.ObservedAt) || now.Sub(u.ObservedAt) > 48*time.Hour {
		return nil
	}
	copy := *u
	if u.Allowed != nil {
		allowed := *u.Allowed
		copy.Allowed = &allowed
	}
	if u.LimitReached != nil {
		limitReached := *u.LimitReached
		copy.LimitReached = &limitReached
	}
	copy.Windows = []Window{}
	for _, w := range u.Windows {
		if w.ResetAt.After(now) {
			copy.Windows = append(copy.Windows, w)
		}
	}
	if !useful(&copy) {
		return nil
	}
	return &copy
}

func cloneBank(b *Bank, now time.Time) *Bank {
	if b == nil {
		return nil
	}
	copy := *b
	if b.AvailableCount != nil {
		count := *b.AvailableCount
		copy.AvailableCount = &count
	}
	copy.Credits = append([]Grant{}, b.Credits...)
	expired := now.Before(b.ObservedAt) || now.Sub(b.ObservedAt) > 8*24*time.Hour
	for _, grant := range b.Credits {
		if grant.Status == "available" && grant.ExpiresAt != nil && !grant.ExpiresAt.After(now) {
			expired = true
		}
	}
	if expired {
		copy.AvailableCount, copy.Complete = nil, false
	}
	return &copy
}

func (c *Coordinator) snapshot(ids []Identity, now time.Time) Snapshot {
	s := Snapshot{Schema: 1, ObservedAt: now.UTC(), Accounts: []Account{}, InFlight: c.inFlight, NextStart: c.state.NextStart}
	for _, id := range ids {
		a := Account{AuthIndex: id.AuthIndex, Identity: id.Key, Blocked: id.Blocked, Active: id.Active}
		if e := c.entries[id.Key]; e != nil && id.Enabled {
			a.Usage, a.Bank = cloneUsage(e.Usage, now), cloneBank(e.Bank, now)
			a.UsageSchedule, a.ResetSchedule = e.UsageSchedule, e.ResetSchedule
			if e.RetryAt.After(a.UsageSchedule.RetryAt) {
				a.UsageSchedule.RetryAt = e.RetryAt
			}
			if e.RetryAt.After(a.ResetSchedule.RetryAt) {
				a.ResetSchedule.RetryAt = e.RetryAt
			}
			for _, lane := range []Lane{e.UsageSchedule, e.ResetSchedule} {
				if !lane.PendingAt.IsZero() {
					s.Pending++
				}
			}
		}
		s.Accounts = append(s.Accounts, a)
	}
	if c.storageError {
		s.Error = "persistence_unavailable"
	}
	return s
}
