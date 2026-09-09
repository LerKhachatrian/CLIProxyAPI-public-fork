package codexmonitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

// ParseRequest rejects ambiguity before a request can queue any provider work.
func ParseRequest(data []byte) (Request, error) {
	var req Request
	if len(data) > 4096 {
		return req, errors.New("monitor request too large")
	}
	if _, err := strictObject(data); err != nil {
		return req, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return req, err
	}
	if req.Refresh != "" && req.Refresh != UsageLane && req.Refresh != ResetLane || len(req.AuthIndex) > 160 || req.AuthIndex != "" && req.Refresh == "" {
		return req, errors.New("invalid monitor request")
	}
	return req, nil
}

func number(v any) (float64, bool) {
	var f float64
	var err error
	switch n := v.(type) {
	case json.Number:
		f, err = n.Float64()
	case string:
		f, err = strconv.ParseFloat(n, 64)
	case float64:
		f = n
	default:
		return 0, false
	}
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func first(m map[string]any, names ...string) any {
	for _, name := range names {
		if v, ok := m[name]; ok && v != nil {
			return v
		}
	}
	return nil
}

func boolean(v any) *bool {
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

func booleanHeader(v string) *bool {
	if v != "true" && v != "false" && v != "1" && v != "0" {
		return nil
	}
	b := v == "true" || v == "1"
	return &b
}

func instant(v any) *time.Time {
	if s, ok := v.(string); ok {
		if len(s) <= 64 {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				u := t.UTC()
				return &u
			}
		}
	}
	n, ok := number(v)
	if !ok || n < 1 || n > 253402300799 {
		return nil
	}
	t := time.Unix(int64(n), int64((n-math.Floor(n))*1e9)).UTC()
	return &t
}

func plan(v any) string {
	s, _ := v.(string)
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "prolite", "pro_lite":
		return "pro_5x"
	case "free", "plus", "pro", "pro_5x", "pro_20x", "team", "business", "enterprise", "edu":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return "unknown"
	}
}

func window(used, duration any, reset *time.Time, additional bool, label string) *Window {
	u, validUsed := number(used)
	d, validDuration := number(duration)
	if !validUsed || u < 0 || u > 100 || !validDuration || d < 1 || d > 366*86400 || d != math.Floor(d) || reset == nil {
		return nil
	}
	kind := "unknown"
	if d >= 14000 && d <= 22000 {
		kind, label = "five_hour", "5 hour"
	} else if d >= 500000 && d <= 800000 {
		kind, label = "weekly", "Weekly"
	} else if d >= 2000000 {
		kind, label = "monthly", "Monthly"
	}
	if additional {
		kind, label = "additional", "Additional quota"
	}
	return &Window{Kind: kind, Label: label, UsedPercent: u, DurationSeconds: int64(d), ResetAt: *reset}
}

func usageWindow(v any, now time.Time, additional bool) *Window {
	m := object(v)
	duration := first(m, "limit_window_seconds", "duration_seconds", "windowDurationSeconds")
	if duration == nil {
		if n, ok := number(first(m, "windowDurationMins", "window_minutes")); ok {
			duration = n * 60
		}
	}
	reset := instant(first(m, "reset_at", "resetAt", "resetsAt"))
	if reset == nil {
		if n, ok := number(first(m, "reset_after_seconds", "resetAfterSeconds")); ok && n >= 0 && n <= 366*86400 {
			t := now.Add(time.Duration(n * float64(time.Second)))
			reset = &t
		}
	}
	used := first(m, "used_percent", "usedPercent")
	if used == nil {
		if n, ok := number(first(m, "remaining_percent", "remainingPercent")); ok {
			used = 100 - n
		}
	}
	return window(used, duration, reset, additional, "Quota")
}

func appendWindows(u *Usage, v any, now time.Time, additional bool) {
	m := object(v)
	p, s := first(m, "primary_window", "primaryWindow", "primary"), first(m, "secondary_window", "secondaryWindow", "secondary")
	if p == nil && s == nil {
		p = m
	}
	for _, value := range []any{p, s} {
		if w := usageWindow(value, now, additional); w != nil {
			u.Windows = append(u.Windows, *w)
		}
	}
}

// ParseUsage retains only normalized fields, never an upstream response body.
func ParseUsage(data []byte, observedAt time.Time) (*Usage, error) {
	m, err := strictObject(data)
	if err != nil {
		return nil, err
	}
	u := &Usage{ObservedAt: observedAt.UTC(), Source: "provider_check", Plan: plan(first(m, "plan_type", "planType")), Windows: []Window{}}
	rate := object(first(m, "rate_limit", "rateLimit"))
	u.Allowed = boolean(rate["allowed"])
	u.LimitReached = boolean(first(rate, "limit_reached", "limitReached"))
	appendWindows(u, rate, observedAt, false)
	if list, ok := first(m, "additional_rate_limits", "additionalRateLimits").([]any); ok {
		for _, value := range list[:min(len(list), 8)] {
			entry := object(value)
			nested := first(entry, "rate_limit", "rateLimit")
			if nested == nil {
				nested = entry
			}
			appendWindows(u, nested, observedAt, true)
		}
	}
	if !useful(u) || !uniqueMainWindows(u) {
		return nil, errors.New("main quota windows not reported")
	}
	return u, nil
}

// FromSignals reads one bounded HTTP/WS observation, not a merge of observations.
// Retry-After/credits/additional-only signals cannot supply a main entitlement.
func FromSignals(signals map[string]string, at time.Time) *Usage {
	if at.IsZero() || len(signals) > 64 {
		return nil
	}
	h := make(http.Header, len(signals))
	for k, v := range signals {
		if len(v) <= 512 {
			h.Set(k, v)
		}
	}
	u := &Usage{ObservedAt: at.UTC(), Source: "passive", Plan: plan(h.Get("X-Codex-Plan-Type")), Windows: []Window{},
		Allowed: booleanHeader(h.Get("X-Codex-Allowed")), LimitReached: booleanHeader(h.Get("X-Codex-Limit-Reached"))}
	prefixes := []string{"X-Codex-Primary-", "X-Codex-Secondary-"}
	var extra []string
	for key := range h {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-codex-") && strings.HasSuffix(lower, "-used-percent") &&
			lower != "x-codex-primary-used-percent" && lower != "x-codex-secondary-used-percent" {
			extra = append(extra, key[:len(key)-len("Used-Percent")])
		}
	}
	sort.Strings(extra)
	prefixes = append(prefixes, extra[:min(16, len(extra))]...)
	for i, prefix := range prefixes {
		minutes, ok := number(h.Get(prefix + "Window-Minutes"))
		if !ok {
			continue
		}
		reset := instant(h.Get(prefix + "Reset-At"))
		if reset == nil {
			if n, valid := number(h.Get(prefix + "Reset-After-Seconds")); valid && n >= 0 && n <= 366*86400 {
				t := at.Add(time.Duration(n * float64(time.Second)))
				reset = &t
			}
		}
		if w := window(h.Get(prefix+"Used-Percent"), minutes*60, reset, i >= 2, "Quota"); w != nil {
			u.Windows = append(u.Windows, *w)
		}
	}
	if !useful(u) || !uniqueMainWindows(u) {
		return nil
	}
	return u
}

func useful(u *Usage) bool {
	if u != nil {
		for _, w := range u.Windows {
			if w.Kind == "five_hour" || w.Kind == "weekly" || w.Kind == "monthly" {
				return true
			}
		}
	}
	return false
}

func uniqueMainWindows(u *Usage) bool {
	seen := map[string]bool{}
	for _, w := range u.Windows {
		if w.Kind == "additional" {
			continue
		}
		if seen[w.Kind] {
			return false
		}
		seen[w.Kind] = true
	}
	return true
}

func ParseBank(data []byte, observedAt time.Time) (*Bank, error) {
	m, err := strictObject(data)
	if err != nil {
		return nil, err
	}
	b := &Bank{ObservedAt: observedAt.UTC(), Credits: []Grant{}, Complete: true}
	n, valid := number(m["available_count"])
	if valid && n >= 0 && n <= MaxAccountGrants && math.Floor(n) == n {
		count := int(n)
		b.AvailableCount = &count
	} else {
		b.Complete = false
	}
	list, ok := m["credits"].([]any)
	if !ok || len(list) > MaxAccountGrants {
		return nil, errors.New("reset inventory shape or capacity invalid")
	}
	seen := make(map[string]bool, len(list))
	available := 0
	for _, value := range list {
		entry := object(value)
		id, _ := entry["id"].(string)
		if !identifier.MatchString(id) || seen[id] {
			return nil, errors.New("reset inventory identity invalid")
		}
		seen[id] = true
		g := Grant{ID: id, ResetType: "unsupported", Status: "unknown", GrantedAt: instant(entry["granted_at"]), ExpiresAt: instant(entry["expires_at"]),
			RedeemStartedAt: instant(entry["redeem_started_at"]), RedeemedAt: instant(entry["redeemed_at"])}
		resetType, _ := entry["reset_type"].(string)
		if resetType == "codex_rate_limits" {
			g.ResetType = "codex_rate_limits"
		} else {
			b.Complete = false
		}
		status, _ := entry["status"].(string)
		switch status {
		case "available", "redeemed", "expired", "redeeming", "in_progress":
			g.Status = status
		default:
			b.Complete = false
		}
		if g.Status == "available" {
			available++
			if g.RedeemStartedAt != nil || g.RedeemedAt != nil {
				b.Complete = false
			}
		}
		if g.GrantedAt == nil || g.ExpiresAt == nil || !g.ExpiresAt.After(*g.GrantedAt) {
			b.Complete = false
		}
		for _, name := range []string{"redeem_started_at", "redeemed_at"} {
			if entry[name] != nil && instant(entry[name]) == nil {
				b.Complete = false
			}
		}
		if g.Status == "redeemed" && g.RedeemedAt == nil {
			b.Complete = false
		}
		if g.GrantedAt != nil && g.GrantedAt.After(observedAt) {
			b.Complete = false
		}
		for _, at := range []*time.Time{g.RedeemStartedAt, g.RedeemedAt} {
			if at != nil && (at.After(observedAt) || g.GrantedAt != nil && at.Before(*g.GrantedAt)) {
				b.Complete = false
			}
		}
		if g.RedeemStartedAt != nil && g.RedeemedAt != nil && g.RedeemedAt.Before(*g.RedeemStartedAt) {
			b.Complete = false
		}
		if (g.Status == "redeeming" || g.Status == "in_progress") && (g.RedeemStartedAt == nil || g.RedeemedAt != nil) {
			b.Complete = false
		}
		b.Credits = append(b.Credits, g)
	}
	if b.AvailableCount == nil || *b.AvailableCount != available {
		b.Complete = false
	}
	return b, nil
}

// strictObject rejects duplicate keys and excessive nesting before allowlisting.
// Silently choosing one of two grant IDs or counts is unsafe even for a cache.
func strictObject(data []byte) (map[string]any, error) {
	if len(data) > MaxBodyBytes {
		return nil, errors.New("response exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := jsonValue(d, 0)
	if err != nil {
		return nil, errors.New("invalid response JSON")
	}
	if _, errEnd := d.Token(); errEnd != io.EOF {
		return nil, errors.New("trailing response JSON")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("response must be an object")
	}
	return m, nil
}

func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("nesting limit")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			key, errKey := d.Token()
			k, ok := key.(string)
			if errKey != nil || !ok || len(m) >= 8192 {
				return nil, errors.New("invalid object")
			}
			if _, duplicate := m[k]; duplicate {
				return nil, errors.New("duplicate field")
			}
			value, errValue := jsonValue(d, depth+1)
			if errValue != nil {
				return nil, errValue
			}
			m[k] = value
		}
		_, err = d.Token()
		return m, err
	case json.Delim('['):
		list := []any{}
		for d.More() {
			if len(list) >= 8192 {
				return nil, errors.New("array limit")
			}
			value, errValue := jsonValue(d, depth+1)
			if errValue != nil {
				return nil, errValue
			}
			list = append(list, value)
		}
		_, err = d.Token()
		return list, err
	default:
		if _, delim := t.(json.Delim); delim {
			return nil, errors.New("unexpected delimiter")
		}
		return t, nil
	}
}
