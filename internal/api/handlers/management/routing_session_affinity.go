package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// GetSessionAffinity reports whether the active routing selector uses session
// affinity and how many non-secret session keys are currently cached.
func (h *Handler) GetSessionAffinity(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	sessionKeys, enabled := h.authManager.SessionAffinityStatus()
	c.JSON(http.StatusOK, gin.H{
		"enabled":      enabled,
		"session_keys": sessionKeys,
	})
}

// ResetSessionAffinity clears cached session-to-credential bindings. Requests
// that already selected a credential continue; their result callbacks cannot
// recreate a binding removed by this reset.
func (h *Handler) ResetSessionAffinity(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	cleared, enabled := h.authManager.ResetSessionAffinity()
	if !enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "session affinity is not enabled"})
		return
	}

	log.WithField("cleared_session_keys", cleared).Info("session-affinity bindings reset through management API")
	c.JSON(http.StatusOK, gin.H{
		"status":               "ok",
		"cleared_session_keys": cleared,
	})
}
