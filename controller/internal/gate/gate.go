// Package gate holds the pure decision logic for vpa-gate: given an HPA's
// status, which VPA updateMode should be active right now.
package gate

import (
	"fmt"
	"strconv"
	"time"
)

const (
	AnnPrefix = "pubsub-autoscaling/"
	// AnnGate must be "enabled" for a VPA to be managed.
	AnnGate = AnnPrefix + "vpa-gate"
	// AnnHPA names the HPA (same namespace) whose activity gates the VPA.
	AnnHPA = AnnPrefix + "hpa-name"
	// AnnStable optionally overrides the stable window in seconds.
	AnnStable = AnnPrefix + "stable-seconds"
)

// Modes are the two VPA updateModes the gate switches between.
type Modes struct {
	Scaling string // used while the HPA is scaling or scaled recently
	Settled string // used once the HPA has been stable for the window
}

// DefaultModes: new pods sized at creation while scaling; running pods
// resized once stable.
var DefaultModes = Modes{Scaling: "Initial", Settled: "InPlaceOrRecreate"}

// HPAState is the subset of HPA status the gate needs.
type HPAState struct {
	HasStatus     bool
	Current       int32
	Desired       int32
	LastScaleTime *time.Time
}

// Decision is the gate's verdict for one VPA.
type Decision struct {
	Mode    string
	Settled bool
	Reason  string
	// RecheckAfter > 0 means the verdict will change by itself after this
	// long (the stable window ends) even if nothing in the cluster changes.
	RecheckAfter time.Duration
}

// Decide returns the VPA mode for the given HPA state at time now.
func Decide(h HPAState, now time.Time, stable time.Duration, m Modes) Decision {
	switch {
	case !h.HasStatus:
		return Decision{Mode: m.Scaling, Reason: "hpa has no status yet"}
	case h.Current != h.Desired:
		return Decision{Mode: m.Scaling,
			Reason: fmt.Sprintf("scaling: current=%d desired=%d", h.Current, h.Desired)}
	case h.LastScaleTime == nil:
		return Decision{Mode: m.Settled, Settled: true,
			Reason: fmt.Sprintf("settled: replicas=%d, never scaled", h.Current)}
	}

	age := now.Sub(*h.LastScaleTime)
	if age < stable {
		return Decision{Mode: m.Scaling, RecheckAfter: stable - age + time.Second,
			Reason: fmt.Sprintf("recently scaled: replicas=%d, last scale %s ago < %s",
				h.Current, age.Truncate(time.Second), stable)}
	}
	return Decision{Mode: m.Settled, Settled: true,
		Reason: fmt.Sprintf("settled: replicas=%d, last scale %s ago",
			h.Current, age.Truncate(time.Second))}
}

// Config is the per-VPA configuration read from annotations.
type Config struct {
	HPAName string
	Stable  time.Duration
}

// ConfigFrom returns the gate config for a VPA's annotations, or ok=false if
// the VPA is not gated (annotation missing/disabled or no HPA named).
func ConfigFrom(ann map[string]string, defaultStable time.Duration) (Config, bool) {
	if ann[AnnGate] != "enabled" || ann[AnnHPA] == "" {
		return Config{}, false
	}
	c := Config{HPAName: ann[AnnHPA], Stable: defaultStable}
	if v, ok := ann[AnnStable]; ok {
		if s, err := strconv.Atoi(v); err == nil && s >= 0 {
			c.Stable = time.Duration(s) * time.Second
		}
	}
	return c, true
}
