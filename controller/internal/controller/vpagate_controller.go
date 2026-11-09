// Package controller wires the gate decision into a controller-runtime
// reconciler that watches VPAs and the HPAs they reference.
package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nitin1393/pubsub-autoscaling/controller/internal/gate"
)

// VPA is handled as unstructured so we don't depend on the VPA Go module.
var VPAGVK = schema.GroupVersionKind{
	Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler",
}

var (
	settledGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vpa_gate_settled",
		Help: "1 if the HPA is settled and the VPA may resize running pods, else 0.",
	}, []string{"namespace", "vpa", "hpa"})
	switchesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vpa_gate_mode_switches_total",
		Help: "VPA updateMode changes made by vpa-gate.",
	}, []string{"namespace", "vpa", "to_mode"})
)

func init() { metrics.Registry.MustRegister(settledGauge, switchesTotal) }

// Reconciler sets each gated VPA's updateMode from its HPA's activity.
type Reconciler struct {
	client.Client
	Recorder      record.EventRecorder
	DefaultStable time.Duration
	Modes         gate.Modes
	DryRun        bool
	Now           func() time.Time // for tests; defaults to time.Now
}

func newVPA() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VPAGVK)
	return u
}

// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	vpa := newVPA()
	if err := r.Get(ctx, req.NamespacedName, vpa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cfg, ok := gate.ConfigFrom(vpa.GetAnnotations(), r.DefaultStable)
	if !ok {
		settledGauge.DeletePartialMatch(prometheus.Labels{"namespace": req.Namespace, "vpa": req.Name})
		return ctrl.Result{}, nil
	}

	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: cfg.HPAName}, &hpa); err != nil {
		if apierrors.IsNotFound(err) {
			// Leave the VPA alone; the HPA watch will wake us when it appears.
			logger.Info("hpa not found; leaving vpa unchanged", "hpa", cfg.HPAName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	d := gate.Decide(hpaState(&hpa), now(), cfg.Stable, r.Modes)
	settledGauge.WithLabelValues(req.Namespace, req.Name, cfg.HPAName).Set(b2f(d.Settled))
	result := ctrl.Result{RequeueAfter: d.RecheckAfter}

	have, _, _ := unstructured.NestedString(vpa.Object, "spec", "updatePolicy", "updateMode")
	if have == d.Mode {
		return result, nil
	}
	if r.DryRun {
		logger.Info("would switch vpa mode (dry run)", "from", have, "to", d.Mode, "reason", d.Reason)
		return result, nil
	}

	patch := client.MergeFrom(vpa.DeepCopy())
	if err := unstructured.SetNestedField(vpa.Object, d.Mode, "spec", "updatePolicy", "updateMode"); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Patch(ctx, vpa, patch); err != nil {
		return ctrl.Result{}, err
	}
	switchesTotal.WithLabelValues(req.Namespace, req.Name, d.Mode).Inc()
	logger.Info("switched vpa mode", "hpa", cfg.HPAName, "from", have, "to", d.Mode, "reason", d.Reason)
	if r.Recorder != nil {
		r.Recorder.Eventf(vpa, corev1.EventTypeNormal, "UpdateModeChanged",
			"updateMode %s -> %s (%s)", orNone(have), d.Mode, d.Reason)
	}
	return result, nil
}

// vpasForHPA maps an HPA event to the gated VPAs that reference it.
func (r *Reconciler) vpasForHPA(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(VPAGVK.GroupVersion().WithKind(VPAGVK.Kind + "List"))
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing vpas for hpa", "hpa", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for _, v := range list.Items {
		if v.GetAnnotations()[gate.AnnHPA] == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: v.GetNamespace(), Name: v.GetName()}})
		}
	}
	return reqs
}

// SetupWithManager registers the controller: reconcile on VPA changes and
// on any change to an HPA that a gated VPA references.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("vpa-gate").
		For(newVPA()).
		Watches(&autoscalingv2.HorizontalPodAutoscaler{}, handler.EnqueueRequestsFromMapFunc(r.vpasForHPA)).
		Complete(r)
}

func hpaState(h *autoscalingv2.HorizontalPodAutoscaler) gate.HPAState {
	s := gate.HPAState{
		// An HPA the controller-manager has never processed has an empty status.
		HasStatus: h.Status.LastScaleTime != nil || h.Status.DesiredReplicas != 0 || h.Status.CurrentReplicas != 0,
		Current:   h.Status.CurrentReplicas,
		Desired:   h.Status.DesiredReplicas,
	}
	if h.Status.LastScaleTime != nil {
		t := h.Status.LastScaleTime.Time
		s.LastScaleTime = &t
	}
	return s
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "<unset>"
	}
	return s
}
