package access

import (
	"testing"
	"time"
)

// newTestStore builds an in-memory store (no path) with an injectable clock and
// a single enabled key "k1"/"secret1" carrying the given limits.
func newTestStore(now *time.Time, l Limits) *Store {
	s := &Store{now: func() time.Time { return *now }}
	s.entries = []*keyEntry{{Name: "k1", Key: "secret1", Enabled: true, Limits: l}}
	return s
}

func TestLimitsValidate(t *testing.T) {
	cases := []struct {
		name string
		l    Limits
		ok   bool
	}{
		{"empty", Limits{}, true},
		{"good periods", Limits{BudgetPeriod: "month", RatePeriod: "minute"}, true},
		{"bad budget", Limits{BudgetPeriod: "fortnight"}, false},
		{"bad rate", Limits{RatePeriod: "decade"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.l.Validate()
			if (err == nil) != tc.ok {
				t.Errorf("Validate() err=%v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestPeriodWindows(t *testing.T) {
	// Defaults: budget=month (30d), rate=minute.
	if got := (Limits{}).budgetWindow(); got != 30*24*time.Hour {
		t.Errorf("default budgetWindow = %v", got)
	}
	if got := (Limits{}).rateWindow(); got != time.Minute {
		t.Errorf("default rateWindow = %v", got)
	}
	for _, p := range []struct {
		s string
		d time.Duration
	}{
		{"minute", time.Minute}, {"hour", time.Hour}, {"day", 24 * time.Hour},
		{"week", 7 * 24 * time.Hour}, {"month", 30 * 24 * time.Hour}, {"nope", 0},
	} {
		if got := periodDuration(p.s); got != p.d {
			t.Errorf("periodDuration(%q) = %v, want %v", p.s, got, p.d)
		}
	}
}

func TestAdmitNoLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(&now, Limits{})
	for i := 0; i < 5; i++ {
		if d := s.Admit("k1"); d != Allowed {
			t.Fatalf("unlimited key Admit #%d = %v", i, d)
		}
	}
	// Unknown name and nil store are allowed (fail-open: enforcement only applies
	// to keys that actually carry limits).
	if d := s.Admit("ghost"); d != Allowed {
		t.Errorf("unknown Admit = %v", d)
	}
	var nilStore *Store
	if d := nilStore.Admit("x"); d != Allowed {
		t.Errorf("nil store Admit = %v", d)
	}
}

func TestAdmitRequestLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(&now, Limits{MaxRequests: 2, RatePeriod: "minute"})
	if d := s.Admit("k1"); d != Allowed {
		t.Fatalf("req1 = %v", d)
	}
	if d := s.Admit("k1"); d != Allowed {
		t.Fatalf("req2 = %v", d)
	}
	if d := s.Admit("k1"); d != DeniedRate {
		t.Fatalf("req3 = %v, want DeniedRate", d)
	}
	// After the window elapses the counter resets and admits again.
	now = now.Add(61 * time.Second)
	if d := s.Admit("k1"); d != Allowed {
		t.Fatalf("after reset = %v, want Allowed", d)
	}
}

func TestAdmitTokenLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(&now, Limits{MaxTokens: 100, RatePeriod: "hour"})
	if d := s.Admit("k1"); d != Allowed {
		t.Fatalf("admit = %v", d)
	}
	s.Charge("k1", 0, 100) // hit the token ceiling
	if d := s.Admit("k1"); d != DeniedRate {
		t.Fatalf("after tokens = %v, want DeniedRate", d)
	}
}

func TestAdmitBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(&now, Limits{MaxCostUSD: 1.0, BudgetPeriod: "hour"})
	s.Charge("k1", 0.6, 10)
	if d := s.Admit("k1"); d != Allowed {
		t.Fatalf("under budget = %v", d)
	}
	s.Charge("k1", 0.6, 10) // cumulative 1.2 >= 1.0
	if d := s.Admit("k1"); d != DeniedBudget {
		t.Fatalf("over budget = %v, want DeniedBudget", d)
	}
	// Budget resets after its (hour) window.
	now = now.Add(time.Hour + time.Minute)
	if d := s.Admit("k1"); d != Allowed {
		t.Fatalf("after budget reset = %v, want Allowed", d)
	}
}

func TestChargeNoopAndUsage(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(&now, Limits{MaxCostUSD: 5, MaxTokens: 1000})
	s.Charge("k1", 0, 0)     // no-op
	s.Charge("ghost", 1, 1)  // unknown name no-op
	s.Charge("k1", 1.25, 42) // recorded
	u := s.List()[0].Usage
	if u.CostUSD != 1.25 || u.Tokens != 42 {
		t.Errorf("usage = %+v, want cost 1.25 tokens 42", u)
	}
}

func TestChargeUnlimitedIgnored(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(&now, Limits{}) // no limits → not tracked
	s.Charge("k1", 9.99, 999)
	if u := s.List()[0].Usage; u.CostUSD != 0 || u.Tokens != 0 {
		t.Errorf("unlimited key tracked usage: %+v", u)
	}
}

func TestIdentify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(&now, Limits{})
	s.entries = append(s.entries, &keyEntry{Name: "k2", Key: "secret2", Enabled: false})
	if name, ok := s.Identify("secret1"); !ok || name != "k1" {
		t.Errorf("Identify(secret1) = %q,%v", name, ok)
	}
	if _, ok := s.Identify("secret2"); ok {
		t.Error("Identify matched a disabled key")
	}
	if _, ok := s.Identify("nope"); ok {
		t.Error("Identify matched a missing key")
	}
	if !s.entries[0].LastUsedAt.Equal(now) {
		t.Error("Identify did not stamp last-used")
	}
}

func TestSetLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newTestStore(&now, Limits{})
	if err := s.SetLimits("k1", Limits{BudgetPeriod: "bad"}); err == nil {
		t.Error("SetLimits accepted an invalid period")
	}
	if err := s.SetLimits("ghost", Limits{}); err != ErrNotFound {
		t.Errorf("SetLimits(ghost) = %v, want ErrNotFound", err)
	}
	if err := s.SetLimits("k1", Limits{MaxRequests: 1, RatePeriod: "minute"}); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	if d := s.Admit("k1"); d != Allowed {
		t.Fatalf("first = %v", d)
	}
	if d := s.Admit("k1"); d != DeniedRate {
		t.Fatalf("second = %v, want DeniedRate", d)
	}
}

func TestSetDefaultLimitsAppliedOnAdd(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := &Store{now: func() time.Time { return now }}
	s.SetDefaultLimits(Limits{MaxRequests: 3, RatePeriod: "minute"})
	_, info, err := s.Add("fresh")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if info.Limits.MaxRequests != 3 {
		t.Errorf("new key limits = %+v, want MaxRequests 3", info.Limits)
	}
}
