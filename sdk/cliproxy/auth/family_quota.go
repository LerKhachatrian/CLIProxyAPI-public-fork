package auth

import (
	"context"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// FamilyQuotaObservation is the narrow typed projection consumed by routing.
// The adapter must use the exact registered identity/epoch and memory only.
type FamilyQuotaObservation struct {
	Available           bool
	Identity            string
	RegistrationEpoch   uint64
	ObservedAt          time.Time
	WeeklyKnown         bool
	WeeklyUsedPercent   float64
	WeeklyResetAt       time.Time
	ShortExhaustedUntil time.Time
	ShortObservedAt     time.Time
}

type familyExhaustion struct {
	ObservedAt time.Time `json:"observed_at"`
	ResetAt    time.Time `json:"reset_at"`
}

func (r *familyRouter) quotaEligibleLocked(auth *Auth, now time.Time) (bool, error) {
	observation := r.quotaSource(auth)
	identity := auth.CodexAccountIdentity()
	if !observation.Available || observation.Identity != identity || observation.RegistrationEpoch != auth.RegistrationEpoch {
		return false, familyRequestError("family_quota_unavailable", "account quota projection is unavailable or belongs to another registration", http.StatusServiceUnavailable)
	}
	if old, exists := r.exhausted[identity]; exists && !old.ResetAt.After(now) {
		delete(r.exhausted, identity)
		r.markDirtyLocked()
	}
	validWeekly := validFamilyWeeklyObservation(observation, now)
	if validWeekly && observation.WeeklyUsedPercent >= 100 {
		next := familyExhaustion{ObservedAt: observation.ObservedAt.UTC(), ResetAt: observation.WeeklyResetAt.UTC()}
		old, exists := r.exhausted[identity]
		if !exists && len(r.exhausted) >= 128 {
			return false, familyRequestError("family_quota_capacity", "confirmed weekly exclusion capacity exceeded", http.StatusServiceUnavailable)
		}
		if !exists || next.ObservedAt.After(old.ObservedAt) && next.ResetAt.After(old.ResetAt) {
			r.exhausted[identity] = next
			r.markDirtyLocked()
		}
	}
	// Positive, partial, stale and absent reads cannot erase a confirmed empty
	// window before its reset. Credit redemption requires separate reset proof.
	if evidence, exists := r.exhausted[identity]; exists && evidence.ResetAt.After(now) {
		return false, nil
	}
	if !observation.ShortObservedAt.IsZero() && !observation.ShortObservedAt.After(now) && observation.ShortExhaustedUntil.After(now) {
		return false, nil
	}
	return true, nil
}

func validFamilyWeeklyObservation(observation FamilyQuotaObservation, now time.Time) bool {
	return observation.WeeklyKnown && !observation.ObservedAt.IsZero() && !observation.ObservedAt.After(now) &&
		observation.WeeklyResetAt.After(now) && observation.WeeklyResetAt.Sub(observation.ObservedAt) <= 10*24*time.Hour &&
		!math.IsNaN(observation.WeeklyUsedPercent) && !math.IsInf(observation.WeeklyUsedPercent, 0) &&
		observation.WeeklyUsedPercent >= 0 && observation.WeeklyUsedPercent <= 100
}

// SetFamilyQuotaSource installs a typed read-only in-memory adapter. The source
// owns no allocation policy and must not open storage or call a provider.
func (m *Manager) SetFamilyQuotaSource(source func(*Auth) FamilyQuotaObservation) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.familyQuotaSource = source
	m.mu.Unlock()
}

func (m *Manager) familyQuota(auth *Auth) FamilyQuotaObservation {
	m.mu.RLock()
	source := m.familyQuotaSource
	m.mu.RUnlock()
	if source == nil {
		return FamilyQuotaObservation{}
	}
	return source(auth)
}

func (m *Manager) familyAccount(index, identity, model string) (*Auth, bool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var found *Auth
	for _, candidate := range m.auths {
		if candidate == nil || !strings.EqualFold(candidate.Provider, "codex") || candidate.Index != index || candidate.CodexAccountIdentity() != identity {
			continue
		}
		if found != nil {
			return nil, false, false
		}
		found = candidate
	}
	if found == nil {
		return nil, false, false
	}
	supported := model == "" || m.authSupportsRouteModel(registry.GetGlobalRegistry(), found, model)
	blocked, _, _ := isAuthBlockedForModel(found, m.selectionModelForAuth(found, model), time.Now())
	return found.Clone(), supported, blocked
}

func (m *Manager) attachFamilySelector(selector Selector) {
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity.family != nil {
		affinity.family.attach(m.familyQuota, m.familyAccount)
	}
}

func (m *Manager) ReconfigureFamilyRouting(cfg FamilyRoutingConfig) bool {
	if m == nil {
		return false
	}
	m.selectorMu.Lock()
	defer m.selectorMu.Unlock()
	selector, ok := m.Selector().(*SessionAffinitySelector)
	return ok && selector.family != nil && selector.family.reconfigure(cfg)
}

func (m *Manager) FamilyRoutingStatus() (FamilyRoutingStatus, bool) {
	if m == nil {
		return FamilyRoutingStatus{}, false
	}
	selector, ok := m.Selector().(*SessionAffinitySelector)
	if !ok || selector.family == nil {
		return FamilyRoutingStatus{}, false
	}
	status := selector.family.status()
	now := time.Now()
	selector.family.mu.Lock()
	excluded := make(map[string]bool, len(selector.family.exhausted))
	for identity, evidence := range selector.family.exhausted {
		excluded[identity] = evidence.ResetAt.After(now)
	}
	selector.family.mu.Unlock()
	status.WeeklyExcludedAccounts = 0
	for _, auth := range m.List() {
		if auth == nil || !strings.EqualFold(auth.Provider, "codex") || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		quota := m.familyQuota(auth)
		identity := auth.CodexAccountIdentity()
		known := quota.Available && quota.Identity == identity && quota.RegistrationEpoch == auth.RegistrationEpoch && validFamilyWeeklyObservation(quota, now)
		if excluded[identity] || known && quota.WeeklyUsedPercent >= 100 {
			status.WeeklyExcludedAccounts++
		} else if !known {
			status.UnknownQuotaAccounts++
		}
	}
	return status, true
}

// ResetSessionAffinityContext keeps legacy reset semantics and makes a family
// reset durable before reporting success. Membership survives the reset.
func (m *Manager) ResetSessionAffinityContext(ctx context.Context) (cleared, preserved int, family, enabled bool, err error) {
	if m == nil {
		return 0, 0, false, false, nil
	}
	m.selectorMu.Lock()
	defer m.selectorMu.Unlock()
	selector := m.Selector()
	controller, enabled := selector.(sessionAffinityController)
	if !enabled {
		return 0, 0, false, false, nil
	}
	cleared = controller.ResetSessionAffinity()
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity.family != nil {
		r := affinity.family
		r.mu.Lock()
		sequence, timeout := r.sequence, r.cfg.AdmissionTimeout
		preserved = len(r.families)
		r.mu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		err = r.awaitDurable(waitCtx, sequence)
		return cleared, preserved, true, true, err
	}
	return cleared, 0, false, true, nil
}
