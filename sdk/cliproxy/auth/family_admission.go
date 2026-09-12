package auth

import (
	"context"
	"net/http"
	"sync"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type familyWaiter struct {
	token   *familySelection
	done    chan struct{}
	err     error
	granted bool
	retired bool
}

type familyAccountAdmission struct {
	inFlight int
	queued   int
	waiters  map[string][]*familyWaiter
	order    []string
	active   map[string]int
}

type familyAdmissionLease struct {
	router     *familyRouter
	token      *familySelection
	account    *familyAccountAdmission
	once       sync.Once
	stopCancel func() bool
	released   bool // protected by router.mu
}

func (r *familyRouter) validateSelectionLocked(token *familySelection) (uint64, error) {
	if errUnavailable := r.unavailableLocked(); errUnavailable != nil {
		return 0, errUnavailable
	}
	entry := r.families[token.family]
	if entry == nil || entry.Generation != token.generation || entry.Identity != token.identity || entry.AuthIndex != token.authIndex {
		return 0, familyReselectError()
	}
	current, supported, blocked := r.lookup(token.authIndex, token.identity, token.model)
	if current == nil || current.ID != token.authID || current.RegistrationEpoch != token.epoch ||
		current.Disabled || current.Status == StatusDisabled {
		if entry.epoch != 0 && entry.epoch != token.epoch {
			return 0, familyReselectError()
		}
		r.clearAssignmentLocked(token.family, entry)
		return 0, familyReselectError()
	}
	if !supported {
		return 0, familyRequestError("family_account_incompatible", "the family's account no longer supports this model", http.StatusConflict)
	}
	now := r.clock()
	eligible, errQuota := r.quotaEligibleLocked(current, now)
	if errQuota != nil {
		return 0, errQuota
	}
	if !eligible || blocked {
		r.clearAssignmentLocked(token.family, entry)
		return 0, familyReselectError()
	}
	return max(token.sequence, r.sequence), nil
}

// acquire waits only before a provider connection. It does not carry its
// acquisition deadline into the upstream execution or a returned stream.
func (r *familyRouter) acquire(ctx context.Context, token *familySelection) (*familyAdmissionLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	deadline := r.cfg.AdmissionTimeout
	r.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		r.mu.Lock()
		sequence, errValidate := r.validateSelectionLocked(token)
		r.mu.Unlock()
		if errValidate != nil {
			return nil, errValidate
		}
		if errPersist := r.awaitDurable(waitCtx, sequence); errPersist != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, familyRequestError("family_admission_unavailable", "family assignment could not become durable before admission", http.StatusServiceUnavailable)
		}
		r.mu.Lock()
		latest, errValidate := r.validateSelectionLocked(token)
		if errValidate != nil {
			r.mu.Unlock()
			return nil, errValidate
		}
		if latest > r.durable {
			r.mu.Unlock()
			continue
		}
		account := r.accounts[token.identity]
		if account == nil {
			if len(r.accounts) >= 128 {
				r.mu.Unlock()
				return nil, familyRequestError("family_account_capacity", "family admission account capacity exceeded", http.StatusServiceUnavailable)
			}
			account = &familyAccountAdmission{waiters: map[string][]*familyWaiter{}, active: map[string]int{}}
			r.accounts[token.identity] = account
		}
		waiter := &familyWaiter{token: token, done: make(chan struct{})}
		if account.inFlight < r.cfg.MaxConcurrentPerAccount && account.queued == 0 {
			r.grantLocked(account, waiter)
		} else {
			if account.queued >= r.cfg.MaxQueuedPerAccount {
				r.mu.Unlock()
				return nil, familyRequestError("family_queue_full", "this account's bounded family admission queue is full", http.StatusServiceUnavailable)
			}
			if len(account.waiters[token.family]) == 0 {
				account.order = append(account.order, token.family)
			}
			account.waiters[token.family] = append(account.waiters[token.family], waiter)
			account.queued++
			r.signalLocked()
			r.dispatchLocked(account)
		}
		r.mu.Unlock()
		select {
		case <-waitCtx.Done():
			r.mu.Lock()
			r.retireWaiterLocked(account, waiter)
			r.mu.Unlock()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, familyRequestError("family_admission_timeout", "family admission timed out before any upstream connection", http.StatusServiceUnavailable)
		case <-waiter.done:
		}
		r.mu.Lock()
		if waiter.err != nil {
			err := waiter.err
			r.mu.Unlock()
			return nil, err
		}
		_, errValidate = r.validateSelectionLocked(token)
		if errValidate != nil || ctx.Err() != nil {
			r.retireWaiterLocked(account, waiter)
			r.mu.Unlock()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errValidate
		}
		lease := &familyAdmissionLease{router: r, token: token, account: account}
		r.mu.Unlock()
		// Register cancellation without racing initialization of stopCancel.
		ready := make(chan struct{})
		lease.stopCancel = context.AfterFunc(ctx, func() { <-ready; lease.release() })
		close(ready)
		return lease, nil
	}
}

func (r *familyRouter) grantLocked(account *familyAccountAdmission, waiter *familyWaiter) {
	account.inFlight++
	account.active[waiter.token.family]++
	waiter.granted = true
	close(waiter.done)
	r.signalLocked()
}

// Round robin among families, FIFO within a family. Each dispatch moves that
// family's remaining queue to the tail, so one large fan-out cannot monopolize
// all subsequent slots. A single active family may use the configured capacity.
func (r *familyRouter) dispatchLocked(account *familyAccountAdmission) {
	if r.closed {
		return
	}
	for account.inFlight < r.cfg.MaxConcurrentPerAccount && len(account.order) > 0 {
		key := account.order[0]
		account.order = account.order[1:]
		waiters := account.waiters[key]
		waiter := waiters[0]
		waiters = waiters[1:]
		account.queued--
		if len(waiters) == 0 {
			delete(account.waiters, key)
		} else {
			account.waiters[key] = waiters
			account.order = append(account.order, key)
		}
		r.grantLocked(account, waiter)
	}
}

func (r *familyRouter) retireWaiterLocked(account *familyAccountAdmission, waiter *familyWaiter) {
	if waiter.retired {
		return
	}
	waiter.retired = true
	r.signalLocked()
	key := waiter.token.family
	if waiter.granted {
		account.inFlight--
		account.active[key]--
		if account.active[key] == 0 {
			delete(account.active, key)
		}
	} else {
		waiters := account.waiters[key]
		for index, queued := range waiters {
			if queued == waiter {
				account.waiters[key] = append(waiters[:index], waiters[index+1:]...)
				account.queued--
				break
			}
		}
		if len(account.waiters[key]) == 0 {
			delete(account.waiters, key)
			r.removeFamilyOrderLocked(account, key)
		}
	}
	r.dispatchLocked(account)
	if account.inFlight == 0 && account.queued == 0 {
		delete(r.accounts, waiter.token.identity)
	}
}

func (r *familyRouter) removeFamilyOrderLocked(account *familyAccountAdmission, key string) {
	for index, family := range account.order {
		if family == key {
			account.order = append(account.order[:index], account.order[index+1:]...)
			return
		}
	}
}

func (r *familyRouter) cancelFamilyWaitersLocked(key string, err error) {
	r.signalLocked()
	for identity, account := range r.accounts {
		for _, waiter := range account.waiters[key] {
			waiter.err, waiter.retired = err, true
			account.queued--
			close(waiter.done)
		}
		delete(account.waiters, key)
		r.removeFamilyOrderLocked(account, key)
		if account.inFlight == 0 && account.queued == 0 {
			delete(r.accounts, identity)
		}
	}
}

func (r *familyRouter) familyBusyLocked(key string) bool {
	for _, account := range r.accounts {
		if account.active[key] > 0 || len(account.waiters[key]) > 0 {
			return true
		}
	}
	return false
}

func (lease *familyAdmissionLease) release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		if lease.stopCancel != nil {
			lease.stopCancel()
		}
		r := lease.router
		r.mu.Lock()
		lease.released = true
		r.retireWaiterLocked(lease.account, &familyWaiter{token: lease.token, granted: true})
		r.mu.Unlock()
	})
}

// validate is called again at every real transport attempt, including an
// executor's bootstrap reconnect. It is memory-only and never replays output.
func (lease *familyAdmissionLease) validate() error {
	r := lease.router
	r.mu.Lock()
	defer r.mu.Unlock()
	if lease.released {
		return familyReselectError()
	}
	_, err := r.validateSelectionLocked(lease.token)
	return err
}

func admitFamilyRequest(ctx context.Context, auth *Auth, opts cliproxyexecutor.Options) (context.Context, func(), error) {
	token := familyTokenFromOptions(opts)
	if token == nil {
		return ctx, func() {}, nil
	}
	if auth == nil || auth.ID != token.authID || auth.CodexAccountIdentity() != token.identity || auth.RegistrationEpoch != token.epoch {
		return ctx, func() {}, familyReselectError()
	}
	lease, err := token.router.acquire(ctx, token)
	if err != nil {
		return ctx, func() {}, err
	}
	return cliproxyexecutor.WithUpstreamAttemptGuard(ctx, lease.validate), lease.release, nil
}

func executeStreamWithAdmissionGuard(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := cliproxyexecutor.CheckUpstreamAttempt(ctx); err != nil {
		return nil, err
	}
	return executor.ExecuteStream(ctx, auth, req, opts)
}

func countWithFamilyAdmission(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx, release, err := admitFamilyRequest(ctx, auth, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer release()
	if errGuard := cliproxyexecutor.CheckUpstreamAttempt(ctx); errGuard != nil {
		return cliproxyexecutor.Response{}, errGuard
	}
	return executor.CountTokens(ctx, auth, req, opts)
}
