package codexmonitor

import (
	"errors"
	"math"
	"os"
	"time"
)

// RoutingQuota is a typed, memory-only view. It grants no scheduling or write
// authority and deliberately keeps additional/model/Fast limits out of the
// base weekly entitlement. Unknown and expired observations remain unknown.
type RoutingQuota struct {
	Available       bool
	ObservedAt      time.Time // Weekly observation, or Short when weekly is unknown.
	ShortObservedAt time.Time
	Weekly          *Window
	Short           *Window
}

type routingWeeklyExhaustion struct {
	ObservedAt time.Time `json:"observed_at"`
	Window     Window    `json:"window"`
}

type routingWeeklyState struct {
	Schema  int                                `json:"schema"`
	Windows map[string]routingWeeklyExhaustion `json:"windows"`
}

// A companion outside the strict v1 cache directory follows the existing reset
// pacing precedent. Older binaries neither parse nor delete this safety state.
type routingWeeklyStore interface {
	loadRoutingWeekly() (map[string]routingWeeklyExhaustion, error)
	saveRoutingWeekly(map[string]routingWeeklyExhaustion) error
}

func (s *FileStore) loadRoutingWeekly() (map[string]routingWeeklyExhaustion, error) {
	state := routingWeeklyState{Schema: 1, Windows: map[string]routingWeeklyExhaustion{}}
	err := readJSON(s.dir+".routing-weekly.v1.json", 128<<10, &state)
	if errors.Is(err, os.ErrNotExist) {
		return state.Windows, nil
	}
	if err != nil || state.Schema != 1 || state.Windows == nil || len(state.Windows) > MaxAccounts {
		return nil, errors.New("routing quota cache invalid; preserved")
	}
	for key, evidence := range state.Windows {
		if !cacheKey.MatchString(key) || !validRoutingWindow(evidence.Window, evidence.ObservedAt) ||
			evidence.Window.Kind != "weekly" || evidence.Window.UsedPercent < 100 || evidence.ObservedAt.After(time.Now().Add(5*time.Minute)) {
			return nil, errors.New("routing quota evidence invalid; preserved")
		}
	}
	return state.Windows, nil
}

func (s *FileStore) saveRoutingWeekly(windows map[string]routingWeeklyExhaustion) error {
	return s.writeFile(s.dir+".routing-weekly.v1.json", routingWeeklyState{Schema: 1, Windows: windows}, 128<<10)
}

func (c *Coordinator) loadRoutingWeekly() error {
	c.routingWeekly = map[string]routingWeeklyExhaustion{}
	if store, ok := c.store.(routingWeeklyStore); ok {
		var err error
		c.routingWeekly, err = store.loadRoutingWeekly()
		return err
	}
	return nil
}

func validRoutingWindow(window Window, observed time.Time) bool {
	if observed.IsZero() || !window.ResetAt.After(observed) || math.IsNaN(window.UsedPercent) ||
		math.IsInf(window.UsedPercent, 0) || window.UsedPercent < 0 || window.UsedPercent > 100 {
		return false
	}
	switch window.Kind {
	case "weekly":
		return window.DurationSeconds >= 500000 && window.DurationSeconds <= 800000 && window.ResetAt.Sub(observed) <= 10*24*time.Hour
	case "five_hour":
		return window.DurationSeconds >= 14000 && window.DurationSeconds <= 22000 && window.ResetAt.Sub(observed) <= 24*time.Hour
	}
	return false
}

func (c *Coordinator) observeRoutingWeekly(key string, usage *Usage, now time.Time) {
	if usage == nil || !cacheKey.MatchString(key) || usage.ObservedAt.After(now) {
		return
	}
	for _, window := range usage.Windows {
		if window.Kind != "weekly" || window.UsedPercent < 100 || !window.ResetAt.After(now) || !validRoutingWindow(window, usage.ObservedAt) {
			continue
		}
		old, exists := c.routingWeekly[key]
		if !exists && len(c.routingWeekly) >= MaxAccounts {
			c.pruneRoutingWeekly(now)
			if len(c.routingWeekly) >= MaxAccounts {
				c.routingError = errors.New("routing quota evidence capacity exceeded")
				return
			}
		}
		if !exists || window.ResetAt.After(old.Window.ResetAt) && usage.ObservedAt.After(old.ObservedAt) {
			c.routingWeekly[key] = routingWeeklyExhaustion{ObservedAt: usage.ObservedAt, Window: window}
			c.routingDirty = true
		}
	}
}

func (c *Coordinator) pruneRoutingWeekly(now time.Time) {
	for key, evidence := range c.routingWeekly {
		if !evidence.Window.ResetAt.After(now) {
			delete(c.routingWeekly, key)
			c.routingDirty = true
		}
	}
}

func (c *Coordinator) flushRoutingWeekly(now time.Time) error {
	if c.routingError != nil {
		return c.routingError
	}
	c.pruneRoutingWeekly(now)
	if !c.routingDirty {
		return nil
	}
	if store, ok := c.store.(routingWeeklyStore); ok {
		if err := store.saveRoutingWeekly(c.routingWeekly); err != nil {
			return errors.New("routing quota persistence unavailable")
		}
	}
	c.routingDirty = false
	return nil
}

// RoutingProjection does not reconcile, flush, schedule, open storage or fetch.
// Passive signals belong to the exact auth snapshot passed by the adapter.
func (c *Coordinator) RoutingProjection(key string, passive *Usage, now time.Time) RoutingQuota {
	if c == nil {
		return RoutingQuota{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !cacheKey.MatchString(key) || c.storageError || c.routingError != nil {
		return RoutingQuota{}
	}
	var stored *Usage
	var invalidated time.Time
	if entry := c.entries[key]; entry != nil {
		stored, invalidated = entry.Usage, entry.InvalidatedAt
	}
	projection := RoutingQuota{Available: true}
	for _, usage := range []*Usage{stored, passive} {
		if usage == nil || usage.ObservedAt.IsZero() || usage.ObservedAt.After(now) || !usage.ObservedAt.After(invalidated) {
			continue
		}
		c.observeRoutingWeekly(key, usage, now)
		for _, window := range usage.Windows {
			if !validRoutingWindow(window, usage.ObservedAt) || !window.ResetAt.After(now) || now.Sub(usage.ObservedAt) > 48*time.Hour {
				continue
			}
			copy := window
			switch window.Kind {
			case "weekly":
				if projection.Weekly == nil || usage.ObservedAt.After(projection.ObservedAt) {
					projection.Weekly, projection.ObservedAt = &copy, usage.ObservedAt
				}
			case "five_hour":
				if projection.Short == nil || usage.ObservedAt.After(projection.ShortObservedAt) {
					projection.Short, projection.ShortObservedAt = &copy, usage.ObservedAt
				}
			}
		}
	}
	if evidence, ok := c.routingWeekly[key]; ok && evidence.Window.ResetAt.After(now) && !evidence.ObservedAt.After(now) {
		copy := evidence.Window
		projection.Weekly, projection.ObservedAt = &copy, evidence.ObservedAt
	}
	if projection.Weekly == nil {
		projection.ObservedAt = projection.ShortObservedAt
	}
	projection.Available = !c.storageError && c.routingError == nil
	return projection
}
