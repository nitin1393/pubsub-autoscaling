package controller

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nitin1393/pubsub-autoscaling/controller/internal/gate"
)

const (
	modeScaling = "Initial"
	modeSettled = "InPlaceOrRecreate"
	window      = 15 * time.Minute
)

// hpaSim models how the KEDA HPA reacts to a backlog: desired = backlog/20
// clamped to [2, 100]; current follows desired one step later; lastScaleTime
// moves whenever current changes.
type hpaSim struct {
	current, desired int32
	lastScale        *time.Time
}

func (s *hpaSim) step(now time.Time, backlog float64) {
	prevDesired := s.desired
	d := int32(math.Ceil(backlog / 20))
	if d < 2 {
		d = 2
	}
	if d > 100 {
		d = 100
	}
	s.desired = d
	if s.current != prevDesired { // HPA applies last step's decision now
		s.current = prevDesired
		t := now
		s.lastScale = &t
	}
}

func (s *hpaSim) apply(t *testing.T, c client.Client, name string) {
	t.Helper()
	h := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, h); err != nil {
		t.Fatal(err)
	}
	h.Status.CurrentReplicas, h.Status.DesiredReplicas = s.current, s.desired
	if s.lastScale != nil {
		lt := metav1.NewTime(*s.lastScale)
		h.Status.LastScaleTime = &lt
	}
	if err := c.Status().Update(context.Background(), h); err != nil {
		t.Fatal(err)
	}
}

// burstyDay generates 8 hours of backlog at 30 s steps: a quiet base load
// with random spikes (5x to 50x, 2 to 40 min long), a sawtooth hour and
// two long quiet periods.
func burstyDay(seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	steps := 8 * 60 * 2
	b := make([]float64, steps)
	for i := range b {
		b[i] = 20 + r.Float64()*20 // base: about 2 pods
	}
	for k := 0; k < 12; k++ {
		start := r.Intn(steps)
		length := 4 + r.Intn(76)
		height := 100 + r.Float64()*1900
		for i := start; i < start+length && i < steps; i++ {
			b[i] += height
		}
	}
	for i := 4 * 120; i < 5*120; i++ { // sawtooth hour: ramps every 10 min
		b[i] += float64((i%20)*60) + 40
	}
	for i := 6 * 120; i < 6*120+60; i++ { // 30 quiet minutes
		b[i] = 25
	}
	for i := steps - 60; i < steps; i++ { // ends with 30 quiet minutes
		b[i] = 25
	}
	return b
}

// TestStressBurstyDay replays a bursty day for several seeds and checks the
// gate's guarantees at every 30 s step.
func TestStressBurstyDay(t *testing.T) {
	days := int64(20)
	if testing.Short() {
		days = 3 // -short (used with -race in CI); the full 20 days run in the stress job
	}
	for seed := int64(1); seed <= days; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			r, c, _ := setup(t, vpa("w", "Off", enabled("h")), hpa("h", 2, 2, nil))
			sim := &hpaSim{current: 2, desired: 2}
			clock := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
			r.Now = func() time.Time { return clock }
			r.Recorder = nil // FakeRecorder blocks after 10 events; stress tests switch more often

			var switches, scaleEvents int
			last := "Off"
			prevScale := (*time.Time)(nil)
			for i, backlog := range burstyDay(seed) {
				clock = clock.Add(30 * time.Second)
				sim.step(clock, backlog)
				sim.apply(t, c, "h")
				if sim.lastScale != prevScale {
					scaleEvents++
					prevScale = sim.lastScale
				}
				reconcileVPA(t, r, "w")
				mode := modeOf(t, c, "w")

				scaling := sim.current != sim.desired
				recent := sim.lastScale != nil && clock.Sub(*sim.lastScale) < window
				switch {
				case (scaling || recent) && mode != modeScaling:
					t.Fatalf("step %d: HPA scaling=%v recent=%v but VPA mode=%s (must be %s)",
						i, scaling, recent, mode, modeScaling)
				case !scaling && !recent && mode != modeSettled:
					t.Fatalf("step %d: HPA quiet for >= %s but VPA mode=%s (must be %s)",
						i, window, mode, modeSettled)
				}
				if mode != last {
					switches++
					last = mode
				}
			}
			if last != modeSettled {
				t.Fatalf("day ends quiet but final mode is %s", last)
			}
			// No flapping: every switch pair needs a full quiet window, so
			// switches can never exceed 2 per (window) of simulated time.
			if maxSw := 2*int((8*time.Hour)/window) + 1; switches > maxSw {
				t.Fatalf("%d switches > %d (flapping)", switches, maxSw)
			}
			t.Logf("scale events=%d, VPA mode switches=%d", scaleEvents, switches)
		})
	}
}

// TestStressConstantFlapping: HPA changes every minute for 3 hours. VPA must
// stay in Initial the whole time and switch exactly once (in), then once
// more (out) after the final quiet window.
func TestStressConstantFlapping(t *testing.T) {
	r, c, _ := setup(t, vpa("w", modeSettled, enabled("h")), hpa("h", 2, 2, nil))
	clock := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return clock }
	r.Recorder = nil
	sim := &hpaSim{current: 2, desired: 2}

	switches, last := 0, modeSettled
	for i := 0; i < 180*2+40; i++ { // 3 h flapping, then 20 quiet minutes
		clock = clock.Add(30 * time.Second)
		backlog := 30.0
		if i < 360 && (i/2)%2 == 0 {
			backlog = 400 // flips between ~2 and ~20 pods every minute
		}
		sim.step(clock, backlog)
		sim.apply(t, c, "h")
		reconcileVPA(t, r, "w")
		m := modeOf(t, c, "w")
		if i < 360 && m != modeScaling {
			t.Fatalf("minute %d of flapping: mode %s", i/2, m)
		}
		if m != last {
			switches++
			last = m
		}
	}
	if last != modeSettled || switches != 2 {
		t.Fatalf("got final=%s switches=%d, want %s and 2", last, switches, modeSettled)
	}
}

// TestStressConcurrentReconciles: 200 VPA/HPA pairs reconciled from 32
// goroutines, 4000 times in total (run with -race). Every VPA must end in the
// mode its own HPA calls for, and an HPA change must only touch its own VPA.
func TestStressConcurrentReconciles(t *testing.T) {
	const pairs = 200
	var objs []client.Object
	for i := 0; i < pairs; i++ {
		h := fmt.Sprintf("h%d", i)
		objs = append(objs, vpa(fmt.Sprintf("v%d", i), "Off", enabled(h)))
		switch i % 3 {
		case 0:
			objs = append(objs, hpa(h, 5, 5, dur(time.Hour))) // settled
		case 1:
			objs = append(objs, hpa(h, 5, 9, dur(time.Hour))) // scaling
		default:
			objs = append(objs, hpa(h, 5, 5, dur(time.Minute))) // recently scaled
		}
	}
	r, c, _ := setup(t, objs...)
	r.Recorder = nil

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(g)))
			for k := 0; k < 125; k++ {
				name := fmt.Sprintf("v%d", rnd.Intn(pairs))
				_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
				if err != nil && !isConflict(err) {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// one clean pass so VPAs never picked at random are reconciled too
	for i := 0; i < pairs; i++ {
		reconcileVPA(t, r, fmt.Sprintf("v%d", i))
	}
	for i := 0; i < pairs; i++ {
		want := modeSettled
		if i%3 != 0 {
			want = modeScaling
		}
		if got := modeOf(t, c, fmt.Sprintf("v%d", i)); got != want {
			t.Fatalf("v%d: mode %s, want %s", i, got, want)
		}
	}
	if reqs := r.vpasForHPA(context.Background(), hpa("h7", 1, 1, nil)); len(reqs) != 1 || reqs[0].Name != "v7" {
		t.Fatalf("hpa h7 maps to %v, want only v7", reqs)
	}
}

func isConflict(err error) bool {
	// concurrent patches of the same VPA may race; merge patches don't conflict,
	// but keep the test robust if a client returns a conflict.
	return err != nil && (contains(err.Error(), "conflict") || contains(err.Error(), "Conflict"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// BenchmarkReconcile: one reconcile in a namespace with 1000 gated VPAs.
func BenchmarkReconcile(b *testing.B) {
	r, _ := benchSetup(b, 1000)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "v500"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHPAToVPAMapping: cost of mapping one HPA event to its VPA among 1000.
func BenchmarkHPAToVPAMapping(b *testing.B) {
	r, _ := benchSetup(b, 1000)
	h := hpa("h500", 1, 1, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if reqs := r.vpasForHPA(context.Background(), h); len(reqs) != 1 {
			b.Fatalf("got %d requests", len(reqs))
		}
	}
}

func benchSetup(b *testing.B, n int) (*Reconciler, client.Client) {
	b.Helper()
	t := &testing.T{}
	var objs []client.Object
	for i := 0; i < n; i++ {
		objs = append(objs, vpa(fmt.Sprintf("v%d", i), "Off", enabled(fmt.Sprintf("h%d", i))),
			hpa(fmt.Sprintf("h%d", i), 5, 5, dur(time.Hour)))
	}
	r, c, _ := setup(t, objs...)
	r.Recorder = nil
	return r, c
}

var _ = gate.DefaultModes
