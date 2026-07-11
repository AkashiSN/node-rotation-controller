package controller

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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

	noderotationv1alpha1 "github.com/AkashiSN/node-rotation-controller/api/v1alpha1"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/policy"
	"github.com/AkashiSN/node-rotation-controller/internal/resolve"
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

	// Namespace, PlaceholderImage, and PriorityClassName configure the surge
	// placeholder Pod (spec §3.3).
	Namespace         string
	PlaceholderImage  string
	PriorityClassName string

	// Recorder emits the §4.2 metrics/alerts; nil means no-op.
	Recorder Recorder
	// Events emits the §4.2 / §3.2-layer-3 warning Events (issue #50); nil
	// disables event emission (log-only).
	Events events.EventRecorder

	// Clock is the time source; nil means time.Now (overridden in tests).
	Clock func() time.Time

	// warnOnce lazily builds the single warningEmitter so its in-memory dedup
	// state persists across reconciles even when the reconciler is constructed
	// directly (tests) rather than through SetupWithManager.
	warnOnce    sync.Once
	warnEmitter *warningEmitter

	// sweepOnce gates the spec §5.3 startup sweep into the first Reconcile so it
	// completes before any NodePool can start or resume a rotation. Registering
	// the sweep as a separate manager Runnable did not order it against the
	// reconcile loop — controller-runtime starts leader runnables concurrently —
	// so the sweep could read a stale anchor snapshot and reap a live rotation's
	// artifacts. Do blocks every concurrent reconcile until the sweep returns.
	sweepOnce sync.Once
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

func (r *RotationReconciler) warn() *warningEmitter {
	r.warnOnce.Do(func() { r.warnEmitter = newWarningEmitter(r.Events) })
	return r.warnEmitter
}

// Reconcile resolves the request to its NodePool and runs one rotation step.
// Out-of-scope NodePools are ignored without a requeue; in-scope ones always
// return a RequeueAfter, so the periodic re-evaluation (window edges, freeze
// releases — spec §5.2) is realized by the self-requeue rather than a separate
// Ticker.
func (r *RotationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Run the §5.3 startup sweep exactly once, before any reconcile does work, so
	// it never operates on a stale anchor snapshot while a new rotation is being
	// created (PR #33 review). It is best-effort: errors are logged, never
	// returned, so a transient API hiccup neither fails the reconcile nor
	// re-arms the sweep — the next controller restart re-attempts.
	r.sweepOnce.Do(func() {
		if err := r.Sweep(ctx); err != nil {
			log.FromContext(ctx).Error(err, "startup sweep encountered errors")
		}
	})

	var pool karpv1.NodePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			// The NodePool is gone; drop its metric series so the recomputed gauges
			// don't latch at their last value once its reconciles stop (§4.2).
			// NodePool is cluster-scoped, so the request name is the pool name.
			r.recorder().ForgetPool(req.Name)
			r.warn().Forget(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the single RotationPolicy that governs this NodePool (spec §5.4):
	// most-specific selector wins; an equal-specificity tie or a runtime-invalid
	// policy is a hard conflict; a NodePool matched by no policy is not rotated.
	pol, sched, conflict, err := r.governingPolicy(ctx, &pool)
	switch {
	case err != nil:
		// Transient: listing RotationPolicies failed. Requeue with backoff; do not
		// treat it as a conflict (no event, no gauge flip).
		return ctrl.Result{}, err
	case conflict != "":
		// Tie or runtime-invalid policy: refuse to rotate this pool, never guess
		// (#119 §3). Drop the stale rotation gauges so they don't latch, raise the
		// conflict gauge, and emit a deduplicated Warning event. The misconfig is
		// re-evaluated on the next self-requeue and on any RotationPolicy change.
		log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("rotation policy conflict; not rotating", "detail", conflict)
		r.recorder().ForgetPool(pool.Name)
		r.recorder().ObservePolicyConflict(pool.Name, true)
		r.warn().EmitConflict(ctx, &pool, conflict)
		// A contested pool stops being advanced, so an in-flight rotation anchored on
		// it would be orphaned; roll it back while still surfacing the conflict (#141).
		if err := r.reapUngovernedRotation(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	case pol == nil:
		// No governing policy: not rotated (the expireAfter backstop still applies,
		// spec §4). An in-flight rotation anchored before governance was lost is
		// reaped first — no future reconcile would touch the now-ungoverned pool, so
		// leaving its placeholder and do-not-disrupt marker would leak them (#141).
		// Then drop any series left by a policy that used to govern it; a future
		// RotationPolicy create/update re-enqueues the pool via the watch.
		if err := r.reapUngovernedRotation(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
		r.recorder().ForgetPool(pool.Name)
		r.warn().Forget(pool.Name)
		return ctrl.Result{}, nil
	}

	// Governed: clear any prior conflict signal and run one rotation step.
	r.recorder().ObservePolicyConflict(pool.Name, false)
	r.warn().ClearConflict(pool.Name)
	return r.reconcileNodePool(ctx, &pool, pol, sched)
}

// governingPolicy resolves the RotationPolicy that governs pool (spec §5.4).
// The return shape is a tri-state plus a transient error:
//   - err != nil          → listing failed; the caller requeues with backoff.
//   - conflict != ""       → an equal-specificity tie or a runtime-invalid policy;
//     the caller refuses to rotate and surfaces it (the string is the event detail).
//   - pol == nil (no err)  → no policy matches; the pool is not rotated.
//   - pol != nil           → the governing policy and its maintenance schedule.
func (r *RotationReconciler) governingPolicy(ctx context.Context, pool *karpv1.NodePool) (*policy.Policy, *window.Schedule, string, error) {
	var list noderotationv1alpha1.RotationPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil, nil, "", err
	}

	winner, outcome, tied := resolve.Governing(pool, list.Items)
	switch outcome {
	case resolve.NoMatch:
		return nil, nil, "", nil
	case resolve.Conflict:
		return nil, nil, fmt.Sprintf("NodePool is matched by multiple equally-specific RotationPolicies %v; refusing to rotate until the overlap is resolved", tied), nil
	}

	pol, err := resolve.ToPolicy(winner.Spec)
	if err != nil {
		return nil, nil, fmt.Sprintf("RotationPolicy %q is invalid: %v", winner.Name, err), nil
	}
	sched, err := window.New(pol.MaintenanceWindows)
	if err != nil {
		return nil, nil, fmt.Sprintf("RotationPolicy %q has an unbuildable schedule: %v", winner.Name, err), nil
	}
	return pol, sched, "", nil
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
		// A RotationPolicy create/update/delete re-evaluates EVERY NodePool: adding,
		// editing, or removing one policy can change which policy wins — or whether a
		// tie exists — for any pool the change's selector touches, and removing a
		// policy can hand a pool to a different one (spec §5.4). Enqueuing all pools
		// is the simple, always-correct mapping; the pool count bounds the fan-out.
		Watches(&noderotationv1alpha1.RotationPolicy{}, handler.EnqueueRequestsFromMapFunc(r.allNodePools)).
		Complete(r)
}

// allNodePools enqueues a reconcile for every NodePool — the conservative mapping
// for a RotationPolicy change, whose effect on policy resolution is not local to a
// single pool (spec §5.4).
func (r *RotationReconciler) allNodePools(ctx context.Context, _ client.Object) []reconcile.Request {
	var pools karpv1.NodePoolList
	if err := r.List(ctx, &pools); err != nil {
		log.FromContext(ctx).Error(err, "listing NodePools to re-evaluate a RotationPolicy change")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(pools.Items))
	for i := range pools.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: pools.Items[i].Name}})
	}
	return reqs
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

// resolved holds the per-NodePool policy and schedule that govern the pool
// (resolved from its RotationPolicy, spec §5.4) plus the durations derived from
// them. pol and sched are carried so the methods threaded with a resolved no
// longer read a single cluster-wide Policy/Schedule off the reconciler.
type resolved struct {
	pol           *policy.Policy
	sched         *window.Schedule
	leadTime      selection.LeadTime // K·P + t_rot, resolved per claim (§3.2)
	override      *time.Duration     // explicit ageThreshold, nil in auto mode
	retryBackoff  time.Duration
	readyTimeout  time.Duration
	cooldown      time.Duration
	drainBound    time.Duration  // tGP + buffer; DrainFallback when tGP unset (§5.2)
	drainEstimate *time.Duration // surge.drainEstimate; nil => schedule resolves min(tGP, default). Layer-2 forecast only (§3.2)
}

func (r *RotationReconciler) resolve(pool *karpv1.NodePool, pol *policy.Policy, sched *window.Schedule) resolved {
	s := pol.Surge
	tgp, unset := poolTGP(pool)
	p, _ := sched.WorstCasePeriod()

	override, isAuto, _ := pol.AgeThresholdOverride()
	var ov *time.Duration
	if !isAuto {
		d := override
		ov = &d
	}

	drainBound := tgp + schedule.Buffer
	if unset {
		drainBound = schedule.DrainFallback
	}

	// nil is meaningful: schedule.Derive resolves an unset estimate to
	// min(tGP, DrainEstimateDefault). Never fold it into leadTime or drainBound.
	var drainEst *time.Duration
	if s.DrainEstimate != nil {
		drainEst = new(s.DrainEstimate.Duration)
	}

	return resolved{
		pol:   pol,
		sched: sched,
		// Base omits tGP; LeadTime.For adds each claim's own terminationGracePeriod
		// so a template tGP shortened after a claim was stamped cannot under-estimate
		// the per-node lead time (§3.2, per-node trigger). The pool tGP above feeds
		// only the representative drainBound (§5.2) and schedule.Derive validation.
		leadTime: selection.LeadTime{
			Base:          time.Duration(pol.K())*p + s.ReadyTimeout.Duration + schedule.Buffer,
			DrainFallback: schedule.DrainFallback,
		},
		override:      ov,
		retryBackoff:  s.RetryBackoff.Duration,
		readyTimeout:  s.ReadyTimeout.Duration,
		cooldown:      s.CooldownAfter.Duration,
		drainBound:    drainBound,
		drainEstimate: drainEst,
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

func (r *RotationReconciler) selInputs(res resolved, now time.Time, excluded map[string]bool) selection.Inputs {
	return selection.Inputs{
		Now:          now,
		LeadTime:     res.leadTime,
		Override:     res.override,
		RetryBackoff: res.retryBackoff,
		Excluded:     excluded,
	}
}

// reconcileNodePool is the §5.2 driver: drive any in-flight rotation first
// (serial), else evaluate the start gates and begin a new one. pol and sched are
// the NodePool's governing RotationPolicy and its maintenance schedule, resolved
// in Reconcile (spec §5.4).
func (r *RotationReconciler) reconcileNodePool(ctx context.Context, pool *karpv1.NodePool, pol *policy.Policy, sched *window.Schedule) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("nodepool", pool.Name)
	now := r.now()
	res := r.resolve(pool, pol, sched)

	// List the pool's claims once and feed both the §4.2 gauges and candidate
	// selection: the state is identical for both and unchanged in between (step 2
	// writes nothing), so a single read avoids a redundant cache list per pass.
	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Build the opt-out set (§3.2): claims on a Node carrying an operator-set
	// do-not-disrupt are declined for proactive rotation. One label-scoped Node
	// list per pass, shared by the candidates gauge (step 0) and the pick (step 3).
	excluded, err := r.excludedClaims(ctx, pool, claims)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Derive the §3.2 thresholds + feasibility findings once from current state;
	// the §4.2 gauges (step 0) and the fatal-feasibility gate (step 1b) share them.
	// WorstCasePeriod/ShortestWindow's ok is always true here: policy.Validate
	// rejects an empty maintenanceWindows (and empty days), so the Schedule always
	// has ≥1 occurrence. N is the pool's in-scope claim count (issue #36).
	// ShortestIdleGap is genuinely undefined for a continuously-open window, and a
	// nil gap tells Derive to skip the carry-over check (issue #211).
	p, _ := sched.WorstCasePeriod()
	d, _ := sched.ShortestWindow()
	var idleGap *time.Duration
	if g, ok := sched.ShortestIdleGap(); ok {
		idleGap = &g
	}
	derived := r.derivedThresholds(pool, res, p, d, idleGap, len(claims))

	// ── 0. Emit the §4.2 reconcile-time gauges from current state, every pass.
	r.observe(pool, res, now, claims, p, derived, excluded)

	// Per-pass heartbeat at debug verbosity (issue #100): a single un-deduplicated
	// line every reconcile so liveness is visible at raised -v / -zap-devel even
	// when no findings change — unlike the transition-deduped warning above, which
	// stays silent in steady state. The authoritative liveness signal remains the
	// controller_runtime_reconcile_* / workqueue_* metrics (spec §4.2); this log is
	// a human-readable aid, not a substitute for them.
	log.V(1).Info("reconcile",
		"phase", reconcilePhase(pool),
		"candidates", selection.CountEligible(claims, r.selInputs(res, now, excluded)),
		"claims", len(claims),
		"inWindow", sched.InWindow(now),
		"findings", len(derived.Findings))

	// Surface non-fatal feasibility findings and per-node short-lead conditions
	// (issue #50): deduplicated logs + Warning Events. Fatal findings keep their
	// own §5.2 gate behavior below.
	r.warn().EmitFindings(ctx, pool, derived.Findings)
	r.warn().EmitShortLead(ctx, pool, claims, res.leadTime)

	// ── 1. Drive the in-flight rotation, keyed on the anchor (it outlives the
	//        old NodeClaim's deletion on success).
	if name := pool.Annotations[annotations.ActiveRotation]; name != "" {
		return r.advance(ctx, pool, name, res)
	}

	// ── 1b. Fatal feasibility gate (spec §3.2 layer 1): a schedule that cannot
	//        guarantee the configured rotation chances (A ≤ 0, override G < 1,
	//        K < 1, no windows) must NOT start a new rotation — the §2.2 invariant
	//        is "validation fails when the schedule cannot guarantee the configured
	//        chances". This gates only the start; an in-flight rotation (step 1)
	//        is already past here and runs to completion/rollback regardless.
	if f, ok := firstFatal(derived.Findings); ok {
		log.Info("schedule feasibility is fatal; not starting a rotation",
			"code", f.Code, "detail", f.Message)
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	// ── 2. Candidate-independent start gates.
	if open, gate := r.startGates(pool, res, now); !open {
		r.warn().EmitNoCandidate(ctx, pool, gate)
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	// ── 3. Pick the candidate, gate on its headroom, then anchor.
	sel := r.selInputs(res, now, excluded)
	cand := selection.PickEarliestDeadlineEligible(claims, sel)
	if cand == nil {
		// The candidates gauge reports that the count is zero; only the census says
		// why (issue #221): a claim excluded because its drain began is otherwise
		// indistinguishable from one that entered retryBackoff.
		r.warn().EmitNoCandidateCensus(ctx, pool, selection.TakeCensus(claims, sel))
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}
	// A candidate that cannot complete a graceful surge before its own deadline
	// rotates surge-less when the opt-in fallback is enabled (spec §3.3); it has
	// no surge, so the headroom gate (which sizes the placeholder) does not apply.
	surgeless := r.surgelessFallback(cand, res, now)
	if !surgeless {
		ok, err := r.headroomFits(ctx, pool, cand)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			log.Info("insufficient limits headroom; cannot surge", "candidate", cand.Name)
			return ctrl.Result{RequeueAfter: longRequeue}, nil
		}
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
	// The rotation now owns the pool's serial gate. Announce the pick and the
	// numbers that produced it, and reset the idle dedup so the next quiet period
	// re-reports its reason (issue #221).
	r.warn().ClearNoCandidate(pool.Name)
	kv := []any{
		"nodeclaim", cand.Name,
		"age", now.Sub(cand.CreationTimestamp.Time).Round(time.Second).String(),
		"eligible", selection.CountEligible(claims, sel),
		"surgeless", surgeless,
	}
	if dl, ok := claimDeadline(cand); ok {
		kv = append(kv, "deadline", rfc3339(dl))
	}
	log.Info("rotation candidate selected", kv...)
	if r.Events != nil {
		r.Events.Eventf(pool, cand, corev1.EventTypeNormal, reasonRotationStarted, actionRotateNode,
			"rotating NodeClaim %s", cand.Name)
	}
	if surgeless {
		return r.startForcefulFallback(ctx, pool, cand)
	}
	return r.advance(ctx, pool, cand.Name, res)
}

// surgelessFallback reports whether the candidate must rotate via the opt-in
// window-bounded surge-less path (spec §3.3): the fallback is enabled and a
// graceful surge started now cannot finish before the candidate's own deadline
// (deadline − now < t_rot), so the surge would only lose the race to Forceful
// Expiration. The pool is already confirmed in-window by startGates. A candidate
// with expireAfter: Never has no deadline and never qualifies.
func (r *RotationReconciler) surgelessFallback(cand *karpv1.NodeClaim, res resolved, now time.Time) bool {
	if !res.pol.Surge.ForcefulFallback.Enabled {
		return false
	}
	dl, ok := claimDeadline(cand)
	if !ok {
		return false
	}
	// t_rot = readyTimeout + tGP + Buffer (spec §3.2); res.drainBound is tGP + Buffer.
	tRot := res.readyTimeout + res.drainBound
	return dl.Sub(now) < tRot
}

// claimDeadline returns the candidate's Forceful Expiration deadline
// (creationTimestamp + spec.expireAfter); ok is false for expireAfter: Never
// (nil Duration), which has no deadline (mirrors internal/selection).
func claimDeadline(cand *karpv1.NodeClaim) (time.Time, bool) {
	e := cand.Spec.ExpireAfter.Duration
	if e == nil {
		return time.Time{}, false
	}
	return cand.CreationTimestamp.Add(*e), true
}

// observe computes and emits the §4.2 reconcile-time gauges from the live claims
// (listed once by the caller) on every pass. Recomputing each pass is what lets
// the 0/1 and "highest"/"count" gauges reset: a cleared drain stops alerting, a
// pool with no failures reports zero retries. The window-active gauge is
// per-NodePool — each pool's governing-policy schedule resolves independently
// (spec §5.4) — and set here because the reconcile is the only periodic tick (§5.2).
func (r *RotationReconciler) observe(pool *karpv1.NodePool, res resolved, now time.Time, claims []karpv1.NodeClaim, p time.Duration, derived schedule.Result, excluded map[string]bool) {
	rec := r.recorder()
	rec.ObserveWindow(pool.Name, res.sched.InWindow(now))

	o := PoolObservation{
		Candidates:      selection.CountEligible(claims, r.selInputs(res, now, excluded)),
		ShortLeadNodes:  selection.CountShortLead(claims, res.leadTime),
		RetryCount:      highestRetry(claims),
		DrainStuck:      r.drainStuck(pool, claims, res, now),
		WindowPeriod:    p,
		AgeThreshold:    derived.A,
		RotationChances: derived.G,
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

// derivedThresholds runs the §3.2 derivation for the pool's representative
// template expireAfter and returns the full schedule.Result: the derived
// ageThreshold A and guaranteed chances G feed the
// noderotation_age_threshold_seconds / noderotation_rotation_chances gauges, and
// the Findings are consumed by the feasibility gate (issue #27). The layer-2
// throughput inputs windowLen (D), idleGap and nodeCount (N) must be passed so the
// throughput findings are meaningful (issues #36, #211); A and G do not depend on
// them. A nil idleGap skips the carry-over check. res.drainEstimate feeds only the
// layer-2 forecast (t_rot_est); a nil value lets schedule.Derive resolve it to
// min(tGP, DrainEstimateDefault) (issue #212).
// A never-expiring template has no derivation: an override A still applies, but
// no chances can be guaranteed and no findings are produced — drainEstimate is
// irrelevant to that early return.
func (r *RotationReconciler) derivedThresholds(pool *karpv1.NodePool, res resolved, p, windowLen time.Duration, idleGap *time.Duration, nodeCount int) schedule.Result {
	eptr := pool.Spec.Template.Spec.ExpireAfter.Duration
	if eptr == nil {
		if res.override != nil {
			return schedule.Result{A: *res.override}
		}
		return schedule.Result{}
	}
	tgp, unset := poolTGP(pool)
	return schedule.Derive(schedule.Inputs{
		E:              *eptr,
		TGP:            tgp,
		TGPWasUnset:    unset,
		P:              p,
		WindowLen:      windowLen,
		IdleGap:        idleGap,
		ReadyTimeout:   res.readyTimeout,
		Cooldown:       res.cooldown,
		DrainEstimate:  res.drainEstimate,
		RetryBackoff:   res.retryBackoff,
		K:              res.pol.K(),
		MaxUnavailable: res.pol.SurgeMaxUnavailable(),
		NodeCount:      nodeCount,
		Override:       res.override,
	})
}

// reconcilePhase reports a coarse, human-readable phase for the per-pass debug
// heartbeat (issue #100): the in-flight rotation's state when one is anchored,
// else "idle". It reads the same anchor annotations the reconcile drives on, so
// it never adds a client call.
func reconcilePhase(pool *karpv1.NodePool) string {
	if pool.Annotations[annotations.ActiveRotation] == "" {
		return "idle"
	}
	if st := pool.Annotations[annotations.ActiveRotationState]; st != "" {
		return st
	}
	return annotations.StatePending
}

// firstFatal returns the first Fatal finding (spec §3.2 layer 1), if any. Used to
// gate a NodePool out of starting new rotations when its schedule cannot
// guarantee the configured rotation chances (issue #27).
func firstFatal(findings []schedule.Finding) (schedule.Finding, bool) {
	for _, f := range findings {
		if f.Severity == schedule.Fatal {
			return f, true
		}
	}
	return schedule.Finding{}, false
}

// Names of the §5.2 step-2 start gates, reported by startGates so a blocked pool
// can say which gate held it rather than idling silently (issue #221).
const (
	gateOutOfWindow          = "outOfWindow"
	gateFrozen               = "frozen"
	gateCooldownAfterSuccess = "cooldownAfterSuccess"
	gateCooldownAfterFailure = "cooldownAfterFailure"
)

// startGates is the §5.2 step-2 gate set, shared verbatim with the failed →
// pending re-entry so the two never diverge. It returns the name of the first
// gate that blocks the start, or "" when all are open.
func (r *RotationReconciler) startGates(pool *karpv1.NodePool, res resolved, now time.Time) (bool, string) {
	switch {
	case !res.sched.InWindow(now):
		return false, gateOutOfWindow
	case frozen(pool, now):
		return false, gateFrozen
	case since(pool.Annotations[annotations.LastRotationAt], now) < res.cooldown:
		return false, gateCooldownAfterSuccess
	case since(pool.Annotations[annotations.LastFailureAt], now) < res.cooldown:
		return false, gateCooldownAfterFailure
	}
	return true, ""
}

// advance runs one step for the in-flight rotation, keyed by the anchor name. res
// carries the NodePool's governing policy and schedule (spec §5.4), resolved once
// by the caller.
func (r *RotationReconciler) advance(ctx context.Context, pool *karpv1.NodePool, name string, res resolved) (ctrl.Result, error) {
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

	// Assert pending + write-once started-at (a single claim update). Capture the
	// authoritative started-at from inside the mutator — either the value already
	// present or the one we stamp this pass — so the readyTimeout check below never
	// depends on a stale cache re-read. A cached Get that briefly lags this write
	// would observe started-at empty, making now − parseTime("") trivially exceed
	// readyTimeout and roll back a freshly selected candidate instantly (#95 item 3).
	var stampedStartedAt string
	if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
		m[annotations.State] = annotations.StatePending
		if m[annotations.StartedAt] == "" {
			m[annotations.StartedAt] = rfc3339(r.now())
		}
		stampedStartedAt = m[annotations.StartedAt]
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
	// failure path cannot resurrect the placeholder (spec §5.2). Use the started-at
	// captured at patch time (not the re-read above, which may lag the write and
	// read empty — #95 item 3); the re-read still serves the other fields below.
	startedAt, _ := parseTime(stampedStartedAt)
	if r.now().Sub(startedAt) > res.readyTimeout {
		return r.failPending(ctx, pool, res, cand)
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
	if !ready {
		// A placeholder the scheduler has rejected stalls until readyTimeout with no
		// other controller-side signal; say why now, once (issue #221).
		r.warn().EmitPlaceholderPending(ctx, pool.Name, cand, ph)
		return ctrl.Result{RequeueAfter: shortRequeue}, nil
	}

	surgeWait := r.now().Sub(startedAt)
	r.warn().ClearPlaceholderPending(pool.Name, cand.Name)
	if err := r.freezeNode(ctx, host, cand.Name); err != nil {
		return ctrl.Result{}, err
	}
	// Durable phase record BEFORE the delete — it decides the completion
	// outcome — plus the drain-start anchor for the §4.2 drain histogram,
	// stamped write-once in the same update so a re-run never moves it.
	if err := r.patchPool(ctx, pool, func(m map[string]string) {
		m[annotations.ActiveRotationState] = annotations.StateDraining
		if m[annotations.DrainingAt] == "" {
			m[annotations.DrainingAt] = rfc3339(r.now())
			// Carry surge_wait forward write-once alongside draining-at: the old
			// NodeClaim (and its started-at) is deleted just below, so completion —
			// a different reconcile pass — could not otherwise recover it to report
			// the whole rotation's total = surge_wait + drain (#228, spec §5.3).
			m[annotations.SurgeWait] = surgeWait.String()
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
		m[annotations.State] = annotations.StateDraining
	}); err != nil {
		return ctrl.Result{}, err
	}
	// surge_wait phase complete: started-at → surge_ready (§4.2). Observed only
	// after the claim's pending → draining write has landed. A pass that fails
	// either write above is retried from this same phase (the writes are
	// idempotent by design), so an observation placed before them would take a
	// second, strictly-larger sample on every retry (same started-at anchor, a
	// later now) and inflate the histogram with a duration no rotation took. A
	// controller that dies between the write and this line drops one sample
	// instead — for a histogram that is the correct trade: a missing sample lowers
	// _count truthfully, a phantom sample reports a duration that never occurred.
	r.recorder().ObserveDuration(pool.Name, PhaseSurgeWait, surgeWait)
	// Both lines are emitted only after that same write has landed, for the same
	// reason — an emission before it would repeat on every retry.
	l := log.FromContext(ctx).WithValues("nodepool", pool.Name)
	l.Info("surge node ready", "nodeclaim", cand.Name, "surgeNode", host,
		"surgeWait", surgeWait.Round(time.Second).String())
	l.Info("drain started", "nodeclaim", cand.Name, "node", cand.Status.NodeName, "mode", "surge")
	if err := client.IgnoreNotFound(r.Delete(ctx, cand)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: shortRequeue}, nil
}

// startForcefulFallback begins an opt-in surge-less rotation (spec §3.3): with no
// surge to provision, it records the rotation as forceful-fallback on the anchor,
// transitions straight to draining, and deletes the old NodeClaim so Karpenter's
// termination controller drains it via the voluntary path (PDBs apply). The drain
// and completion reuse advanceDraining/completeOrAbort unchanged. No placeholder,
// no readyTimeout, no node freeze — there is no surge pair to protect. A crash
// after the state write but before the delete is healed by advanceDraining, which
// re-issues the delete (rotation_controller.go ~846).
func (r *RotationReconciler) startForcefulFallback(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	// Durable phase + mode record BEFORE the delete: the mode lives on the anchor
	// (the candidate is deleted just below), and DrainingAt is the §4.2 drain
	// histogram start, stamped write-once.
	if err := r.patchPool(ctx, pool, func(m map[string]string) {
		m[annotations.ActiveRotationState] = annotations.StateDraining
		m[annotations.RotationMode] = annotations.RotationModeForcefulFallback
		if m[annotations.DrainingAt] == "" {
			m[annotations.DrainingAt] = rfc3339(r.now())
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
		m[annotations.State] = annotations.StateDraining
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.recorder().ForcefulFallback(pool.Name, cand.Name)
	if r.Events != nil {
		r.Events.Eventf(pool, nil, corev1.EventTypeWarning, reasonForcefulFallback, actionForcefulFallback,
			"rotating NodeClaim %s surge-less: a graceful surge cannot complete before its deadline; deleting in-window via the voluntary path (PDBs apply)", cand.Name)
	}
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("drain started",
		"nodeclaim", cand.Name, "node", cand.Status.NodeName, "mode", annotations.RotationModeForcefulFallback)
	if err := client.IgnoreNotFound(r.Delete(ctx, cand)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: shortRequeue}, nil
}

// abortPendingExpiry handles a candidate caught force-expiring in pending: clean
// up the runtime objects, mark the claim terminally expired (before releasing the
// anchor), and emit expired — never success, no cooldown (spec §5.2).
func (r *RotationReconciler) abortPendingExpiry(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	r.warn().ClearPlaceholderPending(pool.Name, cand.Name)
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
// res supplies the timeouts the failure line reports back to the operator.
func (r *RotationReconciler) failPending(ctx context.Context, pool *karpv1.NodePool, res resolved, cand *karpv1.NodeClaim) (ctrl.Result, error) {
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

	// Say why the attempt was rolled back, how many have been made, and when the
	// next one becomes possible — the claim-scoped escalated backoff (issue #221).
	retry := parseInt(cand.Annotations[annotations.RetryCount]) + 1
	backoffUntil := r.now().Add(selection.EscalatedBackoff(retry, res.retryBackoff))
	r.warn().ClearPlaceholderPending(pool.Name, cand.Name)
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("rotation attempt failed",
		"nodeclaim", cand.Name,
		"reason", "readyTimeout",
		"readyTimeout", res.readyTimeout.String(),
		"retryCount", retry,
		"backoffUntil", rfc3339(backoffUntil))
	if r.Events != nil {
		r.Events.Eventf(cand, pool, corev1.EventTypeWarning, reasonRotationFailed, actionRotateNode,
			"the surge node did not become Ready within readyTimeout %v; rolled back, attempt %d, next attempt no earlier than %s",
			res.readyTimeout, retry, rfc3339(backoffUntil))
	}

	// Single pool update (last): the inter-attempt pause anchor + the gate release.
	if err := r.patchPool(ctx, pool, func(m map[string]string) {
		m[annotations.LastFailureAt] = rfc3339(r.now())
		clearRotationAnchorFields(m)
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
	open, _ := r.startGates(pool, res, now)
	if open &&
		now.Sub(failedAt) >= selection.EscalatedBackoff(retry, res.retryBackoff) &&
		headroomOK {
		if err := r.patchClaim(ctx, cand.Name, func(m map[string]string) {
			m[annotations.State] = annotations.StatePending
		}); err != nil {
			return ctrl.Result{}, err
		}
		return r.advance(ctx, pool, cand.Name, res) // falls into the pending handler, re-stamps started-at
	}

	// Otherwise: repair a torn failure write (crash between the failed write and
	// the pool update). Re-stamp last-failure-at = max(existing, failed-at) so the
	// §4.4 pause is never voided, then release the gate.
	if err := r.patchPool(ctx, pool, func(m map[string]string) {
		m[annotations.LastFailureAt] = maxRFC3339(m[annotations.LastFailureAt], cand.Annotations[annotations.FailedAt])
		clearRotationAnchorFields(m)
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
	// Recover the surge node for the completion line BEFORE unfreezeNodes strips its
	// surge-for marker (#228). "" on the surge-less forceful-fallback path.
	surgeNode := r.surgeHostFor(ctx, name)
	if err := r.deletePlaceholder(ctx, name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.unfreezeNodes(ctx, name); err != nil {
		return ctrl.Result{}, err
	}
	r.warn().ClearPlaceholderPending(pool.Name, name)
	if pool.Annotations[annotations.ActiveRotationState] == annotations.StateDraining {
		r.recorder().Success(pool.Name) // emit before releasing the gate (at-least-once)
		// drain phase complete: draining-at → finalization (§4.2). Guarded so a
		// rotation that reached draining before this anchor existed is uncounted
		// rather than mis-anchored. Read the anchor's fields before the patch below
		// erases them, but observe the histogram and log only after it lands.
		drain, hasDrain := time.Duration(0), false
		kv := []any{"nodeclaim", name, "mode", rotationMode(pool)}
		if surgeNode != "" {
			kv = append(kv, "surgeNode", surgeNode)
		}
		// surge_wait was carried forward from the transition (#228); absent on the
		// surge-less forceful-fallback path, which has no surge phase.
		surgeWait, hasSurgeWait := parseDuration(pool.Annotations[annotations.SurgeWait])
		if hasSurgeWait {
			kv = append(kv, "surgeWait", surgeWait.Round(time.Second).String())
		}
		if drainingAt, ok := parseTime(pool.Annotations[annotations.DrainingAt]); ok {
			drain, hasDrain = r.now().Sub(drainingAt), true
			kv = append(kv, "drain", drain.Round(time.Second).String())
		}
		// total = surge_wait + drain: the whole rotation on one line, but only when
		// both phases are known — never a partial sum mislabelled as the total.
		if hasSurgeWait && hasDrain {
			kv = append(kv, "total", (surgeWait + drain).Round(time.Second).String())
		}
		if err := r.patchPool(ctx, pool, func(m map[string]string) {
			m[annotations.LastRotationAt] = rfc3339(r.now())
			clearRotationAnchorFields(m)
		}); err != nil {
			return ctrl.Result{}, err
		}
		// Observed only after the anchor-clearing write lands: a failed patch
		// re-enters completeOrAbort with draining-at still set, and an observation
		// placed before it would take a second, strictly-larger sample (same
		// draining-at anchor, a later now) on every retry — inflating the histogram
		// with a duration no rotation took. A controller that dies between the write
		// and this line drops one sample instead; for a histogram that is the correct
		// trade — a missing sample lowers _count truthfully, a phantom sample reports
		// a duration that never occurred. The Success counter deliberately keeps its
		// at-least-once placement above — a lost count is worse than a duplicated one.
		if hasDrain {
			r.recorder().ObserveDuration(pool.Name, PhaseDrain, drain)
		}
		log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("rotation complete", kv...)
		if r.Events != nil {
			r.Events.Eventf(pool, nil, corev1.EventTypeNormal, reasonRotationCompleted, actionRotateNode,
				"NodeClaim %s rotated", name)
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

// rotationMode names how the in-flight rotation is replacing its node: the
// surge-less fallback stamps the anchor, everything else is the default surge.
func rotationMode(pool *karpv1.NodePool) string {
	if m := pool.Annotations[annotations.RotationMode]; m != "" {
		return m
	}
	return "surge"
}

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

// candidateRequests sums the reschedulable Pod requests on the candidate node
// and applies the same clamp createPlaceholder does, so the surge_headroom gate
// (spec §5.2 step 3) tests the actual placeholder's footprint against the
// NodePool budget — the pre-check the spec §4.2 note describes. Testing the
// un-clamped sum here would reject exactly the nearly-full nodes the clamp exists
// to keep rotatable under a tight-but-sufficient budget (issue #224). A refused
// clamp returns the full drain, which is correct: that rotation rolls back
// regardless of the budget. An unscheduled candidate has none.
func (r *RotationReconciler) candidateRequests(ctx context.Context, cand *karpv1.NodeClaim) (corev1.ResourceList, error) {
	if cand.Status.NodeName == "" {
		return corev1.ResourceList{}, nil
	}
	pods, err := r.allPods(ctx)
	if err != nil {
		return nil, err
	}
	requests := surge.ReschedulableRequests(pods, cand.Status.NodeName)
	return surge.Clamp(requests, cand.Status.Allocatable, surge.DaemonSetRequests(pods, cand.Status.NodeName)).Requests, nil
}

// surgeReady reports whether the placeholder is Running on a Ready host distinct
// from the candidate node and in the same NodePool (spec §5.2). It takes the
// already-fetched placeholder (the pending handler reads it once per pass) to
// avoid a second Get.
//
// A terminating placeholder (deletionTimestamp set, e.g. preempted by a
// higher-priority Pod during its grace period) does not count as ready: its
// reservation capacity is already being removed, so advancing to old NodeClaim
// deletion would violate make-before-break (issue #28). The pending handler then
// stays pending until the terminating placeholder is gone and a fresh one is
// recreated, bounded by readyTimeout.
func (r *RotationReconciler) surgeReady(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim, ph *corev1.Pod) (string, bool, error) {
	if ph == nil || ph.Status.Phase != corev1.PodRunning || ph.DeletionTimestamp != nil {
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
		if hostsRealPods(pods, sc.Status.NodeName, r.Namespace, surge.PlaceholderName(cand.Name)) {
			return nil // absorb host — never reap
		}
	}
	return client.IgnoreNotFound(r.Delete(ctx, sc))
}

// hostsRealPods reports whether nodeName carries any reschedulable Pod other than
// the placeholder — DaemonSet/mirror/completed Pods do not count (spec §3.3). It
// shares the surge package's infra/completed filter; node-pinned Pods are counted
// here (an absorb host's real workload) even though surge sizing excludes them.
//
// The placeholder is excluded by its full identity — namespace AND name — because
// Pod names are unique only within a namespace and this list is cluster-wide. A
// workload Pod in another namespace that happens to share the placeholder's name
// must still count as real, or the rollback could reap a NodeClaim that hosts it
// (issue #37).
func hostsRealPods(pods []corev1.Pod, nodeName, placeholderNamespace, placeholderName string) bool {
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != nodeName ||
			(p.Namespace == placeholderNamespace && p.Name == placeholderName) {
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
	requests := surge.ReschedulableRequests(pods, cand.Status.NodeName)
	// Clamp the placeholder to what Karpenter can actually provision for a fresh
	// node of this instance type — NodeClaim.status.allocatable minus DaemonSet
	// overhead — so a node the scheduler filled past Karpenter's per-AZ cached
	// estimate is still rotatable (issue #224). No-op when allocatable is absent.
	clamp := surge.Clamp(requests, cand.Status.Allocatable, surge.DaemonSetRequests(pods, cand.Status.NodeName))
	// clamp.Requests is the full drain on both the common path and a refused clamp
	// (DaemonSet overhead exhausts allocatable, so no clamp value induces a node —
	// sizing the placeholder to zero would satisfy surge_ready with nothing
	// reserved, a silent break-before-make; keep it full and unschedulable so the
	// rotation rolls back). band bounds the shortfall of a clamp that did fire.
	band := surge.Band(node.Status.Allocatable, cand.Status.Allocatable)
	ph := surge.BuildPlaceholder(surge.PlaceholderInputs{
		Candidate:         cand,
		Node:              node,
		Pool:              pool,
		Requests:          clamp.Requests,
		Match:             res.pol.Surge.MatchNodeRequirements,
		ExcludedHostnames: excluded,
		PriorityClassName: r.PriorityClassName,
		Image:             r.PlaceholderImage,
		Namespace:         r.Namespace,
	})
	if err := r.Create(ctx, ph); err != nil {
		// A cached read can report the placeholder absent just after it was created;
		// the create is idempotent, but the line below must not claim a creation that
		// this pass did not perform.
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	// State BOTH the computed requests and the census that produced them. Karpenter's
	// FailedScheduling message reports the capacity it must find — these requests PLUS
	// the DaemonSet overhead it adds to any fresh node — which reads like a
	// double-count of the DaemonSet Pods unless the controller says what it excluded
	// (issue #221).
	c := surge.CensusOnNode(pods, cand.Status.NodeName)
	kv := []any{
		"nodeclaim", cand.Name,
		"placeholder", ph.Name,
		"requests", formatRequests(clamp.Requests),
		"reschedulablePods", c.Counted,
		"daemonSetPods", c.DaemonSet,
		"mirrorPods", c.Mirror,
		"completedPods", c.Completed,
		"nodePinnedPods", c.NodePinned,
	}
	// Three mutually exclusive surge states, each announced on this one line and,
	// except the common path, with a matching Event (issue #224):
	//   - refused: DaemonSet overhead exhausts allocatable; the placeholder keeps
	//     the full drain, stays unschedulable, and the rotation rolls back.
	//   - clamped: the placeholder gives up a bounded shortfall; if that shortfall
	//     exceeds the measured band, the controller's accounting has diverged from
	//     the scheduler's and it says so — but still proceeds.
	//   - common: the drain fits; the line is exactly the #221 line, silent.
	l := log.FromContext(ctx).WithValues("nodepool", pool.Name)
	switch {
	case clamp.Refused:
		kv = append(kv, "clampRefused", clamp.RefusedResource)
		l.Info("surge placeholder created", kv...)
		if r.Events != nil {
			r.Events.Eventf(cand, pool, corev1.EventTypeWarning, reasonSurgeClampRefused, actionProvisionSurge,
				"DaemonSet overhead leaves no provisionable capacity for %s; the surge placeholder cannot be clamped and the rotation will roll back — opt into surge.forcefulFallback for surge-less rotation",
				clamp.RefusedResource)
		}
	case clamp.Clamped:
		kv = append(kv,
			"clamped", true,
			"unclamped", formatRequests(requests),
			"limit", formatRequests(clamp.Limit),
			"shortfall", formatRequests(clamp.Shortfall))
		over, exceeds := surge.ExceedsBand(clamp.Shortfall, band)
		if exceeds {
			kv = append(kv, "bandExceeded", over)
		}
		l.Info("surge placeholder created", kv...)
		if r.Events != nil {
			// Normal: a within-band clamp is a deliberate, bounded weakening of the
			// capacity guarantee, not a failure. It replaces the SurgeUnschedulable
			// Warning that an in-band node would otherwise stall on.
			r.Events.Eventf(cand, pool, corev1.EventTypeNormal, reasonSurgeClamped, actionProvisionSurge,
				"surge placeholder clamped to Karpenter's provisionable capacity (limit %s); %s below the full drain, absorbed by placeholder preemption and Karpenter follow-up",
				formatRequests(clamp.Limit), formatRequests(clamp.Shortfall))
			if exceeds {
				// Warning: the shortfall is larger than the per-AZ band explains, so a
				// modelling assumption (request accounting matching the scheduler's) no
				// longer holds. The rotation still proceeds; the divergence is surfaced.
				r.Events.Eventf(cand, pool, corev1.EventTypeWarning, reasonSurgeClampBandExceeded, actionProvisionSurge,
					"clamp shortfall on %s exceeds the measured per-AZ band (%s); the placeholder reserves less than one drain and the shortfall is no longer bounded by capacity variance",
					over, formatRequests(band))
			}
		}
	default:
		l.Info("surge placeholder created", kv...)
	}
	return nil
}

// formatRequests renders a ResourceList in a stable, greppable order so the
// placeholder's sizing can be compared against Karpenter's FailedScheduling
// message by eye.
func formatRequests(rl corev1.ResourceList) string {
	names := make([]string, 0, len(rl))
	for n := range rl {
		names = append(names, string(n))
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		q := rl[corev1.ResourceName(n)]
		parts = append(parts, n+"="+q.String())
	}
	return strings.Join(parts, ",")
}

// excludedHostnames is the placeholder's hostname NotIn set: the candidate node
// plus every near-deadline host (a triggered claim's node) so the surge prefers
// not to land on a node that will itself rotate soon (spec §3.3). The placeholder
// applies this set as a SOFT (preferred) anti-affinity, not a required term, so
// Karpenter can still provision a new surge node for it (issue #96); the candidate
// is hard-guaranteed off the placeholder by its cordon (applied in pending) plus
// surge_ready's host != candidate re-check, and the near-deadline exclusion is
// best-effort (spec §3.3 bounded residual). The set itself is unchanged.
func (r *RotationReconciler) excludedHostnames(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim, candNode *corev1.Node, res resolved) ([]string, error) {
	set := map[string]bool{hostnameOf(candNode): true}
	claims, err := r.poolClaims(ctx, pool)
	if err != nil {
		return nil, err
	}
	sel := r.selInputs(res, r.now(), nil)
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

// reapUngovernedRotation drives an in-flight rotation to a clean terminal state
// when the controller has ceased to govern the pool — no RotationPolicy matches
// it any longer, or it is contested by an unresolved tie (spec §5.4). In either
// case Reconcile stops advancing the pool, so without this reap the anchored
// rotation's artifacts would be orphaned: the placeholder Pod keeps holding
// capacity and the candidate node keeps its controller-owned do-not-disrupt
// marker, silently blocking Karpenter's voluntary disruption on that node
// indefinitely, with no future reconcile to clean either up (issue #141).
//
// It rolls the rotation back to the same clean state advanceExpired leaves:
// delete the placeholder, unfreeze every node carrying this rotation's surge-for
// marker (lifting the controller's do-not-disrupt and cordon while preserving an
// operator's own protections, spec §3.3/§5.3), and clear the anchor. It is a
// no-op when the pool carries no anchor, so the common ungoverned-idle-pool path
// is untouched.
func (r *RotationReconciler) reapUngovernedRotation(ctx context.Context, pool *karpv1.NodePool) error {
	claim := pool.Annotations[annotations.ActiveRotation]
	if claim == "" {
		return nil
	}
	log.FromContext(ctx).WithValues("nodepool", pool.Name, "claim", claim).
		Info("ceased to govern a pool mid-rotation; reaping orphaned rotation artifacts")
	if r.Events != nil {
		r.Events.Eventf(pool, nil, corev1.EventTypeWarning, reasonGovernanceLost, actionReapRotation,
			"NodePool left RotationPolicy governance with an in-flight rotation on %s; rolled it back (deleted placeholder, removed freeze markers and cordon, cleared anchor) so no do-not-disrupt marker or placeholder is orphaned",
			claim)
	}
	// The attempt ends here, so drop its unschedulable-placeholder dedup entry. The
	// no-policy caller drops the whole pool's warn state via Forget, but the
	// policy-conflict caller deliberately keeps it (to dedup the conflict itself),
	// and would otherwise retain this claim's key forever (issue #221).
	r.warn().ClearPlaceholderPending(pool.Name, claim)
	if err := r.deletePlaceholder(ctx, claim); err != nil {
		return err
	}
	if err := r.unfreezeNodes(ctx, claim); err != nil {
		return err
	}
	return r.clearAnchor(ctx, pool)
}

func (r *RotationReconciler) clearAnchor(ctx context.Context, pool *karpv1.NodePool) error {
	return r.patchPool(ctx, pool, clearRotationAnchorFields)
}

// clearRotationAnchorFields deletes every NodePool annotation scoped to a single
// in-flight rotation. It is the ONE place the anchor's field set is enumerated, so
// the success clear (completeOrAbort), the two failure clears (failPending and
// advanceFailed's torn-write repair) and the abort/reap clear (clearAnchor) can
// never drift — a field added to the anchor is cleared on every end path by
// editing this alone. It leaves the post-rotation anchors last-rotation-at /
// last-failure-at untouched; the caller writes those in the same update.
func clearRotationAnchorFields(m map[string]string) {
	delete(m, annotations.ActiveRotation)
	delete(m, annotations.ActiveRotationState)
	delete(m, annotations.DrainingAt)
	delete(m, annotations.SurgeWait)
	delete(m, annotations.RotationMode)
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

// surgeHostFor returns the surge target node still carrying this rotation's
// surge-for marker, for the self-contained completion line (#228). Callers must
// invoke it BEFORE unfreezeNodes, which strips the marker. On the success path
// the old node's NodeClaim has finalized away with its Node, so the surge target
// is the sole marked node; it returns "" when none survives (the surge-less
// forceful-fallback path, or already swept) and "" — rather than a guess — if
// more than one node is still marked, since this decorates a log line and must
// never fail a completion or name the wrong node.
func (r *RotationReconciler) surgeHostFor(ctx context.Context, claimName string) string {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return ""
	}
	host := ""
	for i := range nodes.Items {
		if nodes.Items[i].Annotations[annotations.SurgeFor] != claimName {
			continue
		}
		if host != "" {
			return "" // ambiguous (old node still marked) — omit rather than guess
		}
		host = nodes.Items[i].Name
	}
	return host
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

// excludedClaims returns the pool's NodeClaims opted out of proactive rotation:
// those scheduled onto a Node carrying an operator-set karpenter.sh/do-not-disrupt
// (spec §3.2). It lists the pool's Nodes once (label-scoped, symmetric with
// poolClaims) and maps the operator-opted-out nodes to their claims. Returns nil
// when nothing is opted out.
func (r *RotationReconciler) excludedClaims(ctx context.Context, pool *karpv1.NodePool, claims []karpv1.NodeClaim) (map[string]bool, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabels{karpv1.NodePoolLabelKey: pool.Name}); err != nil {
		return nil, err
	}
	return excludedClaimNames(claims, excludedNodeNames(nodes.Items)), nil
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

// parseDuration parses a Go duration string (as time.Duration.String() emits),
// reporting false on an absent or malformed value so an unset surge-wait anchor
// simply omits the completion line's total rather than reporting a zero (#228).
func parseDuration(s string) (time.Duration, bool) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d, true
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
