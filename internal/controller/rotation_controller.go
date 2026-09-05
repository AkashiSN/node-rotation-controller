package controller

import (
	"context"
	"fmt"
	"slices"
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
	"github.com/AkashiSN/node-rotation-controller/internal/adapt"
	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
	"github.com/AkashiSN/node-rotation-controller/internal/crd"
	"github.com/AkashiSN/node-rotation-controller/internal/decide"
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

// schedule.Buffer bounds this controller's own detection lag: each
// pending→draining→complete transition is observed at most one shortRequeue
// late, plus patch/delete round-trips. It lives in internal/schedule, which
// must not import this package (cycle), so pin the two together here — the side
// that already depends on schedule. A negative time.Duration constant is not
// convertible to uint, so these fail to COMPILE if Buffer ever stops tracking
// 4*shortRequeue in either direction.
const _ = uint(schedule.Buffer - 4*shortRequeue)
const _ = uint(4*shortRequeue - schedule.Buffer)

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

	pol, err := crd.ToPolicy(winner.Spec)
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
	pol                  *policy.Policy
	sched                *window.Schedule
	leadTime             selection.LeadTime // K·P + t_rot, resolved per claim (§3.2)
	override             *time.Duration     // explicit ageThreshold, nil in auto mode
	retryBackoff         time.Duration
	readyTimeout         time.Duration
	cooldown             time.Duration  // surge.cooldownAfter; gate A (post-success settle) + layer-2 forecast (§3.2). May be 0 (ADR-0004)
	failurePause         time.Duration  // surge.failurePause; gate B (post-failure inter-attempt pause); nil => max(FailurePauseFloor, cooldown) (§4.4, ADR-0004)
	drainBound           time.Duration  // tGP + buffer; DrainFallback when tGP unset (§5.2)
	drainEstimate        *time.Duration // surge.drainEstimate; nil => schedule resolves min(tGP, default). Layer-2 forecast only (§3.2)
	provisioningEstimate *time.Duration // surge.provisioningEstimate; nil => schedule resolves min(readyTimeout, default). Layer-2 forecast only (§3.2)
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

	// nil is meaningful the same way: schedule.Derive resolves an unset estimate to
	// min(readyTimeout, ProvisioningEstimateDefault). Forecast-side only (§3.2).
	var provEst *time.Duration
	if s.ProvisioningEstimate != nil {
		provEst = new(s.ProvisioningEstimate.Duration)
	}

	// gate B (post-failure pause): an unset failurePause defaults to
	// max(FailurePauseFloor, cooldownAfter) so lowering cooldownAfter for throughput
	// never silently shortens the failure pause (§4.4, ADR-0004). cooldownAfter is
	// always non-nil after ApplyDefaults.
	failurePause := max(policy.FailurePauseFloor, s.CooldownAfter.Duration)
	if s.FailurePause != nil {
		failurePause = s.FailurePause.Duration
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
		override:             ov,
		retryBackoff:         s.RetryBackoff.Duration,
		readyTimeout:         s.ReadyTimeout.Duration,
		cooldown:             s.CooldownAfter.Duration,
		failurePause:         failurePause,
		drainBound:           drainBound,
		drainEstimate:        drainEst,
		provisioningEstimate: provEst,
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

// selInputs maps the resolved policy onto the pure selection view. The occurrence
// bounds are resolved here rather than inside selection, which must stay free of
// internal/window for the wasm simulator (issue #320); zero bounds mean "no
// occurrence", which EffectiveBackoff reads as "no clamp".
//
// The bounds are resolved only when anyFailed is true. WindowStart/WindowEnd are
// read by exactly one predicate, selection.failedPastBackoff, reachable only for a
// claim already in the noderotation.io/state=failed state (stateAllows) — every
// other caller of the Inputs this builds never looks at them. OccurrenceBounds
// runs window's zone-and-transition-aligned audit on every call, which is wasted
// work when nothing failed is in view (issue #320; the same guard in
// internal/sim/loop.go measured 10x-406x wall-clock savings on the wasm
// simulator). A future consumer of these bounds that is not gated on the failed
// state must remove this guard, or it will silently be handed zero bounds.
func (r *RotationReconciler) selInputs(res resolved, now time.Time, excluded map[string]bool, anyFailed bool) selection.Inputs {
	in := selection.Inputs{
		Now:          now,
		LeadTime:     res.leadTime,
		Override:     res.override,
		RetryBackoff: res.retryBackoff,
		Excluded:     excluded,
	}
	if anyFailed {
		if start, end, ok := res.sched.OccurrenceBounds(now); ok {
			in.WindowStart, in.WindowEnd = start, end
		}
	}
	return in
}

// hasFailedClaim reports whether any claim view carries the failed state — the
// only condition under which selInputs' occurrence bounds are consumed (see
// selInputs for what this assumes).
func hasFailedClaim(views []selection.Claim) bool {
	for i := range views {
		if views[i].Annotations[annotations.State] == annotations.StateFailed {
			return true
		}
	}
	return false
}

// effectiveBackoff resolves the window-clamped re-selection backoff the selection
// predicate will apply. The failure announcement and advanceFailed's re-entry gate
// both go through it, so the instant an operator is told about, the instant the
// anchored retry acts on, and the instant the predicate reopens the claim cannot
// disagree (issue #320).
func (r *RotationReconciler) effectiveBackoff(res resolved, now, failedAt time.Time, retry int) time.Duration {
	var start, end time.Time
	if s, e, ok := res.sched.OccurrenceBounds(now); ok {
		start, end = s, e
	}
	return selection.EffectiveBackoff(retry, res.retryBackoff, failedAt, start, end)
}

// gateInputs maps the NodePool's resolved policy onto the pure decide view. The
// window verdict is resolved here — decide takes a bool, not a *window.Schedule, so
// the simulator can drive it from its virtual clock.
func (r *RotationReconciler) gateInputs(pool *karpv1.NodePool, res resolved, now time.Time) decide.Inputs {
	return decide.Inputs{
		Now:             now,
		InWindow:        res.sched.InWindow(now),
		Annotations:     pool.Annotations,
		Cooldown:        res.cooldown,
		FailurePause:    res.failurePause,
		FallbackEnabled: res.pol.Surge.ForcefulFallback.Enabled,
		ReadyTimeout:    res.readyTimeout,
		DrainBound:      res.drainBound,
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
	// views and byName project the same List result once (pointer-identity rule):
	// byName aliases &claims[i], so a pick resolves back to the Karpenter object
	// without a re-Get (internal/adapt).
	views, byName := adapt.Claims(claims)

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
	r.observe(pool, res, now, claims, views, p, derived, excluded)

	// Per-pass heartbeat at debug verbosity (issue #100): a single un-deduplicated
	// line every reconcile so liveness is visible at raised -v / -zap-devel even
	// when no findings change — unlike the transition-deduped warning above, which
	// stays silent in steady state. The authoritative liveness signal remains the
	// controller_runtime_reconcile_* / workqueue_* metrics (spec §4.2); this log is
	// a human-readable aid, not a substitute for them.
	log.V(1).Info("reconcile",
		"phase", reconcilePhase(pool),
		"candidates", selection.CountEligible(views, r.selInputs(res, now, excluded, hasFailedClaim(views))),
		"claims", len(claims),
		"inWindow", sched.InWindow(now),
		"findings", len(derived.Findings))

	// Surface non-fatal feasibility findings and per-node short-lead conditions
	// (issue #50): deduplicated logs + Warning Events. Fatal findings keep their
	// own §5.2 gate behavior below.
	r.warn().EmitFindings(ctx, pool, derived.Findings)
	r.warn().EmitShortLead(ctx, pool, claims, res.leadTime)

	// Record the maintenance window's open, and report an occurrence that closed
	// with candidates unrotated (§4.2, issue #303). Above every gate in this
	// function — Reconcile's governance branches already returned before this
	// point — the signal states what happened to the window, not why the
	// controller did not act.
	if err := r.evaluateWindowEdge(ctx, pool, res, now, views, excluded, sched.InWindow(now)); err != nil {
		return ctrl.Result{}, err
	}

	// ── 1. Drive the in-flight rotation, keyed on the anchor (it outlives the
	//        old NodeClaim's deletion on success).
	if name := pool.Annotations[annotations.ActiveRotation]; name != "" {
		return r.advance(ctx, pool, name, res)
	}

	// ── 1a. Static capacity gate (issue #302, spec §5.2): a NodePool with
	//        spec.replicas set keeps a fixed node count and is never considered by
	//        Karpenter's provisioner when a pod is pending. The surge placeholder
	//        pins karpenter.sh/nodepool to the candidate's own pool as a structural
	//        invariant (internal/surge/placeholder.go), so on a static pool it can
	//        neither be absorbed by another pool nor induce a new NodeClaim: every
	//        attempt would stall until readyTimeout and burn one of the node's
	//        guaranteed rotation chances. Refuse to start and say so once, rather
	//        than rediscovering it once per maintenance window. Like the fatal
	//        feasibility gate below, this gates only the START — an in-flight
	//        rotation (step 1) is already past here and runs to completion. That
	//        matters for an anchor written before this gate existed (an earlier
	//        controller version; Karpenter itself rejects adding spec.replicas to
	//        a running NodePool), which would otherwise be stranded with a
	//        cordoned node and an orphaned placeholder. advanceFailed's retry
	//        branch sits above this gate too and closes on static separately.
	if staticPool(pool) {
		r.warn().EmitStaticNodePool(ctx, pool)
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}
	r.warn().ClearStaticNodePool(pool.Name)

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
	gi := r.gateInputs(pool, res, now)
	if open, gate := decide.StartGate(gi); !open {
		r.warn().EmitNoCandidate(ctx, pool, string(gate))
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	// ── 3. Pick the candidate, gate on its headroom, then anchor.
	sel := r.selInputs(res, now, excluded, hasFailedClaim(views))
	pick := selection.PickEarliestDeadlineEligible(views, sel)
	if pick == nil {
		// The candidates gauge reports that the count is zero; only the census says
		// why (issue #221): a claim excluded because its drain began is otherwise
		// indistinguishable from one that entered retryBackoff.
		r.warn().EmitNoCandidateCensus(ctx, pool, selection.TakeCensus(views, sel))
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}
	// Resolve the pick back to the Karpenter object from the SAME List result.
	// Never re-Get by name: that would widen the list→patch window (spec §3, adapt).
	cand := byName[pick.Name]
	// A candidate that cannot complete a graceful surge before its own deadline
	// rotates surge-less when the opt-in fallback is enabled (spec §3.3); it has
	// no surge, so the headroom gate (which sizes the placeholder) does not apply.
	surgeless := decide.SurgelessFallback(pick, gi)
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
		"eligible", selection.CountEligible(views, sel),
		"surgeless", surgeless,
	}
	if pick.ExpireAfter != nil {
		kv = append(kv, "deadline", rfc3339(pick.CreatedAt.Add(*pick.ExpireAfter)))
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

// evaluateWindowEdge applies the §4.2 window-close evaluation: it records that a
// maintenance window opened, and when that window closes it reports whether the
// occurrence went by with candidates outstanding by age and state and no
// rotation attributable to it ever completing (issue #303).
//
// It runs at step 0, ABOVE the static-capacity gate and the fatal feasibility
// gate, on purpose. A pool that could not rotate because it is static (#302) or
// because its schedule is infeasible still lost the window, and the signal's
// meaning is fixed to that fact rather than to the controller's reason for not
// acting — a signal whose meaning depended on gate order would change meaning
// every time a gate was added. The outstanding count is therefore what the
// window left undone by age and state, including claims one of those later
// pool-level gates would have stopped anyway.
//
// "Attributable to the occurrence" is wider than "inside it": a window gates
// only rotation STARTS (§3.1), so an attempt that began in-window and succeeded
// after the boundary still settles the occurrence — the stamp is held for the
// in-flight rotation, and last-rotation-at then lands at or after it.
//
// The clear is conditional and the emission follows the write: only the pass
// whose Update actually removed the stamp reports the loss, so a retried or
// raced pass cannot double-count. That makes the report AT MOST once per
// occurrence: the clear lands before the emission, so a controller that stops
// in between drops the signal rather than inventing one — the same stance §4.2
// takes for the duration histogram. The counter and the Event have the same
// gap between them, so a stop there can leave one without the other.
//
// A successful write refreshes *pool in place (patchPoolIf's *pool = fresh),
// so on return the pool object can be newer than the res/views/excluded
// snapshot the caller took earlier in the same pass — those still reflect the
// claims as listed before this call, not as of the fresh annotations now on
// pool.
func (r *RotationReconciler) evaluateWindowEdge(
	ctx context.Context,
	pool *karpv1.NodePool,
	res resolved,
	now time.Time,
	views []selection.Claim,
	excluded map[string]bool,
	inWindow bool,
) error {
	in := decide.WindowInputs{
		Now:         now,
		InWindow:    inWindow,
		Annotations: pool.Annotations,
		Census:      selection.TakeCensus(views, r.selInputs(res, now, excluded, hasFailedClaim(views))),
	}
	action := decide.WindowEdge(in)
	opened := pool.Annotations[annotations.WindowOpenedAt]

	// claim turns a mutation into a conditional write that re-asserts the verdict
	// itself against the AUTHORITATIVE annotations: WindowEdge runs again inside
	// the write loop over the fresh map, and the write happens only if it still
	// yields the action being applied. One mechanism covers every
	// annotation-derived condition the verdict rested on — the stamp, the anchor,
	// the freeze, last-rotation-at — and it covers them in BOTH directions. A
	// freeze or an anchor that lands after the cached read vetoes a report that
	// is no longer true; a freeze or a last-rotation-at that has since gone from
	// the object vetoes the silent clear it justified, so a genuine loss is
	// judged again next pass instead of being swallowed for good (issue #304).
	//
	// Now, InWindow and Census stay as this pass computed them. The claims were
	// listed once at the top of the reconcile and cannot be re-listed cheaply, so
	// the re-assertion covers the annotation-derived conditions only — the census
	// is as fresh as the pass that decided, no fresher.
	//
	// The action alone does not identify the occurrence: a NEWER stamp yields the
	// same action, and clearing it would consume an occurrence this pass never
	// evaluated and report it under the earlier stamp. So the exact stamp this
	// pass saw is re-asserted alongside the action. For the stamp branch this is
	// the "veto if already parseable" guard the earlier code wrote by hand —
	// in-window with a parseable stamp is WindowNothing, not WindowStamp.
	claim := func(apply func(map[string]string)) func(map[string]string) bool {
		return func(m map[string]string) bool {
			fresh := in
			fresh.Annotations = m
			if decide.WindowEdge(fresh) != action || m[annotations.WindowOpenedAt] != opened {
				return false
			}
			apply(m)
			return true
		}
	}

	switch action {
	case decide.WindowStamp:
		stamp := rfc3339(now)
		_, err := r.patchPoolIf(ctx, pool, claim(func(m map[string]string) {
			m[annotations.WindowOpenedAt] = stamp
		}))
		return err

	case decide.WindowSettled:
		_, err := r.patchPoolIf(ctx, pool, claim(func(m map[string]string) {
			delete(m, annotations.WindowOpenedAt)
		}))
		return err

	case decide.WindowMissed:
		wrote, err := r.patchPoolIf(ctx, pool, claim(func(m map[string]string) {
			delete(m, annotations.WindowOpenedAt)
		}))
		if err != nil {
			return err
		}
		if !wrote {
			return nil
		}
		r.recorder().WindowMissed(pool.Name)
		r.warn().EmitWindowMissed(ctx, pool, opened, in.Census)
		return nil
	}
	// WindowNothing and WindowDefer: no write, nothing to announce.
	return nil
}

// observe computes and emits the §4.2 reconcile-time gauges from the live claims
// (listed once by the caller) on every pass. Recomputing each pass is what lets
// the 0/1 and "highest"/"count" gauges reset: a cleared drain stops alerting, a
// pool with no failures reports zero retries. The window-active gauge is
// per-NodePool — each pool's governing-policy schedule resolves independently
// (spec §5.4) — and set here because the reconcile is the only periodic tick (§5.2).
func (r *RotationReconciler) observe(pool *karpv1.NodePool, res resolved, now time.Time, claims []karpv1.NodeClaim, views []selection.Claim, p time.Duration, derived schedule.Result, excluded map[string]bool) {
	rec := r.recorder()
	rec.ObserveWindow(pool.Name, res.sched.InWindow(now))

	// One classification pass supplies both eligibility gauges. census.Eligible is
	// exactly what CountEligible returns — TakeCensus applies the same checks in
	// the same order, and selection.TestTakeCensusEligibleMatchesCountEligible
	// guards it — so the candidates gauge is unchanged and the second traversal
	// this used to make is gone.
	census := selection.TakeCensus(views, r.selInputs(res, now, excluded, hasFailedClaim(views)))
	o := PoolObservation{
		Candidates:      census.Eligible,
		InBackoff:       census.InBackoffTriggered,
		ShortLeadNodes:  selection.CountShortLead(views, res.leadTime),
		RetryCount:      highestRetry(claims),
		DrainStuck:      r.drainStuck(pool, claims, res, now),
		WindowPeriod:    p,
		AgeThreshold:    derived.A,
		RotationChances: derived.G,

		ThroughputCapacity: derived.C,
		TRotEstimate:       derived.TRotEst,
		TRotBound:          derived.TRot,
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
		E:                    *eptr,
		TGP:                  tgp,
		TGPWasUnset:          unset,
		P:                    p,
		WindowLen:            windowLen,
		IdleGap:              idleGap,
		ReadyTimeout:         res.readyTimeout,
		Cooldown:             res.cooldown,
		DrainEstimate:        res.drainEstimate,
		ProvisioningEstimate: res.provisioningEstimate,
		RetryBackoff:         res.retryBackoff,
		K:                    res.pol.K(),
		MaxUnavailable:       res.pol.SurgeMaxUnavailable(),
		NodeCount:            nodeCount,
		Override:             res.override,
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

// staticPool reports whether the NodePool is a Karpenter static capacity pool:
// spec.replicas maintains a fixed number of nodes rather than scaling to pod
// demand, and such a pool is excluded from the provisioner's candidates for a
// pending pod — which is exactly what the surge placeholder is (issue #302).
func staticPool(pool *karpv1.NodePool) bool {
	return pool.Spec.Replicas != nil
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

	// Assert pending + write-once started-at (a single claim update), and only from
	// the states advance() dispatches here on. The claim above is an informer-cache
	// read, so a pass can arrive holding a `pending` view of a claim whose durable
	// state has already moved on; asserting unconditionally would rewrite a rolled
	// back `failed` claim as `pending`, restart its readyTimeout deadline with a
	// fresh started-at and so bypass the escalated backoff only advanceFailed
	// enforces — while retry-count keeps the value that escalation was based on
	// (issue #307). Capture the authoritative started-at from inside the mutator —
	// either the value already present or the one we stamp this pass — so the
	// readyTimeout check below never depends on a stale cache re-read. A cached Get
	// that briefly lags this write would observe started-at empty, making
	// now − parseTime("") trivially exceed readyTimeout and roll back a freshly
	// selected candidate instantly (#95 item 3).
	var stampedStartedAt string
	wrote, err := r.patchClaimIf(ctx, cand.Name, func(m map[string]string) bool {
		if st := m[annotations.State]; st != "" && st != annotations.StatePending {
			return false
		}
		m[annotations.State] = annotations.StatePending
		if m[annotations.StartedAt] == "" {
			m[annotations.StartedAt] = rfc3339(r.now())
		}
		stampedStartedAt = m[annotations.StartedAt]
		return true
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if wrote != claimWritten {
		// Nothing was written, so this pass owns nothing and must touch none of the
		// rotation's runtime objects: the claim has vanished, or the handler for the
		// state it actually holds owns it and is reached on the next pass.
		return ctrl.Result{RequeueAfter: shortRequeue}, nil
	}
	cand, err = r.getClaim(ctx, cand.Name)
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
	if decide.Frozen(pool.Annotations, r.now()) {
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

// abortPendingExpiry handles a candidate caught force-expiring in pending: mark
// the claim terminally expired, clean up the runtime objects, release the anchor
// and emit expired — never success, no cooldown (spec §5.2).
//
// The conditional write comes FIRST, before the cleanup, because a pass that does
// not own this transition must not touch the rotation's runtime objects either:
// unfreezing on a stale view would strip the surge node's protection out from
// under a drain that is still running (issue #307). A crash between the write and
// the cleanup is repaired by advanceExpired, whose whole job is that cleanup.
func (r *RotationReconciler) abortPendingExpiry(ctx context.Context, pool *karpv1.NodePool, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	out, err := r.markExpired(ctx, cand.Name, func(m map[string]string) {
		delete(m, annotations.StartedAt)
		delete(m, annotations.SurgeClaim)
	}, "", annotations.StatePending)
	if err != nil {
		return ctrl.Result{}, err
	}
	if out == expiryGone || out == expiryRaced {
		// Nothing was written, so this pass owns nothing. Either the claim finalized
		// away — leave the anchor and the next pass reaches completeOrAbort, which
		// owns exactly that case — or the durable state has moved past this view, and
		// the handler for the state it actually holds owns the rotation.
		return ctrl.Result{RequeueAfter: shortRequeue}, nil
	}
	// Announce BEFORE the cleanup, which is fallible. The write already decided the
	// outcome, and a cleanup error sends the next reconcile to advanceExpired — the
	// terminal-state handler, which repairs the cleanup and deliberately never emits
	// — so an emission placed after it would be lost for good on an ordinary
	// transient API error, not merely deferred. That leaves only the irreducible
	// window: a controller that dies between the write and this line.
	if out == expiryClaimed {
		r.recorder().Expired(pool.Name, cand.Name)
	}
	r.warn().ClearPlaceholderPending(pool.Name, cand.Name)
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
	// gauge emission is needed here. retry is captured from inside the mutator, so
	// the attempt number and next-attempt instant reported below are the values this
	// write actually produced rather than the caller's cached copy, which may lag
	// them (issue #307).
	var retry int
	var failedAt time.Time
	wrote, err := r.patchClaimIf(ctx, cand.Name, func(m map[string]string) bool {
		m[annotations.State] = annotations.StateFailed
		// Truncate to the second the RFC3339 annotation stores, so the instant this
		// function reports is byte-for-byte the one failedPastBackoff parses back. A
		// second r.now() would differ by the dropped sub-second plus whatever the
		// clock moved in between (issue #320).
		failedAt = r.now().Truncate(time.Second)
		m[annotations.FailedAt] = rfc3339(failedAt)
		retry = parseInt(m[annotations.RetryCount]) + 1
		m[annotations.RetryCount] = strconv.Itoa(retry)
		delete(m, annotations.StartedAt)
		delete(m, annotations.SurgeClaim)
		return true
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if wrote != claimWritten {
		// The claim finalized away mid-rollback, so no attempt was recorded: retry
		// names no durable value, and there is nothing to announce or to pause on.
		// What actually happened is a force-expiry, and the anchor — left set — hands
		// it to completeOrAbort, which counts it as one (issue #307).
		return ctrl.Result{RequeueAfter: shortRequeue}, nil
	}
	r.recorder().Failure(pool.Name, cand.Name)

	// Say why the attempt was rolled back, how many have been made, and when the
	// next one becomes possible — the window-clamped backoff (issue #320), off the
	// failed-at this write just persisted.
	backoffUntil := failedAt.Add(r.effectiveBackoff(res, r.now(), failedAt, retry))
	r.warn().ClearPlaceholderPending(pool.Name, cand.Name)
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("rotation attempt failed",
		"nodeclaim", cand.Name,
		"reason", "readyTimeout",
		"readyTimeout", res.readyTimeout.String(),
		"retryCount", retry,
		"backoffUntil", rfc3339(backoffUntil))
	if r.Events != nil {
		r.Events.Eventf(cand, pool, corev1.EventTypeWarning, reasonRotationFailed, actionRotateNode,
			"the surge node did not become Ready within readyTimeout %v; rolled back, attempt %d, next attempt %s under the current schedule (a maintenanceWindows change recalculates it)",
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
// every start gate passes past the effective (window-aware) backoff, or repair a
// torn failure write by releasing the gate while preserving the pause anchor
// (spec §5.2).
func (r *RotationReconciler) advanceFailed(ctx context.Context, pool *karpv1.NodePool, res resolved, cand *karpv1.NodeClaim) (ctrl.Result, error) {
	if cand.DeletionTimestamp != nil {
		out, err := r.markExpired(ctx, cand.Name, nil, annotations.StateFailed)
		if err != nil {
			return ctrl.Result{}, err
		}
		if out == expiryGone || out == expiryRaced { // as in abortPendingExpiry: this pass owns nothing
			return ctrl.Result{RequeueAfter: shortRequeue}, nil
		}
		if out == expiryClaimed {
			r.recorder().Expired(pool.Name, cand.Name)
		}
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
	// A re-entry is a NEW attempt, so it must pass EVERYTHING a fresh start would —
	// including the static capacity gate (issue #302), which step 1a applies to a
	// fresh start but which this path sits above: the anchor sends the reconcile
	// into advance() at step 1 before that gate is reached. A static pool whose
	// anchor was written by an earlier controller version would otherwise retry
	// the one attempt that can never succeed, once per escalated backoff, forever.
	// Closing it here drops through to the repair branch below, which releases the
	// anchor and preserves the failure pause.
	open, _ := decide.StartGate(r.gateInputs(pool, res, now))
	if open && !staticPool(pool) &&
		now.Sub(failedAt) >= r.effectiveBackoff(res, now, failedAt, retry) &&
		headroomOK {
		// Only from failed — the state this handler was dispatched on. That guard is
		// also what bounds the re-entry below: advance() re-reads the claim through
		// the cache, and a read still lagging this very write dispatches straight back
		// here with every gate still open, which an unconditional write would answer
		// by starting the attempt over, once per turn round the loop (issue #307).
		wrote, err := r.patchClaimIf(ctx, cand.Name, func(m map[string]string) bool {
			if m[annotations.State] != annotations.StateFailed {
				return false
			}
			m[annotations.State] = annotations.StatePending
			return true
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		if wrote != claimWritten {
			// The claim vanished, or another pass already re-entered it and owns the
			// attempt; either way this pass starts nothing.
			return ctrl.Result{RequeueAfter: shortRequeue}, nil
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
//
// Both the outcome and the right to announce it come from the single write that
// releases the anchor, never from the NodePool this handler was called with. That
// copy is an informer-cache read, and completion is exactly when reconciles pile
// up on the pool — the old NodeClaim's delete and the surge node reaching Ready
// both enqueue it — so a pass can arrive here after an earlier pass already
// completed the rotation, on a cached pool whose anchor still looks set. Deciding
// from that copy counted one rotation twice and logged the completion line twice
// (issue #304). Deciding inside patchPoolIf, from the read the write itself is
// validated against, makes the counter, the histogram, the line and the Event
// fire once per released anchor.
//
// The cost is that a controller dying between the write and the emissions below
// drops them, where the previous ordering would have re-emitted on recovery. That
// is the trade the drain histogram already made for the same reason: a lost count
// understates truthfully, a duplicate reports a rotation that never happened, and
// a crash in that window is far rarer than the cache lag this replaces.
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

	// released: this pass is the one that cleared the anchor, so the emissions
	// below are its own. rotated: the mirror says the rotation reached draining
	// (success + cooldown) rather than force-expiring out of pending.
	var released, rotated, hasDrain, hasSurgeWait bool
	var drain, surgeWait time.Duration
	var mode string
	if _, err := r.patchPoolIf(ctx, pool, func(m map[string]string) bool {
		// RetryOnConflict re-runs this against a newer read, so every value it
		// produces is reset here and derived from that read alone.
		released, rotated, hasDrain, hasSurgeWait = false, false, false, false
		if m[annotations.ActiveRotation] != name {
			return false // an earlier pass already completed this rotation
		}
		released = true
		mode = rotationMode(m)
		rotated = m[annotations.ActiveRotationState] == annotations.StateDraining
		if rotated {
			// drain phase complete: draining-at → finalization (§4.2). Guarded so a
			// rotation that reached draining before this anchor existed is uncounted
			// rather than mis-anchored.
			if drainingAt, ok := parseTime(m[annotations.DrainingAt]); ok {
				drain, hasDrain = r.now().Sub(drainingAt), true
			}
			// surge_wait was carried forward from the transition (#228); absent on the
			// surge-less forceful-fallback path, which has no surge phase.
			surgeWait, hasSurgeWait = parseDuration(m[annotations.SurgeWait])
			m[annotations.LastRotationAt] = rfc3339(r.now()) // the cooldown starts here
		}
		clearRotationAnchorFields(m)
		return true
	}); err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case !released:
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	case !rotated:
		r.recorder().Expired(pool.Name, name) // vanished out of pending — nothing rotated
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	r.recorder().Success(pool.Name)
	if hasDrain {
		r.recorder().ObserveDuration(pool.Name, PhaseDrain, drain)
	}
	kv := []any{"nodeclaim", name, "mode", mode}
	if surgeNode != "" {
		kv = append(kv, "surgeNode", surgeNode)
	}
	if hasSurgeWait {
		kv = append(kv, "surgeWait", surgeWait.Round(time.Second).String())
	}
	if hasDrain {
		kv = append(kv, "drain", drain.Round(time.Second).String())
	}
	// total = surge_wait + drain: the whole rotation on one line, but only when
	// both phases are known — never a partial sum mislabelled as the total.
	if hasSurgeWait && hasDrain {
		kv = append(kv, "total", (surgeWait + drain).Round(time.Second).String())
	}
	log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("rotation complete", kv...)
	if r.Events != nil {
		r.Events.Eventf(pool, nil, corev1.EventTypeNormal, reasonRotationCompleted, actionRotateNode,
			"NodeClaim %s rotated", name)
	}
	return ctrl.Result{RequeueAfter: longRequeue}, nil
}

// ── Start-gate helpers ─────────────────────────────────────────────────────

// rotationMode names how the in-flight rotation is replacing its node, read from
// a NodePool's annotations: the surge-less fallback stamps the anchor, everything
// else is the default surge.
func rotationMode(anns map[string]string) string {
	if m := anns[annotations.RotationMode]; m != "" {
		return m
	}
	return "surge"
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
	views, _ := adapt.Claims(claims)
	sel := r.selInputs(res, r.now(), nil, hasFailedClaim(views))
	for i := range claims {
		c := &claims[i]
		if c.Name == cand.Name || c.Status.NodeName == "" {
			continue
		}
		v := views[i]
		if !selection.Triggered(&v, sel) {
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
//
// The anchor clear is both the last step and the write that claims the reap, and
// the line and Event follow it — the completion path's ordering (issue #304), not
// the terminal-claim one (issue #307). Two properties force it here (issue #315).
// The rollback must precede the clear: the anchor is the ONLY thing that brings a
// later reconcile back to this cleanup, since the reap returns early without one
// and the pool is ungoverned, so a clear followed by a failed deletePlaceholder
// would orphan the placeholder for good — the exact leak this function exists to
// prevent. And announcing before the clear announces nothing in particular: the
// reap is re-entered from the anchor the caller was handed, which is a cache read
// that still shows the anchor a previous pass already cleared, so every such pass
// re-announced one rotation. Only the write that clears the anchor identifies the
// pass that reaped it. The accepted cost is the one the completion path accepts:
// a controller dying between the write and the emissions drops them.
func (r *RotationReconciler) reapUngovernedRotation(ctx context.Context, pool *karpv1.NodePool) error {
	claim := pool.Annotations[annotations.ActiveRotation]
	if claim == "" {
		return nil
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
	reaped, err := r.clearAnchorIf(ctx, pool, claim)
	if err != nil || !reaped {
		return err
	}

	log.FromContext(ctx).WithValues("nodepool", pool.Name, "claim", claim).
		Info("ceased to govern a pool mid-rotation; reaped orphaned rotation artifacts")
	if r.Events != nil {
		r.Events.Eventf(pool, nil, corev1.EventTypeWarning, reasonGovernanceLost, actionReapRotation,
			"NodePool left RotationPolicy governance with an in-flight rotation on %s; rolled it back (deleted placeholder, removed freeze markers and cordon, cleared anchor) so no do-not-disrupt marker or placeholder is orphaned",
			claim)
	}
	return nil
}

func (r *RotationReconciler) clearAnchor(ctx context.Context, pool *karpv1.NodePool) error {
	return r.patchPool(ctx, pool, clearRotationAnchorFields)
}

// clearAnchorIf is clearAnchor with the veto that decides who announces the reap:
// it clears the anchor only while the pool still names claim, so of the passes
// that enter the reap from the same cached anchor exactly one performs the write,
// and that one owns the announcement (issue #315). It reports whether this pass
// was that one; false means an earlier pass already ended the rotation.
func (r *RotationReconciler) clearAnchorIf(ctx context.Context, pool *karpv1.NodePool, claim string) (bool, error) {
	var cleared bool
	_, err := r.patchPoolIf(ctx, pool, func(m map[string]string) bool {
		// RetryOnConflict re-runs this against a newer read, so the outcome is reset
		// here and derived from that read alone.
		cleared = false
		if m[annotations.ActiveRotation] != claim {
			return false
		}
		cleared = true
		clearRotationAnchorFields(m)
		return true
	})
	if err != nil {
		return false, err
	}
	return cleared, nil
}

// clearRotationAnchorFields deletes every NodePool annotation scoped to a single
// in-flight rotation. It is the ONE place the anchor's field set is enumerated, so
// the completion clear (completeOrAbort, both outcomes), the two failure clears
// (failPending and advanceFailed's torn-write repair) and the reap clear
// (clearAnchorIf) can never drift — a field added to the anchor is cleared on
// every end path by editing this alone. It leaves the post-rotation anchors
// last-rotation-at / last-failure-at untouched; the caller writes those in the
// same update.
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
	_, err := r.patchPoolIf(ctx, pool, func(m map[string]string) bool {
		mutate(m)
		return true
	})
	return err
}

// patchPoolIf is patchPool with a veto: mutate runs against the freshly read
// annotations and reports whether the result should be written. Returning false
// skips the Update; either way pool ends up holding the fresh object.
//
// The veto is what makes a conditional write possible. The reconciler reads its
// NodePool through the informer cache, so the annotations a handler was called
// with may already be behind the API server; only the read inside this loop is
// authoritative, and only a write that succeeds against it proves the caller was
// the one that made the transition (issue #304). A caller whose fresh read no
// longer shows the state it is acting on vetoes its write and learns it lost the
// race; a caller whose read is itself stale cannot write at all — its Update
// carries the stale resourceVersion and conflicts, so RetryOnConflict re-reads.
//
// The bool reports whether THIS pass performed the Update. It is produced by the
// retry loop and reset at the top of every attempt, never by the mutator: mutate
// may run several times under RetryOnConflict, so a result it carried would
// outlive the attempt that produced it (issue #307). Callers that emit a signal
// for a transition use it as the proof they owned that transition.
func (r *RotationReconciler) patchPoolIf(ctx context.Context, pool *karpv1.NodePool, mutate func(map[string]string) bool) (bool, error) {
	wrote := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Reset per attempt: mutate runs again on every conflict retry, so a result
		// carried over from a vetoed or failed attempt would report a write this
		// pass never made (issue #307).
		wrote = false
		var fresh karpv1.NodePool
		if err := r.Get(ctx, client.ObjectKeyFromObject(pool), &fresh); err != nil {
			return err
		}
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		if !mutate(fresh.Annotations) {
			*pool = fresh
			return nil
		}
		if err := r.Update(ctx, &fresh); err != nil {
			return err
		}
		wrote = true
		*pool = fresh
		return nil
	})
	return wrote, err
}

// patchClaim applies an idempotent annotation mutation to the named NodeClaim
// with retry-on-conflict. A vanished claim is a no-op.
func (r *RotationReconciler) patchClaim(ctx context.Context, name string, mutate func(map[string]string)) error {
	_, err := r.patchClaimIf(ctx, name, func(m map[string]string) bool {
		mutate(m)
		return true
	})
	return err
}

// claimWrite reports what a conditional NodeClaim write actually did.
type claimWrite int

const (
	claimGone    claimWrite = iota // the claim no longer exists; nothing was written
	claimVetoed                    // mutate refused the fresh state; nothing was written
	claimWritten                   // this pass performed the write
)

// patchClaimIf is patchClaim with a veto — the NodeClaim counterpart of
// patchPoolIf, and there for the same reason (issue #307). The claim a handler
// was called with is an informer-cache read, so only the read inside this loop
// is authoritative, and only a write that succeeds against it proves this pass
// made the transition it is about to announce. mutate reports whether its result
// should be written; returning false skips the Update.
//
// The outcome is produced HERE, and reset at the top of every attempt before the
// Get, rather than recorded by mutate: an attempt whose Update conflicts can be
// followed by one whose Get finds the claim finalized away, and mutate does not
// run on that second attempt. A flag left behind by the losing attempt would tell
// its caller it had written a claim that no longer exists.
func (r *RotationReconciler) patchClaimIf(ctx context.Context, name string, mutate func(map[string]string) bool) (claimWrite, error) {
	out := claimGone
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		out = claimGone
		var c karpv1.NodeClaim
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &c); err != nil {
			return client.IgnoreNotFound(err)
		}
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		if !mutate(c.Annotations) {
			out = claimVetoed
			return nil
		}
		if err := r.Update(ctx, &c); err != nil {
			return err
		}
		out = claimWritten
		return nil
	})
	return out, err
}

// expiry reports what markExpired did with a transition into terminal expired.
type expiry int

const (
	expiryGone    expiry = iota // the claim finalized away; nothing was written
	expiryRaced                 // the durable state is neither from nor expired
	expiryAlready               // an earlier pass made the transition and owns its announcement
	expiryClaimed               // this pass made the transition and owns its announcement
)

// markExpired performs a transition INTO the terminal expired state, but only
// from one of the states from names, and reports who did what. extra carries the
// transition's other annotation edits.
//
// advanceExpired — the handler for a claim ALREADY expired — deliberately never
// re-announces (spec §5.2). The veto extends that to the two paths that enter
// expired, which a claim view taken before an earlier pass wrote would otherwise
// announce a second time, overstating outcome="expired" exactly as the completion
// path overstated outcome="success" (issues #304, #307).
//
// from is the caller's own pre-state, not merely "not expired": a claim deleted
// while pending can reach draining through a pass that read it before the
// deletion, and a later pass dispatched from the older pending view must not
// overwrite a live drain with expired. That case is expiryRaced — nothing was
// written and the handler for the state the claim actually holds owns it.
func (r *RotationReconciler) markExpired(ctx context.Context, name string, extra func(map[string]string), from ...string) (expiry, error) {
	// already is read only when the write was vetoed, and a veto ends the retry loop
	// on the attempt that set it, so it can never describe an earlier attempt.
	var already bool
	w, err := r.patchClaimIf(ctx, name, func(m map[string]string) bool {
		cur := m[annotations.State]
		already = cur == annotations.StateExpired
		if already || !slices.Contains(from, cur) {
			return false
		}
		m[annotations.State] = annotations.StateExpired
		if extra != nil {
			extra(m)
		}
		return true
	})
	switch {
	case err != nil:
		return expiryGone, err
	case w == claimWritten:
		return expiryClaimed, nil
	case w == claimGone:
		return expiryGone, nil
	case already:
		return expiryAlready, nil
	default:
		return expiryRaced, nil
	}
}

// patchNode applies a node mutator (applyFreeze/applyCordon/applyUnfreeze) with
// retry-on-conflict, skipping the Update when nothing changed. A node already gone
// by the Get is a no-op; one that vanishes between the Get and the Update surfaces
// its NotFound to the caller, which is a retryable error on the reconcile paths
// and a non-event for the startup sweep (see sweepNodes). It reports whether it
// wrote, for callers that announce the change.
func (r *RotationReconciler) patchNode(ctx context.Context, nodeName string, mutate func(*corev1.Node) bool) (bool, error) {
	// As in patchClaimIf, the report is produced here and reset at the top of every
	// attempt, never taken from the mutator: an attempt whose Update conflicts can
	// be followed by one whose Get finds the node gone, and the mutator does not run
	// on that second attempt. "There was something to reverse" on the losing attempt
	// says nothing about the node the retry found (issues #307, #313).
	wrote := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		wrote = false
		var n corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &n); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !mutate(&n) {
			return nil
		}
		if err := r.Update(ctx, &n); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return wrote, err
}

func (r *RotationReconciler) freezeNode(ctx context.Context, nodeName, claimName string) error {
	if nodeName == "" {
		return nil
	}
	_, err := r.patchNode(ctx, nodeName, func(n *corev1.Node) bool { return applyFreeze(n, claimName) })
	return err
}

func (r *RotationReconciler) cordonNode(ctx context.Context, nodeName string) error {
	if nodeName == "" {
		return nil
	}
	_, err := r.patchNode(ctx, nodeName, applyCordon)
	return err
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
		if _, err := r.patchNode(ctx, nodes.Items[i].Name, applyUnfreeze); err != nil {
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

// deletePlaceholder removes the claim's placeholder Pod by its deterministic
// name — the reconcile paths address their own placeholder, which they created.
// A Pod already gone is a no-op. The startup sweep does NOT use this: it selects
// on the surge-for label and deletes the object that predicate found (see
// deleteSelected).
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
