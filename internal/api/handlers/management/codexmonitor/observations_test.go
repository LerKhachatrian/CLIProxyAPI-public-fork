package codexmonitor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSignalsMissingPartialZeroAndAdditional(t *testing.T) {
	at := time.Now().UTC()
	for _, signals := range []map[string]string{
		{"Retry-After": "600"},
		{"X-Codex-Primary-Used-Percent": "100"},
		{"X-Codex-Additional-Spark-Primary-Used-Percent": "50", "X-Codex-Additional-Spark-Primary-Window-Minutes": "300", "X-Codex-Additional-Spark-Primary-Reset-After-Seconds": "100"},
	} {
		if u := FromSignals(signals, at); u != nil {
			t.Fatal("partial/additional signal became main entitlement")
		}
	}
	signals := map[string]string{"X-Codex-Secondary-Used-Percent": "28", "X-Codex-Secondary-Window-Minutes": "10080", "X-Codex-Secondary-Reset-After-Seconds": "1000"}
	u := FromSignals(signals, at)
	if u == nil || len(u.Windows) != 1 || u.Windows[0].Kind != "weekly" {
		t.Fatal("weekly-only coverage")
	}
	signals["X-Codex-Primary-Used-Percent"] = "100"
	signals["X-Codex-Primary-Window-Minutes"] = "300"
	signals["X-Codex-Primary-Reset-After-Seconds"] = "10"
	u = FromSignals(signals, at)
	if len(u.Windows) != 2 || u.Windows[0].Kind != "five_hour" || u.Windows[0].UsedPercent != 100 {
		t.Fatal("valid zero allowance lost")
	}
	for _, invalid := range []string{"NaN", "+Inf", "-1", "101", "true"} {
		signals["X-Codex-Primary-Used-Percent"] = invalid
		u = FromSignals(signals, at)
		if len(u.Windows) != 1 {
			t.Fatalf("invalid percentage %s survived", invalid)
		}
	}
}

func TestPlanAliasesDuplicateMainAndBankChronology(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, alias := range []string{"prolite", "pro_lite", " ProLite "} {
		if plan(alias) != "pro_5x" {
			t.Fatal("lost Pro 5x identity")
		}
	}
	if _, err := ParseUsage([]byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":500},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":500}}}`), now); err == nil {
		t.Fatal("ambiguous main windows accepted")
	}
	base := map[string]any{"id": "synthetic", "reset_type": "codex_rate_limits", "status": "available", "granted_at": now.Add(-time.Hour), "expires_at": now.Add(time.Hour)}
	for _, edit := range []map[string]any{
		{"status": map[string]any{"untrusted": true}}, {"status": []any{}}, {"reset_type": []any{}},
		{"granted_at": now.Add(time.Minute)}, {"expires_at": now.Add(-2 * time.Hour)},
		{"status": "redeemed", "redeemed_at": now.Add(-2 * time.Hour)},
		{"status": "redeemed", "redeemed_at": now.Add(time.Minute)},
		{"status": "redeeming"}, {"redeem_started_at": "invalid"},
	} {
		grant := map[string]any{}
		for k, v := range base {
			grant[k] = v
		}
		for k, v := range edit {
			grant[k] = v
		}
		data, _ := json.Marshal(map[string]any{"available_count": 1, "credits": []any{grant}})
		bank, err := ParseBank(data, now)
		if err == nil && bank.Complete {
			t.Fatal("malformed/contradictory bank marked complete")
		}
	}
	for _, count := range []any{true, []any{}, map[string]any{}, -1, 1.5, "NaN", 257} {
		data, _ := json.Marshal(map[string]any{"available_count": count, "credits": []any{}})
		bank, err := ParseBank(data, now)
		if err != nil || bank.Complete || bank.AvailableCount != nil {
			t.Fatal("invalid count became known")
		}
	}
	e := &Entry{Schema: 1, Key: strings.Repeat("1", 64), Usage: usageAt(now)}
	e.Usage.Windows[0].Kind = "five_hour"
	if validEntry(e, e.Key) {
		t.Fatal("cache kind/duration conflict accepted")
	}
	e.Usage = nil
	count := 1
	e.Bank = &Bank{ObservedAt: now, AvailableCount: &count, Credits: []Grant{}, Complete: true}
	if validEntry(e, e.Key) {
		t.Fatal("cache upgraded incomplete inventory")
	}
}

func TestUsageParsingNeverCopiesUnknownFields(t *testing.T) {
	now := time.Now().UTC()
	u, err := ParseUsage([]byte(`{"secret":"never-retain","plan_type":"pro","rate_limit":{"allowed":true,"secondary_window":{"used_percent":28,"limit_window_seconds":604800,"reset_after_seconds":1000}}}`), now)
	if err != nil || len(u.Windows) != 1 || u.Windows[0].Kind != "weekly" || u.Allowed == nil || !*u.Allowed {
		t.Fatalf("%+v %v", u, err)
	}
	for _, body := range []string{
		`{"rate_limit":{},"rate_limit":{}}`, `{} {}`, `{"a":[` + strings.Repeat("[", 33) + `0` + strings.Repeat("]", 33) + `]}`,
		`{"rate_limit":{"primary_window":{"used_percent":150,"limit_window_seconds":18000,"reset_at":1800000000}}}`,
	} {
		if _, err := ParseUsage([]byte(body), now); err == nil {
			t.Fatalf("invalid response accepted: %.80s", body)
		}
	}
}

func TestBankParsingExactIdentityAndIncompleteTruth(t *testing.T) {
	now := time.Now().UTC()
	b, err := ParseBank([]byte(`{"available_count":0,"credits":[],"access_token":"discard"}`), now)
	if err != nil || !b.Complete || b.AvailableCount == nil || *b.AvailableCount != 0 {
		t.Fatal("valid empty bank")
	}
	b, err = ParseBank([]byte(`{"available_count":1,"credits":[]}`), now)
	if err != nil || b.Complete {
		t.Fatal("mismatched count became complete")
	}
	b, err = ParseBank([]byte(`{"credits":[]}`), now)
	if err != nil || b.AvailableCount != nil || b.Complete {
		t.Fatal("missing bank became zero")
	}
	for _, body := range []string{
		`{"available_count":0,"available_count":1,"credits":[]}`,
		`{"available_count":0,"credits":[{"id":"a"},{"id":"a"}]}`,
		`{"available_count":0,"credits":[{"id":"../escape"}]}`,
		`{"available_count":0,"credits":"invalid"}`,
	} {
		if _, err := ParseBank([]byte(body), now); err == nil {
			t.Fatal("invalid bank accepted")
		}
	}
}
