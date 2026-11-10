package controller

import (
	"context"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/nitin1393/pubsub-autoscaling/controller/internal/gate"
)

const ns = "my-app"

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func vpa(name, mode string, ann map[string]string) *unstructured.Unstructured {
	u := newVPA()
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetAnnotations(ann)
	if mode != "" {
		_ = unstructured.SetNestedField(u.Object, mode, "spec", "updatePolicy", "updateMode")
	}
	return u
}

func hpa(name string, cur, des int32, lastScaleAgo *time.Duration) *autoscalingv2.HorizontalPodAutoscaler {
	h := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: cur, DesiredReplicas: des},
	}
	if lastScaleAgo != nil {
		t := metav1.NewTime(now.Add(-*lastScaleAgo))
		h.Status.LastScaleTime = &t
	}
	return h
}

func dur(d time.Duration) *time.Duration { return &d }

func enabled(hpaName string) map[string]string {
	return map[string]string{gate.AnnGate: "enabled", gate.AnnHPA: hpaName}
}

func setup(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithIndex(newVPA(), hpaNameIndex, indexHPAName).
		WithStatusSubresource(&autoscalingv2.HorizontalPodAutoscaler{}).Build()
	// fake builder drops status when a status subresource is declared; set it explicitly
	for _, o := range objs {
		if h, ok := o.(*autoscalingv2.HorizontalPodAutoscaler); ok {
			if err := c.Status().Update(context.Background(), h.DeepCopy()); err != nil {
				t.Fatal(err)
			}
		}
	}
	rec := record.NewFakeRecorder(10)
	return &Reconciler{Client: c, Recorder: rec, DefaultStable: 15 * time.Minute,
		Modes: gate.DefaultModes, Now: func() time.Time { return now }}, c, rec
}

func modeOf(t *testing.T, c client.Client, name string) string {
	t.Helper()
	u := newVPA()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, u); err != nil {
		t.Fatal(err)
	}
	m, _, _ := unstructured.NestedString(u.Object, "spec", "updatePolicy", "updateMode")
	return m
}

func reconcileVPA(t *testing.T, r *Reconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func TestSettledSwitchesToInPlace(t *testing.T) {
	r, c, rec := setup(t, vpa("w", "Initial", enabled("h")), hpa("h", 5, 5, dur(20*time.Minute)))
	res := reconcileVPA(t, r, "w")
	if got := modeOf(t, c, "w"); got != "InPlaceOrRecreate" {
		t.Fatalf("mode = %q, want InPlaceOrRecreate", got)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("settled should not requeue, got %s", res.RequeueAfter)
	}
	select {
	case e := <-rec.Events:
		t.Logf("event: %s", e)
	default:
		t.Fatal("expected an UpdateModeChanged event")
	}
}

func TestRecentScaleSwitchesToInitialAndRequeuesAtWindowEnd(t *testing.T) {
	r, c, _ := setup(t, vpa("w", "InPlaceOrRecreate", enabled("h")), hpa("h", 8, 8, dur(3*time.Minute)))
	res := reconcileVPA(t, r, "w")
	if got := modeOf(t, c, "w"); got != "Initial" {
		t.Fatalf("mode = %q, want Initial", got)
	}
	if want := 12*time.Minute + time.Second; res.RequeueAfter != want {
		t.Fatalf("RequeueAfter = %s, want %s", res.RequeueAfter, want)
	}
}

func TestActivelyScalingSwitchesToInitial(t *testing.T) {
	r, c, _ := setup(t, vpa("w", "InPlaceOrRecreate", enabled("h")), hpa("h", 5, 12, dur(time.Hour)))
	reconcileVPA(t, r, "w")
	if got := modeOf(t, c, "w"); got != "Initial" {
		t.Fatalf("mode = %q, want Initial", got)
	}
}

func TestDisabledVPAIsNotTouched(t *testing.T) {
	ann := map[string]string{gate.AnnGate: "disabled", gate.AnnHPA: "h"}
	r, c, _ := setup(t, vpa("w", "Off", ann), hpa("h", 5, 5, dur(time.Hour)))
	reconcileVPA(t, r, "w")
	if got := modeOf(t, c, "w"); got != "Off" {
		t.Fatalf("mode = %q, want Off (untouched)", got)
	}
}

func TestMissingHPALeavesVPAUnchanged(t *testing.T) {
	r, c, _ := setup(t, vpa("w", "Initial", enabled("does-not-exist")))
	reconcileVPA(t, r, "w")
	if got := modeOf(t, c, "w"); got != "Initial" {
		t.Fatalf("mode = %q, want Initial (untouched)", got)
	}
}

func TestNoChangeNoEvent(t *testing.T) {
	r, _, rec := setup(t, vpa("w", "InPlaceOrRecreate", enabled("h")), hpa("h", 5, 5, dur(time.Hour)))
	reconcileVPA(t, r, "w")
	select {
	case e := <-rec.Events:
		t.Fatalf("unexpected event: %s", e)
	default:
	}
}

func TestDryRunDoesNotPatch(t *testing.T) {
	r, c, _ := setup(t, vpa("w", "Initial", enabled("h")), hpa("h", 5, 5, dur(time.Hour)))
	r.DryRun = true
	reconcileVPA(t, r, "w")
	if got := modeOf(t, c, "w"); got != "Initial" {
		t.Fatalf("dry run patched mode to %q", got)
	}
}

func TestHPAMapsToReferencingVPAsOnly(t *testing.T) {
	r, _, _ := setup(t,
		vpa("a", "Initial", enabled("h")),
		vpa("b", "Initial", enabled("other")),
		vpa("c", "Initial", nil),
		hpa("h", 1, 1, nil))
	reqs := r.vpasForHPA(context.Background(), hpa("h", 1, 1, nil))
	if len(reqs) != 1 || reqs[0].Name != "a" {
		t.Fatalf("got %v, want only vpa a", reqs)
	}
}
