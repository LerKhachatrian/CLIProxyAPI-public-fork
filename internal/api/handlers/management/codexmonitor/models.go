// Package codexmonitor owns passive display observations and shared, bounded
// provider-read scheduling. It has no access to credentials or routing writes.
package codexmonitor

import (
	"context"
	"time"
)

const (
	MaxAccounts      = 128
	MaxAccountGrants = 256
	MaxGrants        = 4096
	MaxBodyBytes     = 2 << 20
	MaxPendingAge    = 8 * time.Hour // exceeds two full 128-account queues at a 60-second gap
	UsageLane        = "usage"
	ResetLane        = "resets"
)

type Window struct {
	Kind            string    `json:"kind"`
	Label           string    `json:"label"`
	UsedPercent     float64   `json:"used_percent"`
	DurationSeconds int64     `json:"duration_seconds"`
	ResetAt         time.Time `json:"reset_at"`
}

type Usage struct {
	ObservedAt   time.Time `json:"observed_at"`
	Source       string    `json:"source"`
	Plan         string    `json:"plan_type,omitempty"`
	Windows      []Window  `json:"windows"`
	Allowed      *bool     `json:"allowed,omitempty"`
	LimitReached *bool     `json:"limit_reached,omitempty"`
}

// Grant is an allowlist, never an arbitrary upstream JSON object. Exact IDs are
// private cache/action context and must not appear in diagnostics or logs.
type Grant struct {
	ID              string     `json:"id"`
	ResetType       string     `json:"reset_type"`
	Status          string     `json:"status"`
	GrantedAt       *time.Time `json:"granted_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	RedeemStartedAt *time.Time `json:"redeem_started_at,omitempty"`
	RedeemedAt      *time.Time `json:"redeemed_at,omitempty"`
}

type Bank struct {
	ObservedAt     time.Time `json:"observed_at"`
	AvailableCount *int      `json:"available_count"`
	Credits        []Grant   `json:"credits"`
	Complete       bool      `json:"complete"`
}

// Identity is runtime-only. Key must be an opaque SHA-256 of stable account and
// plan identity; no token, label, email or auth-file contents enter the cache.
type Identity struct {
	Key       string
	AuthIndex string
	Enabled   bool
	Blocked   bool
	RetryAt   time.Time
	Priority  int
	Active    bool
	Passive   *Usage
}

type Policy struct {
	UsageSeconds int `json:"usage_seconds"` // 0 = manual-only
	ResetSeconds int `json:"reset_seconds"` // 0, 86400, 604800
	GapSeconds   int `json:"gap_seconds"`
}

type Request struct {
	Policy    Policy `json:"policy"`
	Refresh   string `json:"refresh,omitempty"`    // "usage" or "resets", not redemption
	AuthIndex string `json:"auth_index,omitempty"` // optional exact manual target
}

type Lane struct {
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	DueAt       time.Time `json:"due_at,omitempty"`
	RetryAt     time.Time `json:"retry_at,omitempty"`
	PendingAt   time.Time `json:"pending_at,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	Error       string    `json:"error,omitempty"`
	ErrorAt     time.Time `json:"error_at,omitempty"`
}

type Entry struct {
	Schema        int       `json:"schema"`
	Key           string    `json:"key"`
	Usage         *Usage    `json:"usage,omitempty"`
	Bank          *Bank     `json:"resets,omitempty"`
	UsageSchedule Lane      `json:"usage_schedule"`
	ResetSchedule Lane      `json:"reset_schedule"`
	Active        bool      `json:"active"`
	RetryAt       time.Time `json:"retry_at,omitempty"`       // account-wide monitoring floor, never routing state
	InvalidatedAt time.Time `json:"invalidated_at,omitempty"` // pre/post explicit action barrier
}

type Account struct {
	AuthIndex     string `json:"auth_index"`
	Identity      string `json:"identity"`
	Usage         *Usage `json:"usage,omitempty"`
	Bank          *Bank  `json:"resets,omitempty"`
	UsageSchedule Lane   `json:"usage_schedule"`
	ResetSchedule Lane   `json:"reset_schedule"`
	Blocked       bool   `json:"blocked"`
	Active        bool   `json:"active"`
}

type Snapshot struct {
	Schema     int       `json:"schema"`
	ObservedAt time.Time `json:"checked_at"`
	Accounts   []Account `json:"accounts"`
	Pending    int       `json:"pending"`
	InFlight   bool      `json:"in_flight"`
	NextStart  time.Time `json:"next_start,omitempty"`
	Attempted  string    `json:"attempted,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Result is sanitized by the adapter before returning to the coordinator.
type Result struct {
	Usage   *Usage
	Bank    *Bank
	Status  int
	RetryAt time.Time
}

type Fetch func(context.Context, Identity, string) Result

func NormalizePolicy(p Policy) Policy {
	if p.UsageSeconds != 0 {
		p.UsageSeconds = max(1200, min(7200, p.UsageSeconds))
	}
	if p.ResetSeconds != 0 && p.ResetSeconds != 604800 {
		p.ResetSeconds = 86400
	}
	p.GapSeconds = max(10, min(60, p.GapSeconds))
	return p
}
