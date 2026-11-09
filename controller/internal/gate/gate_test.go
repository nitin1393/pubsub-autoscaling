package gate

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	stable := 15 * time.Minute

	cases := []struct {
		name        string
		h           HPAState
		wantMode    string
		wantSettled bool
		wantRecheck time.Duration
	}{
		{"no status", HPAState{}, "Initial", false, 0},
		{"scaling up", HPAState{true, 5, 12, ago(time.Hour)}, "Initial", false, 0},
		{"scaling down", HPAState{true, 12, 5, ago(time.Hour)}, "Initial", false, 0},
		{"scaled 3m ago", HPAState{true, 8, 8, ago(3 * time.Minute)}, "Initial", false, 12*time.Minute + time.Second},
		{"scaled 14m59s ago", HPAState{true, 8, 8, ago(stable - time.Second)}, "Initial", false, 2 * time.Second},
		{"exactly at window", HPAState{true, 8, 8, ago(stable)}, "InPlaceOrRecreate", true, 0},
		{"settled 20m", HPAState{true, 5, 5, ago(20 * time.Minute)}, "InPlaceOrRecreate", true, 0},
		{"never scaled", HPAState{true, 2, 2, nil}, "InPlaceOrRecreate", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.h, now, stable, DefaultModes)
			if d.Mode != c.wantMode || d.Settled != c.wantSettled || d.RecheckAfter != c.wantRecheck {
				t.Fatalf("got mode=%s settled=%v recheck=%s (%s); want mode=%s settled=%v recheck=%s",
					d.Mode, d.Settled, d.RecheckAfter, d.Reason, c.wantMode, c.wantSettled, c.wantRecheck)
			}
		})
	}
}

func TestConfigFrom(t *testing.T) {
	def := 15 * time.Minute
	cases := []struct {
		name   string
		ann    map[string]string
		ok     bool
		hpa    string
		stable time.Duration
	}{
		{"no annotations", nil, false, "", 0},
		{"disabled", map[string]string{AnnGate: "disabled", AnnHPA: "h"}, false, "", 0},
		{"enabled without hpa", map[string]string{AnnGate: "enabled"}, false, "", 0},
		{"enabled default window", map[string]string{AnnGate: "enabled", AnnHPA: "h"}, true, "h", def},
		{"custom window", map[string]string{AnnGate: "enabled", AnnHPA: "h", AnnStable: "600"}, true, "h", 10 * time.Minute},
		{"bad window falls back", map[string]string{AnnGate: "enabled", AnnHPA: "h", AnnStable: "abc"}, true, "h", def},
		{"negative window falls back", map[string]string{AnnGate: "enabled", AnnHPA: "h", AnnStable: "-5"}, true, "h", def},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ConfigFrom(c.ann, def)
			if ok != c.ok || got.HPAName != c.hpa || got.Stable != c.stable {
				t.Fatalf("got %+v ok=%v; want hpa=%q stable=%s ok=%v", got, ok, c.hpa, c.stable, c.ok)
			}
		})
	}
}
