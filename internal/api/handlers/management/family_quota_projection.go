package management

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management/codexmonitor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// InitializeFamilyQuotaProjection opens the existing monitor once at startup or
// mode enable. The installed callback is memory-only, including cold selection.
func (h *Handler) InitializeFamilyQuotaProjection() {
	if h == nil || h.authManager == nil {
		return
	}
	monitor, err := h.monitor()
	if err != nil {
		h.authManager.SetFamilyQuotaSource(nil)
		return
	}
	h.authManager.SetFamilyQuotaSource(func(auth *coreauth.Auth) coreauth.FamilyQuotaObservation {
		identity := auth.CodexAccountIdentity()
		quota := monitor.RoutingProjection(identity, codexmonitor.FromSignals(auth.Quota.Signals, auth.Quota.ObservedAt), time.Now())
		result := coreauth.FamilyQuotaObservation{Available: quota.Available, Identity: identity,
			RegistrationEpoch: auth.RegistrationEpoch, ObservedAt: quota.ObservedAt}
		if quota.Weekly != nil {
			result.WeeklyKnown, result.WeeklyUsedPercent, result.WeeklyResetAt = true, quota.Weekly.UsedPercent, quota.Weekly.ResetAt
		}
		if quota.Short != nil && quota.Short.UsedPercent >= 100 {
			result.ShortExhaustedUntil = quota.Short.ResetAt
			result.ShortObservedAt = quota.ShortObservedAt
		}
		return result
	})
}
