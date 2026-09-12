package auth

import (
	"strings"
	"sync"
)

// IsFamilyRoutingStrategy identifies the explicit opt-in strategy.
func IsFamilyRoutingStrategy(strategy string) bool {
	return strings.EqualFold(strings.TrimSpace(strategy), "family-balanced")
}

// SetRoutingFamilyConfigured publishes a configuration commit before its
// asynchronous runtime work. Legacy automation must not enter that gap.
func (m *Manager) SetRoutingFamilyConfigured(strategy string) {
	if m == nil {
		return
	}
	m.routingGuardMu.Lock()
	m.routingConfiguredFamily = IsFamilyRoutingStrategy(strategy)
	m.routingGuardMu.Unlock()
}

// ReserveFamilyRoutingConfig serializes the start of a family-mode save with
// guarded mutations. The reservation lasts through the matching reload, even
// if an older in-flight reload briefly publishes a legacy configuration.
func (m *Manager) ReserveFamilyRoutingConfig(strategy string) func() {
	if m == nil || !IsFamilyRoutingStrategy(strategy) {
		return func() {}
	}
	m.routingGuardMu.Lock()
	m.routingPendingFamily++
	m.routingGuardMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.routingGuardMu.Lock()
			m.routingPendingFamily--
			m.routingGuardMu.Unlock()
		})
	}
}

// GuardLegacyRouting holds a read lease through one legacy automation mutation.
// Configuration commits and selector replacement take the matching write lock.
func (m *Manager) GuardLegacyRouting() (release func(), allowed bool) {
	if m == nil {
		return func() {}, false
	}
	m.routingGuardMu.RLock()
	if m.routingConfiguredFamily || m.routingPendingFamily > 0 || m.FamilyRoutingEnabled() {
		m.routingGuardMu.RUnlock()
		return func() {}, false
	}
	return m.routingGuardMu.RUnlock, true
}

func (m *Manager) FamilyRoutingEnabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	controller, ok := selector.(interface{ FamilyRoutingEnabled() bool })
	return ok && controller.FamilyRoutingEnabled()
}

// EffectiveRoutingStrategy reports the selector actually serving requests.
func (m *Manager) EffectiveRoutingStrategy() string {
	selector := m.Selector()
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		if affinity.FamilyRoutingEnabled() {
			return "family-balanced"
		}
		selector = affinity.fallback
	}
	switch selector.(type) {
	case *RoundRobinSelector:
		return "round-robin"
	case *WeightedRoundRobinSelector:
		return "weighted-round-robin"
	case *FillFirstSelector:
		return "fill-first"
	default:
		return "custom"
	}
}
