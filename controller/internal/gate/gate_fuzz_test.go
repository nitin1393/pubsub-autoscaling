package gate

import (
	"testing"
	"time"
)

// FuzzDecide checks the invariants that must hold for ANY HPA state:
//   - a scaling or recently-scaled HPA never gets the settled mode
//   - the settled mode is only returned when the window has fully passed
//   - RecheckAfter is set exactly when waiting for the window, and never
//     points past the end of the window (+1s)
func FuzzDecide(f *testing.F) {
	f.Add(true, int32(5), int32(5), true, int64(1200), int64(900))
	f.Add(true, int32(5), int32(12), true, int64(3600), int64(900))
	f.Add(true, int32(8), int32(8), true, int64(180), int64(900))
	f.Add(true, int32(2), int32(2), false, int64(0), int64(900))
	f.Add(false, int32(0), int32(0), false, int64(0), int64(900))
	f.Add(true, int32(3), int32(3), true, int64(-60), int64(0)) // clock skew: scale time in the future

	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, hasStatus bool, cur, des int32, hasLast bool, ageSec, stableSec int64) {
		// keep durations in a sane range (about +/- 1 year) to avoid overflow
		if ageSec > 3e7 || ageSec < -3e7 || stableSec < 0 || stableSec > 3e7 {
			t.Skip()
		}
		stable := time.Duration(stableSec) * time.Second
		h := HPAState{HasStatus: hasStatus, Current: cur, Desired: des}
		if hasLast {
			ts := now.Add(-time.Duration(ageSec) * time.Second)
			h.LastScaleTime = &ts
		}
		d := Decide(h, now, stable, DefaultModes)

		if d.Mode != DefaultModes.Scaling && d.Mode != DefaultModes.Settled {
			t.Fatalf("unknown mode %q", d.Mode)
		}
		if d.Settled != (d.Mode == DefaultModes.Settled) {
			t.Fatalf("Settled=%v but mode=%s", d.Settled, d.Mode)
		}
		if !hasStatus && d.Settled {
			t.Fatal("no status must never be settled")
		}
		if hasStatus && cur != des && d.Settled {
			t.Fatalf("scaling (cur=%d des=%d) must never be settled", cur, des)
		}
		if hasStatus && cur == des && hasLast {
			age := time.Duration(ageSec) * time.Second
			if age < stable && d.Settled {
				t.Fatalf("settled with age %s < window %s", age, stable)
			}
			if age >= stable && !d.Settled {
				t.Fatalf("not settled with age %s >= window %s", age, stable)
			}
			if age < stable {
				if d.RecheckAfter <= 0 || d.RecheckAfter > stable-age+time.Second {
					t.Fatalf("RecheckAfter=%s, want (0, %s]", d.RecheckAfter, stable-age+time.Second)
				}
			}
		}
		if d.Settled && d.RecheckAfter != 0 {
			t.Fatalf("settled decision must not requeue, got %s", d.RecheckAfter)
		}
		if d.RecheckAfter < 0 {
			t.Fatalf("negative RecheckAfter %s", d.RecheckAfter)
		}
	})
}
