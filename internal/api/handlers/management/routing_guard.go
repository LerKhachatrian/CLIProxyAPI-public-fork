package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// guardRoutingMutation is an opt-in conditional write for legacy automation.
// Unmarked manual actions retain their existing authorization and behavior.
func (h *Handler) guardRoutingMutation(c *gin.Context) (func(), bool) {
	guard := strings.TrimSpace(c.GetHeader("X-CLIProxy-Routing-Guard"))
	if guard == "" {
		return func() {}, true
	}
	if guard != "legacy" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_routing_guard"})
		return func() {}, false
	}
	release, allowed := h.authManager.GuardLegacyRouting()
	if !allowed {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "family_routing_conflict",
			"message": "Legacy priority automation is unavailable while family routing is enabled.",
		})
	}
	return release, allowed
}
