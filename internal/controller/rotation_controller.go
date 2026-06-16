package controller

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
	"github.com/AkashiSN/node-rotation-controller/internal/surge"
	"github.com/AkashiSN/node-rotation-controller/internal/window"
)

// Requeue cadences (spec §5.2): the slow Tick re-evaluation versus the faster
// poll while a rotation is actively progressing.
const (
	longRequeue  = time.Minute
	shortRequeue = 30 * time.Second
)

// RotationReconciler drives the make-before-break rotation state machine
// (spec §5.2/§5.3). It is keyed on the NodePool: each Reconcile performs exactly
// one non-blocking step, reading all rotation state back from annotations, so a
// worker is never held and progress survives controller restarts. Serialization
// rests on the NodePool's active-rotation anchor (§5.2 step 1).
type RotationReconciler struct {
	client.Client

	// Policy and Schedule resolve the per-NodePool gates and timeouts.
	Policy   *policy.Policy
	Schedule *window.Schedule

	// Namespace, PlaceholderImage, and PriorityClassName configure the surge
	// placeholder Pod (spec §3.3).
	Namespace         string
	PlaceholderImage  string
	PriorityClassName string

	// Recorder emits the §4.2 metrics/alerts; nil means no-op.
	Recorder Recorder
	// Clock is the time source; nil means time.Now (overridden in tests).
	Clock func() time.Time
}

func (r *RotationReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *RotationReconciler) recorder() Recorder {
	if r.Recorder != nil {
		return r.Recorder
	}
	return noopRecorder{}
}

// Reconcile resolves the request to its NodePool and runs one rotation step.
// Out-of-scope NodePools are ignored without a requeue; in-scope ones always
// return a RequeueAfter, so the periodic re-evaluation (window edges, freeze
// releases — spec §5.2) is realized by the self-requeue rather than a separate
// Ticker.
func (r *RotationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool karpv1.NodePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			// The NodePool is gone; drop its metric series so the recomputed gauges
			// don't latch at their last value once its reconciles stop (§4.2).
			// NodePool is cluster-scoped, so the request name is the pool name.
			r.recorder().ForgetPool(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.inScope(&pool) {
		return ctrl.Result{}, nil
	}
	return r.reconcileNodePool(ctx, &pool)
}

// inScope reports whether the NodePool matches the configured selectors (§3.2).
func (r *RotationReconciler) inScope(pool *karpv1.NodePool) bool {
	return len(selection.InScopeNodePools([]karpv1.NodePool{*pool}, r.Policy.NodePoolSelectors)) == 1
}

// SetupWithManager registers the reconciler. It is keyed on the NodePool: the
// NodePool watch seeds and periodically (via self-requeue) re-evaluates each
// pool and picks up freeze-annotation edits, while NodeClaim events are mapped to
// their owning NodePool so a claim becoming Ready/expiring drives its pool
// promptly (spec §5.1/§5.2).
//
// The placeholder-Pod and Node watches cut surge-readiness latency (issue #14):
// the two signals that advance an in-flight pending rotation — the placeholder
// reaching Running and its host Node reaching Ready — would otherwise be seen
// only by the 30s self-requeue. Predicates keep them to those transitions so
// unrelated Pod/Node churn does not amplify reconciles; the periodic requeue
// stays as the backstop for drain progress and force-expiry detection.
func (r *RotationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("rotation").
		For(&karpv1.NodePool{}).
		Watches(&karpv1.NodeClaim{}, handler.EnqueueRequestsFromMapFunc(nodePoolFromLabel)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.placeholderToNodePool),
			builder.WithPredicates(placeholderRunning())).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(nodePoolFromLabel),
			builder.WithPredicates(nodeBecameReady())).
		Complete(r)
}

// nodePoolFromLabel maps an object carrying the karpenter.sh/nodepool label
// (a NodeClaim or a Node) to a reconcile of that NodePool — the reconcile unit.
// An object without the label — e.g. a manually created NodeClaim, or a Node
// outside any Karpenter NodePool — enqueues nothing, bounding the reconcile rate.
func nodePoolFromLabel(_ context.Context, obj client.Object) []reconcile.Request {
	np := obj.GetLabels()[karpv1.NodePoolLabelKey]
	if np == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: np}}}
}

// placeholderToNodePool maps a placeholder Pod to its owning NodePool, read from
// the karpenter.sh/nodepool label stamped at creation (no client lookup). It
// filters to the controller namespace and the surge-for marker so only the
// controller's own placeholders enqueue.
func (r *RotationReconciler) placeholderToNodePool(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Namespace {
		return nil
	}
	labels := obj.GetLabels()
	if labels[annotations.SurgeFor] == "" {
		return nil
	}
	np := labels[karpv1.NodePoolLabelKey]
	if np == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: np}}}
}

// placeholderRunning enqueues only when a placeholder Pod reaches Running — the
// transition advancePending waits on to observe surge readiness. Deletions and
// other phase changes are dropped (issue #14).
func placeholderRunning() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return podRunning(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return !podRunning(e.ObjectOld) && podRunning(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func podRunning(obj client.Object) bool {
	p, ok := obj.(*corev1.Pod)
	return ok && p.Status.Phase == corev1.PodRunning
}

// nodeBecameReady enqueues only when a Node's Ready condition flips to True — the
// other signal advancePending waits on (the surge host registering). Unrelated
// Node churn is dropped to bound the reconcile rate (issue #14).
func nodeBecameReady() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return nodeReadyObj(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return !nodeReadyObj(e.ObjectOld) && nodeReadyObj(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func nodeReadyObj(obj client.Object) bool {
	n, ok := obj.(*corev1.Node)
	return ok && nodeReady(n)
}

// resolved holds the per-NodePool durations derived from policy + schedule.
type resolved struct {
	leadTime     time.Duration  // K·P + t_rot, for candidate selection (§3.2)
	override     *time.Duration // explicit ageThreshold, nil in auto mode
	retryBackoff time.Duration
	readyTimeout time.Duration
	cooldown     time.Duration
	drainBound   time.Duration // tGP + buffer; DrainFallback when tGP unset (§5.2)
}

func (r *RotationReconciler) resolve(pool *karpv1.NodePool) resolved {
	s := r.Policy.Surge
	tgp, unset := poolTGP(pool)
	p, _ := r.Schedule.WorstCasePeriod()
	tRot := s.ReadyTimeout.Duration + tgp + schedule.Buffer

	override, isAuto, _ := r.Policy.AgeThresholdOverride()
	var ov *time.Duration
	if !isAuto {
		d := override
		ov = &d
	}

	drainBound := tgp + schedule.Buffer
	if unset {
		drainBound = schedule.DrainFallback
	}

	return resolved{
		leadTime:     time.Duration(r.Policy.K())*p + tRot,
		override:     ov,
		retryBackoff: s.RetryBackoff.Duration,
		readyTimeout: s.ReadyTimeout.Duration,
		cooldown:     s.CooldownAfter.Duration,
		drainBound:   drainBound,
	}
}

// poolTGP resolves the NodePool's terminationGracePeriod, substituting the fixed
// DrainFallback (and reporting unset) when Karpenter leaves it nil (spec §3.2).
func poolTGP(pool *karpv1.NodePool) (time.Duration, bool) {
	if d := pool.Spec.Template.Spec.TerminationGracePeriod; d != nil {
		return d.Duration, false
	}
	return schedule.DrainFallback, true
}

func (r *RotationReconciler) selInputs(res resolved, now time.Time) selection.Inputs {
	return selection.Inputs{
		Now:          now,
		LeadTime:     res.leadTime,
		Override:     res.override,
		RetryBackoff: res.retryBackoff,
	}
}

// reconcileNodePool is the §5.2 driver: drive any in-flight rotation first
// (serial), else evaluate the start gates and begin a new one.
func (r *RotationReconciler) reconcileNodePool(ctx context.Context, pool *karpv1.NodePool) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodepool", pool.Name)
	now := r.now()
	res := r.resolve(pool)

	// List the pool's claims once and feed both the §4.2 gauges and candidate
	// selection: the state is identical for both and unchanged in between (step 2
	// writes nothing), so a single read avoids a redundant cache list per pass.
	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	// ── 0. Emit the §4.2 reconcile-time gauges from current state, every pass.
	r.observe(pool, res, now, claims)

	// ── 1. Drive the in-flight rotation, keyed on the anchor (it outlives the
	//        old NodeClaim's deletion on success).
	if name := pool.Annotations[annotations.ActiveRotation]; name != "" {
		return r.advance(ctx, pool, name)
	}

	// ── 2. Candidate-independent start gates.
	if !r.startGates(pool, res, now) {
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	// ── 3. Pick the candidate, gate on its headroom, then anchor.
	cand := selection.PickOldestEligible(claims, r.selInputs(res, now))
	if cand == nil {
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}
	ok, err := r.headroomFits(ctx, pool, cand)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		log.Info("insufficient limits headroom; cannot surge", "candidate", cand.Name)
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}
	// Anchor BEFORE any other side effect: a conflict-checked, only-if-absent
	// write (optimistic lock on resourceVersion). A racing reconcile's write
	// loses with a Conflict; the loser does nothing but requeue (spec §5.2).
	if err := r.anchorRotation(ctx, pool, cand.Name); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: longRequeue}, nil
		}
		return ctrl.Result{}, err
	}
	return r.advance(ctx, pool, cand.Name)
}

// observe computes and emits the §4.2 reconcile-time gauges from the live claims
// (listed once by the caller) on every pass. Recomputing each pass is what lets
// the 0/1 and "highest"/"count" gauges reset: a cleared drain stops alerting, a
// pool with no failures reports zero retries. The window-active gauge is
// cluster-wide (label-free) but set here because the reconcile is the only
// periodic tick (§5.2).
func (r *RotationReconciler) observe(pool *karpv1.NodePool, res resolved, now time.Time, claims []karpv1.NodeClaim) {
	rec := r.recorder()
	rec.ObserveWindow(r.Schedule.InWindow(now))

	// WorstCasePeriod's ok is always true here: policy.Validate rejects an empty
	// maintenanceWindows (and empty days), so the Schedule always has ≥1 occurrence.
	p, _ := r.Schedule.WorstCasePeriod()
	a, g := r.derivedThresholds(pool, res, p)
	o := PoolObservation{
		Candidates:      selection.CountEligible(claims, r.selInputs(res, now)),
		ShortLeadNodes:  selection.CountShortLead(claims, res.leadTime),
		RetryCount:      highestRetry(claims),
		DrainStuck:      r.drainStuck(pool, claims, res, now),
		WindowPeriod:    p,
		AgeThreshold:    a,
		RotationChances: g,
	}
	if pool.Annotations[annotations.ActiveRotation] != "" {
		o.InProgress = 1 // serial per NodePool in v1 (0 or 1)
	}
	if until, ok := parseTime(pool.Annotations[annotations.Freeze]); ok && now.Before(until) {
		o.FreezeUntil = until
	}
	rec.ObservePool(pool.Name, o)
}

// highestRetry returns the largest retry-count annotation across the pool's
// claims (0 when none) — the noderotation_retry_count gauge (§4.2).
func highestRetry(claims []karpv1.NodeClaim) int {
	highest := 0
	for i := range claims {
		if n := parseInt(claims[i].Annotations[annotations.RetryCount]); n > highest {
			highest = n
		}
	}
	return highest
}

// drainStuck reports whether the in-flight draining rotation's old NodeClaim has
// been deleting past the drain bound (tGP + buffer) — the noderotation_drain_stuck
// gauge (§4.2, §5.2). It mirrors the bound check in advanceDraining.
func (r *RotationReconciler) drainStuck(pool *karpv1.NodePool, claims []karpv1.NodeClaim, res resolved, now time.Time) bool {
	name := pool.Annotations[annotations.ActiveRotation]
	if name == "" || pool.Annotations[annotations.ActiveRotationState] != annotations.StateDraining {
		return false
	}
	for i := range claims {
		c := &claims[i]
		if c.Name == name && c.DeletionTimestamp != nil {
			return now.Sub(c.DeletionTimestamp.Time) > res.drainBound
		}
	}
	return false
}

// derivedThresholds returns the derived ageThreshold A and guaranteed chances G
// for the pool's representative template expireAfter (§3.2) — the
// noderotation_age_threshold_seconds and noderotation_rotation_chances gauges. A
// never-expiring template has no derivation: an override A still applies, but no
// chances can be guaranteed.
func (r *RotationReconciler) derivedThresholds(pool *karpv1.NodePool, res resolved, p time.Duration) (time.Duration, int) {
	eptr := pool.Spec.Template.Spec.ExpireAfter.Duration
	if eptr == nil {
		if res.override != nil {
			return *res.override, 0
		}
		return 0, 0
	}
	tgp, unset := poolTGP(pool)
	out := schedule.Derive(schedule.Inputs{
		E:              *eptr,
		TGP:            tgp,
		TGPWasUnset:    unset,
		P:              p,
		ReadyTimeout:   res.readyTimeout,
		Cooldown:       res.cooldown,
		RetryBackoff:   res.retryBackoff,
		K:              r.Policy.K(),
		MaxUnavailable: r.Policy.Surge.MaxUnavailable,
		Override:       res.override,
	})
	return out.A, out.G
}

// startGates is the §5.2 step-2 gate set, shared verbatim with the failed →
// pending re-entry so the two never diverge.
func (r *RotationReconciler) startGates(pool *karpv1.NodePool, res resolved, now time.Time) bool {
	if !r.Schedule.InWindow(now) {
		return false
	}
	if frozen(pool, now) {
		return false
	}
	if since(pool.Annotations[annotations.LastRotationAt], now) < res.cooldown {
		return false
	}
	if since(pool.Annotations[annotations.LastFailureAt], now) < res.cooldown {
		return false
	}
	return true
}

// advance runs one step for the in-flight rotation, keyed by the anchor name.
func (r *RotationReconciler) advance(ctx context.Context, pool *karpv1.NodePool, name string) (ctrl.Result, error) {
	res := r.resolve(pool)
	cand, err := r.getClaim(ctx, name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cand == nil {
		// Old NodeClaim finalized away → completion or abort, decided by the mirror.
		return r.completeOrAbort(ctx, pool, name)
	}
	switch cand.Annotations[annotations.State] {
	case "", annotations.StatePending:
		return r.advancePending(ctx, pool, res, cand)
	case annotations.StateDraining:
		return r.advanceDraining(ctx, pool, cand)
	case annotations.StateFailed:
		return r.advanceFailed(ctx, pool, res, cand)
	case annotations.StateExpired:
		return r.advanceExpired(ctx, pool, cand)
	default:
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}
}

// advancePending is the idempotent pending handler (spec §5.2). It re-asserts the
// phase's side effects on every pass so any crash mid-start heals on the next one.
func (r *RotationReconciler) advancePending(ctx context.Context, pool *karpv1.NodePool, res resolved, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	// Force-expiry caught in the act — checked before EVERYTHING: a dying claim
	// must never be escalated to draining nor failed by the timeout (spec §5.2).
	if cand.DeletionTimestamp != nil {
		return r.abortPendingExpiry(ctx, pool, cand)
	}

	// Assert pending + write-once started-at (a single claim update).
	if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
		m[annotations.State] = annotations.StatePending
		if m[annotations.StartedAt] == "" {
			m[annotations.StartedAt] = rfc3339(r.now())
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	cand, err := r.getClaim(ctx, cand.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cand == nil { // vanished between the patch and the re-read
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	// readyTimeout — checked FIRST (before the recreate branch) so a crash on this
	// failure path cannot resurrect the placeholder (spec §5.2).
	startedAt, _ := parseTime(cand.Annotations[annotations.StartedAt])
	if r.now().Sub(startedAt) > res.readyTimeout {
		return r.failPending(ctx, pool, cand)
	}

	// Protective markers are passive — re-asserted every pass, even while frozen.
	if err := r.freezeNode(ctx, cand.Status.NodeName, cand.Name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.cordonNode(ctx, cand.Status.NodeName); err != nil {
		return ctrl.Result{}, err
	}
	// Persist surge-claim the moment the bind target is observable (passive, runs
	// even while frozen) — the placeholder, its only other source, may vanish.
	if cand.Annotations[annotations.SurgeClaim] == "" {
		surgeClaim, err := r.inducedClaim(ctx, pool, cand)
		if err != nil {
			return ctrl.Result{}, err
		}
		if surgeClaim != "" {
			if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
				m[annotations.SurgeClaim] = surgeClaim
			}); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Freeze hold: suspend escalation only (no placeholder (re)creation, no
	// transition to draining); the attempt times out and rolls back cleanly if
	// the freeze outlasts readyTimeout (spec §3.1).
	if frozen(pool, r.now()) {
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	ph, err := r.getPlaceholder(ctx, cand.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ph == nil {
		if err := r.createPlaceholder(ctx, pool, cand, res); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: shortRequeue}, nil
	}

	host, ready, err := r.surgeReady(ctx, pool, cand, ph)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ready {
		// surge_wait phase complete: started-at → surge_ready (§4.2). Observed here
		// because started-at lives on the candidate, which is deleted just below.
		r.recorder().ObserveDuration(pool.Name, PhaseSurgeWait, r.now().Sub(startedAt))
		if err := r.freezeNode(ctx, host, cand.Name); err != nil {
			return ctrl.Result{}, err
		}
		// Durable phase record BEFORE the delete — it decides the completion outcome.
		if err := r.patchPool(ctx, pool, func(m map[string]string) {
			m[annotations.ActiveRotationState] = annotations.StateDraining
		}); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
			m[annotations.State] = annotations.StateDraining
		}); err != nil {
			return ctrl.Result{}, err
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, cand)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: shortRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: shortRequeue}, nil
}

// abortPendingExpiry handles a candidate caught force-expiring in pending: clean
// up the runtime objects, mark the claim terminally expired (before releasing the
// anchor), and emit expired — never success, no cooldown (spec §5.2).
func (r *RotationReconciler) abortPendingExpiry(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	if err := r.deletePlaceholder(ctx, cand.Name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.unfreezeNodes(ctx, cand.Name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
		m[annotations.State] = annotations.StateExpired
		delete(m, annotations.StartedAt)
		delete(m, annotations.SurgeClaim)
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.recorder().Expired(pool.Name, cand.Name)
	if err := r.clearAnchor(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: longRequeue}, nil
}

// failPending performs the readyTimeout rollback: reap the induced claim, delete
// the placeholder, unfreeze, write the failed state in one claim update, emit the
// failure, then release the gate with the pool-level pause anchor (spec §5.2).
func (r *RotationReconciler) failPending(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	surgeClaim := cand.Annotations[annotations.SurgeClaim]
	if surgeClaim == "" { // last resort — normally persisted during pending
		name, err := r.inducedClaim(ctx, pool, cand)
		if err != nil {
			return ctrl.Result{}, err
		}
		surgeClaim = name
	}
	if err := r.reapSurgeClaim(ctx, cand, surgeClaim); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deletePlaceholder(ctx, cand.Name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.unfreezeNodes(ctx, cand.Name); err != nil {
		return ctrl.Result{}, err
	}

	// The retry-count annotation is the durable backoff state; it also feeds the
	// recomputed noderotation_retry_count gauge (set in observe), so no separate
	// gauge emission is needed here.
	if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
		m[annotations.State] = annotations.StateFailed
		m[annotations.FailedAt] = rfc3339(r.now())
		m[annotations.RetryCount] = strconv.Itoa(parseInt(m[annotations.RetryCount]) + 1)
		delete(m, annotations.StartedAt)
		delete(m, annotations.SurgeClaim)
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.recorder().Failure(pool.Name, cand.Name)

	// Single pool update (last): the inter-attempt pause anchor + the gate release.
	if err := r.patchPool(ctx, pool, func(m map[string]string) {
		m[annotations.LastFailureAt] = rfc3339(r.now())
		delete(m, annotations.ActiveRotation)
		delete(m, annotations.ActiveRotationState)
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: longRequeue}, nil
}

// advanceDraining waits for the old NodeClaim to finalize away, re-issuing the
// idempotent delete on crash recovery, while deliberately keeping the serial gate
// held (spec §5.2). The stuck-drain alert is the recomputed drain_stuck gauge
// (observe), so this step no longer needs the drain bound.
func (r *RotationReconciler) advanceDraining(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	if pool.Annotations[annotations.ActiveRotationState] != annotations.StateDraining {
		if err := r.patchPool(ctx, pool, func(m map[string]string) {
			m[annotations.ActiveRotationState] = annotations.StateDraining
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	if cand.DeletionTimestamp == nil { // crash between the state write and the delete
		if err := client.IgnoreNotFound(r.Delete(ctx, cand)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: shortRequeue}, nil
	}
	// The stuck-drain signal is the recomputed drain_stuck gauge (set in observe),
	// not a one-shot emission here — a 0/1 gauge must reset once the drain clears.
	return ctrl.Result{RequeueAfter: shortRequeue}, nil
}

// advanceFailed handles a failed claim still anchored: terminal if it is being
// deleted (the backstop reached a rolled-back claim); else re-enter pending when
// every start gate passes past the escalated backoff, or repair a torn failure
// write by releasing the gate while preserving the pause anchor (spec §5.2).
func (r *RotationReconciler) advanceFailed(ctx context.Context, pool *karpv1.NodePool, res resolved, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	if cand.DeletionTimestamp != nil {
		if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
			m[annotations.State] = annotations.StateExpired
		}); err != nil {
			return ctrl.Result{}, err
		}
		r.recorder().Expired(pool.Name, cand.Name)
		if err := r.clearAnchor(ctx, pool); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	now := r.now()
	failedAt, _ := parseTime(cand.Annotations[annotations.FailedAt])
	retry := parseInt(cand.Annotations[annotations.RetryCount])
	headroomOK, err := r.headroomFits(ctx, pool, cand)
	if err != nil {
		return ctrl.Result{}, err
	}
	// A re-entry is a NEW attempt, so it must pass EVERYTHING a fresh start would.
	if r.startGates(pool, res, now) &&
		now.Sub(failedAt) >= selection.EscalatedBackoff(retry, res.retryBackoff) &&
		headroomOK {
		if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
			m[annotations.State] = annotations.StatePending
		}); err != nil {
			return ctrl.Result{}, err
		}
		return r.advance(ctx, pool, cand.Name) // falls into the pending handler, re-stamps started-at
	}

	// Otherwise: repair a torn failure write (crash between the failed write and
	// the pool update). Re-stamp last-failure-at = max(existing, failed-at) so the
	// §4.4 pause is never voided, then release the gate.
	if err := r.patchPool(ctx, pool, func(m map[string]string) {
		m[annotations.LastFailureAt] = maxRFC3339(m[annotations.LastFailureAt], cand.Annotations[annotations.FailedAt])
		delete(m, annotations.ActiveRotation)
		delete(m, annotations.ActiveRotationState)
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: longRequeue}, nil
}

// advanceExpired re-runs the idempotent cleanup for a terminal claim still
// anchored (crash between the terminal write and the pool clear) and releases the
// gate; the metric/alert are NOT re-emitted (spec §5.2).
func (r *RotationReconciler) advanceExpired(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	if err := r.deletePlaceholder(ctx, cand.Name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.unfreezeNodes(ctx, cand.Name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.clearAnchor(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: longRequeue}, nil
}

// completeOrAbort runs the completion side effects after the old NodeClaim is
// gone. The mirror decides the outcome: draining → success + cooldown; absent →
// expired + alert, no cooldown (spec §5.2).
func (r *RotationReconciler) completeOrAbort(ctx context.Context, pool *karpv1.NodePool, name string) (ctrl.Result, error) {
	if err := r.deletePlaceholder(ctx, name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.unfreezeNodes(ctx, name); err != nil {
		return ctrl.Result{}, err
	}
	if pool.Annotations[annotations.ActiveRotationState] == annotations.StateDraining {
		r.recorder().Success(pool.Name) // emit before releasing the gate (at-least-once)
		if err := r.patchPool(ctx, pool, func(m map[string]string) {
			m[annotations.LastRotationAt] = rfc3339(r.now())
			delete(m, annotations.ActiveRotation)
			delete(m, annotations.ActiveRotationState)
		}); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		r.recorder().Expired(pool.Name, name) // vanished out of pending — nothing rotated
		if err := r.clearAnchor(ctx, pool); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: longRequeue}, nil
}

// ── Start-gate helpers ─────────────────────────────────────────────────────

// frozen reports whether the NodePool's freeze annotation is in the future.
func frozen(pool *karpv1.NodePool, now time.Time) bool {
	until, ok := parseTime(pool.Annotations[annotations.Freeze])
	return ok && now.Before(until)
}

// since returns now − the RFC3339 timestamp, or +∞ when it is unset/unparseable.
func since(ts string, now time.Time) time.Duration {
	t, ok := parseTime(ts)
	if !ok {
		return time.Duration(math.MaxInt64)
	}
	return now.Sub(t)
}

// ── Surge readiness / induced-claim resolution ─────────────────────────────

func (r *RotationReconciler) headroomFits(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (bool, error) {
	reqs, err := r.candidateRequests(ctx, cand)
	if err != nil {
		return false, err
	}
	return surge.FitsHeadroom(pool, reqs), nil
}

// candidateRequests sums the reschedulable Pod requests on the candidate node —
// the placeholder's sizing (spec §3.3). An unscheduled candidate has none.
func (r *RotationReconciler) candidateRequests(ctx context.Context, cand *karpv1.NodeClaim) (corev1.ResourceList, error) {
	if cand.Status.NodeName == "" {
		return corev1.ResourceList{}, nil
	}
	pods, err := r.allPods(ctx)
	if err != nil {
		return nil, err
	}
	return surge.ReschedulableRequests(pods, cand.Status.NodeName), nil
}

// surgeReady reports whether the placeholder is Running on a Ready host distinct
// from the candidate node and in the same NodePool (spec §5.2). It takes the
// already-fetched placeholder (the pending handler reads it once per pass) to
// avoid a second Get.
func (r *RotationReconciler) surgeReady(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim, ph *corev1.Pod) (string, bool, error) {
	if ph == nil || ph.Status.Phase != corev1.PodRunning {
		return "", false, nil
	}
	host := ph.Spec.NodeName
	if host == "" || host == cand.Status.NodeName {
		return "", false, nil
	}
	node, err := r.getNode(ctx, host)
	if err != nil {
		return "", false, err
	}
	if node == nil || !nodeReady(node) || node.Labels[karpv1.NodePoolLabelKey] != pool.Name {
		return "", false, nil
	}
	return host, true, nil
}

// inducedClaim identifies the surge NodeClaim: first via the placeholder's bind
// target (its node's claim), then — when no bind ever happened — as the pool's
// claim created after started-at with no registered Node (spec §3.3 Rollback).
func (r *RotationReconciler) inducedClaim(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (string, error) {
	ph, err := r.getPlaceholder(ctx, cand.Name)
	if err != nil {
		return "", err
	}
	if ph != nil && ph.Spec.NodeName != "" {
		name, err := r.claimForNode(ctx, pool, ph.Spec.NodeName)
		if err != nil {
			return "", err
		}
		if name != "" {
			return name, nil
		}
	}

	startedAt, ok := parseTime(cand.Annotations[annotations.StartedAt])
	if !ok {
		return "", nil
	}
	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		return "", err
	}
	var best string
	var bestTime time.Time
	for i := range claims {
		c := &claims[i]
		if c.Name == cand.Name || c.Status.NodeName != "" {
			continue // skip the candidate and any claim that registered a Node
		}
		if !c.CreationTimestamp.After(startedAt) {
			continue
		}
		if best == "" || c.CreationTimestamp.Time.Before(bestTime) {
			best, bestTime = c.Name, c.CreationTimestamp.Time
		}
	}
	return best, nil
}

func (r *RotationReconciler) claimForNode(ctx context.Context, pool *karpv1.NodePool, nodeName string) (string, error) {
	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		return "", err
	}
	for i := range claims {
		if claims[i].Status.NodeName == nodeName {
			return claims[i].Name, nil
		}
	}
	return "", nil
}

// reapSurgeClaim deletes the induced claim on rollback, guarded so it never
// removes an absorb host: only a claim created after started-at, and whose node
// hosts nothing but the placeholder (+ DaemonSets). No registered Node passes
// trivially (spec §3.3 Rollback).
func (r *RotationReconciler) reapSurgeClaim(ctx context.Context, cand *karpv1.NodeClaim, surgeClaimName string) error {
	if surgeClaimName == "" {
		return nil
	}
	sc, err := r.getClaim(ctx, surgeClaimName)
	if err != nil || sc == nil {
		return err // already gone → nothing to do
	}
	startedAt, ok := parseTime(cand.Annotations[annotations.StartedAt])
	if !ok {
		return nil // cannot verify the after-start guard → never reap
	}
	if !sc.CreationTimestamp.After(startedAt) {
		return nil // pre-existing claim, not this attempt's
	}
	if sc.Status.NodeName != "" {
		pods, err := r.allPods(ctx)
		if err != nil {
			return err
		}
		if hostsRealPods(pods, sc.Status.NodeName, surge.PlaceholderName(cand.Name)) {
			return nil // absorb host — never reap
		}
	}
	return client.IgnoreNotFound(r.Delete(ctx, sc))
}

// hostsRealPods reports whether nodeName carries any reschedulable Pod other than
// the placeholder — DaemonSet/mirror/completed Pods do not count (spec §3.3). It
// shares the surge package's infra/completed filter; node-pinned Pods are counted
// here (an absorb host's real workload) even though surge sizing excludes them.
func hostsRealPods(pods []corev1.Pod, nodeName, placeholderName string) bool {
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != nodeName || p.Name == placeholderName {
			continue
		}
		if surge.IsInfraOrCompleted(p) {
			continue
		}
		return true
	}
	return false
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ── Placeholder creation ───────────────────────────────────────────────────

func (r *RotationReconciler) createPlaceholder(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim, res resolved) error {
	if cand.Status.NodeName == "" {
		return nil // no candidate node to replicate from yet; retry on a later pass
	}
	node, err := r.getNode(ctx, cand.Status.NodeName)
	if err != nil || node == nil {
		return err
	}
	pods, err := r.allPods(ctx)
	if err != nil {
		return err
	}
	excluded, err := r.excludedHostnames(ctx, pool, cand, node, res)
	if err != nil {
		return err
	}
	ph := surge.BuildPlaceholder(surge.PlaceholderInputs{
		Candidate:         cand,
		Node:              node,
		Pool:              pool,
		Requests:          surge.ReschedulableRequests(pods, cand.Status.NodeName),
		Match:             r.Policy.Surge.MatchNodeRequirements,
		ExcludedHostnames: excluded,
		PriorityClassName: r.PriorityClassName,
		Image:             r.PlaceholderImage,
		Namespace:         r.Namespace,
	})
	return client.IgnoreAlreadyExists(r.Create(ctx, ph))
}

// excludedHostnames is the placeholder's hostname NotIn set: the candidate node
// plus every near-deadline host (a triggered claim's node) so the surge does not
// land on a node that will itself rotate soon (spec §3.3).
func (r *RotationReconciler) excludedHostnames(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim, candNode *corev1.Node, res resolved) ([]string, error) {
	set := map[string]bool{hostnameOf(candNode): true}
	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		return nil, err
	}
	sel := r.selInputs(res, r.now())
	for i := range claims {
		c := &claims[i]
		if c.Name == cand.Name || c.Status.NodeName == "" || !selection.Triggered(c, sel) {
			continue
		}
		h, err := r.hostname(ctx, c.Status.NodeName)
		if err != nil {
			return nil, err
		}
		set[h] = true
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out, nil
}

func hostnameOf(n *corev1.Node) string {
	if h := n.Labels[corev1.LabelHostname]; h != "" {
		return h
	}
	return n.Name
}

func (r *RotationReconciler) hostname(ctx context.Context, nodeName string) (string, error) {
	n, err := r.getNode(ctx, nodeName)
	if err != nil {
		return "", err
	}
	if n == nil {
		return nodeName, nil
	}
	return hostnameOf(n), nil
}

// ── Client wrappers ────────────────────────────────────────────────────────

// anchorRotation writes the only-if-absent anchor with optimistic concurrency:
// the resourceVersion the caller holds makes a racing write fail with Conflict.
func (r *RotationReconciler) anchorRotation(ctx context.Context, pool *karpv1.NodePool, name string) error {
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[annotations.ActiveRotation] = name
	return r.Update(ctx, pool)
}

func (r *RotationReconciler) clearAnchor(ctx context.Context, pool *karpv1.NodePool) error {
	return r.patchPool(ctx, pool, func(m map[string]string) {
		delete(m, annotations.ActiveRotation)
		delete(m, annotations.ActiveRotationState)
	})
}

// patchPool applies an idempotent annotation mutation to the NodePool with
// retry-on-conflict (each attempt re-reads the latest object), reflecting the
// result back into pool.
func (r *RotationReconciler) patchPool(ctx context.Context, pool *karpv1.NodePool, mutate func(map[string]string)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh karpv1.NodePool
		if err := r.Get(ctx, client.ObjectKeyFromObject(pool), &fresh); err != nil {
			return err
		}
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		mutate(fresh.Annotations)
		if err := r.Update(ctx, &fresh); err != nil {
			return err
		}
		*pool = fresh
		return nil
	})
}

// patchClaim applies an idempotent annotation mutation to the named NodeClaim
// with retry-on-conflict. A vanished claim is a no-op.
func (r *RotationReconciler) patchClaim(ctx context.Context, name string, mutate func(map[string]string)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var c karpv1.NodeClaim
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &c); err != nil {
			return client.IgnoreNotFound(err)
		}
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		mutate(c.Annotations)
		return r.Update(ctx, &c)
	})
}

// patchNode applies a node mutator (applyFreeze/applyCordon/applyUnfreeze) with
// retry-on-conflict, skipping the Update when nothing changed. A vanished node is
// a no-op.
func (r *RotationReconciler) patchNode(ctx context.Context, nodeName string, mutate func(*corev1.Node) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var n corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &n); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !mutate(&n) {
			return nil
		}
		return r.Update(ctx, &n)
	})
}

func (r *RotationReconciler) freezeNode(ctx context.Context, nodeName, claimName string) error {
	if nodeName == "" {
		return nil
	}
	return r.patchNode(ctx, nodeName, func(n *corev1.Node) bool { return applyFreeze(n, claimName) })
}

func (r *RotationReconciler) cordonNode(ctx context.Context, nodeName string) error {
	if nodeName == "" {
		return nil
	}
	return r.patchNode(ctx, nodeName, applyCordon)
}

// unfreezeNodes reverses the freeze/cordon on every node carrying this rotation's
// surge-for marker (the old node, plus the surge target once frozen).
func (r *RotationReconciler) unfreezeNodes(ctx context.Context, claimName string) error {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return err
	}
	for i := range nodes.Items {
		if nodes.Items[i].Annotations[annotations.SurgeFor] != claimName {
			continue
		}
		if err := r.patchNode(ctx, nodes.Items[i].Name, applyUnfreeze); err != nil {
			return err
		}
	}
	return nil
}

func (r *RotationReconciler) deletePlaceholder(ctx context.Context, claimName string) error {
	ph := &corev1.Pod{}
	ph.Namespace = r.Namespace
	ph.Name = surge.PlaceholderName(claimName)
	return client.IgnoreNotFound(r.Delete(ctx, ph))
}

func (r *RotationReconciler) getPlaceholder(ctx context.Context, claimName string) (*corev1.Pod, error) {
	var p corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: surge.PlaceholderName(claimName)}, &p)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *RotationReconciler) getClaim(ctx context.Context, name string) (*karpv1.NodeClaim, error) {
	var c karpv1.NodeClaim
	err := r.Get(ctx, types.NamespacedName{Name: name}, &c)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *RotationReconciler) getNode(ctx context.Context, name string) (*corev1.Node, error) {
	var n corev1.Node
	err := r.Get(ctx, types.NamespacedName{Name: name}, &n)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (r *RotationReconciler) poolClaims(ctx context.Context, pool *karpv1.NodePool) ([]karpv1.NodeClaim, error) {
	var list karpv1.NodeClaimList
	if err := r.List(ctx, &list, client.MatchingLabels{karpv1.NodePoolLabelKey: pool.Name}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *RotationReconciler) allPods(ctx context.Context) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ── time helpers ───────────────────────────────────────────────────────────

func rfc3339(t time.Time) string { return t.Format(time.RFC3339) }

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func parseInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// maxRFC3339 returns the later of two RFC3339 timestamps, formatted; an
// unset/unparseable side loses to a parseable one.
func maxRFC3339(a, b string) string {
	ta, oka := parseTime(a)
	tb, okb := parseTime(b)
	switch {
	case !oka:
		return b
	case !okb:
		return a
	case tb.After(ta):
		return b
	default:
		return a
	}
}
