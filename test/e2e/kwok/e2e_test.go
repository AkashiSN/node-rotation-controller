//go:build e2e

package kwok

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// Post-aging assertion budgets. Each surge subtest first sleeps the candidate up
// to ageThreshold, then calls eventually(...) to wait on the controller's
// reconcile + KWOK-Karpenter provision/drain. eventually starts its clock when it
// is called (after the sleep), so the timeout it is given IS the real headroom for
// the controller work — the aging sleep is not subtracted from it.
//
// The flake (#101) was therefore purely a magnitude problem: the per-wait budgets
// were too tight to absorb an observed ~2x runner-load spike (e.g. the absorb-bind
// and PDB-draining waits were only 5m30s and timed out at exactly 5m30s on a slow
// runner, while the same commit passed comfortably on a faster one). They were
// also needlessly coupled to ageThreshold (ageThreshold+90s / +120s), which made
// the headroom scale with how long we age the node rather than with the controller
// work it must cover.
//
// These fixed, ageThreshold-independent budgets decouple the wait from the aging
// sleep AND strictly exceed every previous per-wait timeout (the old values were
// 7m for new-provision bind/complete, 5m30s for absorb/confine/PDB bind-style
// waits, and 6m for drain/complete waits), so coverage only ever grows. They are
// generous on purpose: KWOK transitions take seconds, so a healthy runner finishes
// well inside them and the slack only absorbs load spikes; the budgets are spent in
// full only on a genuine failure, which the subtest fails on anyway.
const (
	// surgeBindBudget covers a single in-flight observation (placeholder binds /
	// candidate enters draining) after the aging sleep. Exceeds all prior bind-style
	// budgets (max was the 7m new-provision bind).
	surgeBindBudget = 8 * time.Minute
	// surgeCompleteBudget covers driving a rotation through to `complete` (the full
	// provision/absorb + drain chain) after the aging sleep. Exceeds all prior
	// complete budgets (max was 7m).
	surgeCompleteBudget = 10 * time.Minute
)

// TestKWOKSurge drives and asserts the v1 surge rotation lifecycle against the
// real Karpenter v1 KWOK reference cloudprovider on a kind cluster. The cluster
// is provisioned by bootstrap.sh (the Makefile e2e-kwok target / e2e.yaml job),
// not by this test; the test only drives workloads and asserts.
//
// Subtests run serially (each rotates pool-a) and clean up after themselves so
// the shared cluster returns to a known empty state. Each subtest documents
// which acceptance criterion (issue #92) it covers and any KWOK limitation.
func TestKWOKSurge(t *testing.T) {
	ctx := context.Background()
	cl := k(t)
	requireCluster(t, ctx, cl)

	// A clean slate before the suite: no leftover workloads/claims from a prior
	// run keep the age-based selection deterministic.
	resetCluster(ctx, t, cl)

	t.Run("NewProvisionSurge", func(t *testing.T) {
		testNewProvision(ctx, t, cl)
	})
	t.Run("CapacityAbsorbAndCompletion", func(t *testing.T) {
		testCapacityAbsorb(ctx, t, cl)
	})
	t.Run("MultiNodePoolConfinement", func(t *testing.T) {
		testConfinement(ctx, t, cl)
	})
	t.Run("VoluntaryDrainHonorsPDB", func(t *testing.T) {
		testPDB(ctx, t, cl)
	})
	t.Run("DoNotDisruptOnSurgePair", func(t *testing.T) {
		testDoNotDisrupt(ctx, t, cl)
	})
	t.Run("PlaceholderPreemptionVictim", func(t *testing.T) {
		testPreemption(ctx, t, cl)
	})
	t.Run("GovernanceLossReapsRotation", func(t *testing.T) {
		testGovernanceLossReap(ctx, t, cl)
	})
	t.Run("ForcefulFallbackSurgeless", func(t *testing.T) {
		testForcefulFallback(ctx, t, cl)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// New-provision surge (§3.3 PRIMARY path) — issue #96 acceptance.
//
// Covers the criterion issue #96 turned from negative to positive: a surge that
// reaches `complete` by INDUCING Karpenter to provision a BRAND-NEW NodePool-owned
// NodeClaim for the placeholder, WITHOUT any pre-existing spare to absorb onto.
//
// Why this works now (and did not before): the placeholder previously carried the
// candidate/near-deadline hostname exclusion as a HARD required nodeAffinity term.
// Core Karpenter v1 lists kubernetes.io/hostname in RestrictedLabels, so its
// provisioner rejected any provisionable Pod referencing that key ("using label
// kubernetes.io/hostname is not allowed …") and never provisioned a new node — the
// placeholder stayed Pending and the attempt rolled back at readyTimeout. The
// exclusion is now a SOFT preferred term (§3.3, issue #96): Karpenter's scheduler
// only relaxes preferred node-affinity and never folds it into the NodeClaim
// requirements, so it is never rejected and the new-provision path proceeds. The
// candidate stays off the placeholder via the controller's cordon (applied in
// pending) plus surge_ready's host != candidate re-check, not the (now soft)
// hostname term.
//
// Determinism: a single aged candidate in pool-a with NO spare. When it crosses
// ageThreshold the rotation starts; the placeholder cannot absorb (no spare), so
// the ONLY way it reaches Running is a freshly provisioned NodeClaim. We assert a
// NodeClaim whose name was NOT present pre-rotation appears, the placeholder binds
// to its node, and the rotation reaches `complete`.
func testNewProvision(ctx context.Context, t *testing.T, cl client.Client) {
	defer resetCluster(ctx, t, cl)
	// Keep the controller reconciling promptly through the static-KWOK quiet
	// periods so the aged candidate is picked within the window (see startNudger).
	defer startNudger(ctx, cl, poolA, poolB)()

	const ageThreshold = 4 * time.Minute

	// 1. A single candidate in pool-a, with NO spare staged anywhere in the pool —
	//    so the placeholder has nothing to absorb onto and a new node is the only
	//    path to surge_ready.
	applyDeployment(ctx, t, cl, deployment("cand", poolA, 300, ""))
	candClaim := waitClaimProvisioned(ctx, t, cl, poolA)

	before, err := listClaims(ctx, cl, poolA)
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := claimNames(before)
	if len(beforeNames) != 1 {
		t.Fatalf("want exactly one pre-rotation pool-a NodeClaim (the candidate), got %v", keys(beforeNames))
	}
	t.Logf("single candidate NodeClaim %s present, no spare; aging it to ageThreshold", candClaim)

	// 2. Age the candidate past the threshold so it becomes the sole eligible
	//    candidate and the rotation starts.
	time.Sleep(ageThreshold - 30*time.Second)

	// 3. The surge INDUCES a brand-new NodeClaim: a claim whose name was NOT in the
	//    pre-rotation set appears in pool-a (this is the §3.3 new-provision path the
	//    soft-preference fix unblocks — issue #96), and the placeholder binds to its
	//    Node. A required-hostname placeholder would have stayed Pending here.
	var surgeClaim, surgeNode string
	eventually(t, surgeBindBudget, "Karpenter to provision a NEW NodeClaim for the placeholder", func() error {
		cur, err := listClaims(ctx, cl, poolA)
		if err != nil {
			return err
		}
		for i := range cur {
			if !beforeNames[cur[i].Name] && cur[i].DeletionTimestamp == nil {
				surgeClaim = cur[i].Name
				surgeNode = cur[i].Status.NodeName
			}
		}
		if surgeClaim == "" {
			return fmt.Errorf("no new NodeClaim provisioned yet (set still %v) — placeholder not inducing capacity", keys(claimNames(cur)))
		}
		if surgeNode == "" {
			return fmt.Errorf("new NodeClaim %s not registered a Node yet", surgeClaim)
		}
		return nil
	})
	t.Logf("Karpenter provisioned NEW surge NodeClaim %s (node %s) for the placeholder — new-provision path works (issue #96)", surgeClaim, surgeNode)

	// 4. The placeholder bound to the freshly provisioned surge node (not the
	//    candidate), and that node is in pool-a — confirming the new node is the
	//    surge target, not an absorb onto pre-existing capacity.
	eventually(t, 2*time.Minute, "the placeholder to bind to the freshly provisioned surge node", func() error {
		ph, err := getPlaceholder(ctx, cl, candClaim)
		if err != nil {
			return err
		}
		if ph == nil || ph.Spec.NodeName == "" {
			return fmt.Errorf("placeholder not bound yet")
		}
		if ph.Spec.NodeName != surgeNode {
			return fmt.Errorf("placeholder bound to %s, not the freshly provisioned surge node %s", ph.Spec.NodeName, surgeNode)
		}
		return nil
	})

	// 5. The rotation reaches complete: last-rotation stamped, anchor cleared, the
	//    candidate NodeClaim drained away, and the surge NodeClaim survives (it is
	//    the new node that replaced the candidate).
	eventually(t, surgeCompleteBudget, "the new-provision rotation to reach complete", func() error {
		pool, err := getNodePool(ctx, cl, poolA)
		if err != nil {
			return err
		}
		if poolAnno(pool, annotations.LastRotationAt) == "" {
			return fmt.Errorf("last-rotation-at not yet stamped (anchor=%q)", poolAnno(pool, annotations.ActiveRotation))
		}
		if poolAnno(pool, annotations.ActiveRotation) != "" {
			return fmt.Errorf("anchor still held: %q", poolAnno(pool, annotations.ActiveRotation))
		}
		if _, err := getClaim(ctx, cl, candClaim); !apierrors.IsNotFound(err) {
			return fmt.Errorf("candidate claim %s not yet gone (err=%v)", candClaim, err)
		}
		return nil
	})
	if _, err := getClaim(ctx, cl, surgeClaim); err != nil {
		t.Fatalf("freshly provisioned surge claim %s vanished — it is the candidate's replacement and must survive: %v", surgeClaim, err)
	}

	// 6. Completion side effects: placeholder deleted, surge target unfrozen, and
	//    a success + drain-duration metric recorded — same completion chain the
	//    absorb path asserts, now reached via a genuinely new node.
	assertPlaceholderGone(ctx, t, cl, candClaim)
	assertNoLingeringFreeze(ctx, t, cl, candClaim)
	assertSuccessAndDrainMetrics(ctx, t, poolA)
}

// ─────────────────────────────────────────────────────────────────────────────
// Capacity-absorb path (§3.3 second acceptable outcome) + completion + metrics.
//
// Covers issue #92 criteria:
//   - Capacity-absorb: a pre-existing spare node in the candidate's pool absorbs
//     the placeholder; the rotation reaches `complete` WITHOUT a new NodeClaim.
//   - Completion chain: placeholder deleted, surge target unfrozen, anchor cleared.
//   - Metrics: success + drain-duration scraped from /metrics (metrics_test.go).
//
// This subtest specifically exercises the ABSORB outcome (the new-provision path
// has its own subtest, testNewProvision). When a pre-existing spare exists in the
// candidate's pool, kube-scheduler bin-packs the placeholder onto it — honoring
// the (now soft, §3.3 / issue #96) hostname preference — rather than inducing a
// new node. We assert the placeholder binds to the pre-existing spare and the
// rotation completes WITHOUT a new NodeClaim appearing.
//
// Determinism: with ageThreshold=4m, the candidate is provisioned and aged ~3.5m
// (still below threshold), then a FRESH spare is provisioned in the same pool.
// The candidate crosses the threshold first and becomes the sole eligible
// candidate; the spare stays below threshold (so it is NOT in the placeholder's
// near-deadline hostname-exclusion set, §3.3) for the whole rotation, which
// completes in well under the remaining ~3.5m window.
func testCapacityAbsorb(ctx context.Context, t *testing.T, cl client.Client) {
	defer resetCluster(ctx, t, cl)
	// Keep the controller reconciling promptly through the static-KWOK quiet
	// periods so an aged candidate is picked within the window (see startNudger).
	defer startNudger(ctx, cl, poolA, poolB)()

	const ageThreshold = 4 * time.Minute

	// 1. Provision the candidate and let it age below the threshold.
	applyDeployment(ctx, t, cl, deployment("cand", poolA, 300, ""))
	candClaim := waitClaimProvisioned(ctx, t, cl, poolA)

	// A blocking PDB on the candidate workload holds the drain once the rotation
	// deletes the old NodeClaim, freezing the in-flight pool state so the
	// capacity-absorb proof below (no new NodeClaim induced) is read off a stable
	// snapshot instead of racing KWOK's fast drain. Loosened in step 3b so the
	// rotation can finish; resetCluster also removes it on teardown.
	pdb := blockingPDB("absorb-hold", map[string]string{"app": "cand"})
	if err := cl.Create(ctx, pdb); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create absorb-hold PDB: %v", err)
	}

	t.Logf("candidate NodeClaim %s provisioned; aging it ~%s (just under ageThreshold)", candClaim, ageThreshold-30*time.Second)
	time.Sleep(ageThreshold - 30*time.Second)

	// 2. Provision a FRESH spare in the same pool (distinct node via anti-affinity)
	//    with room to absorb the candidate's reschedulable requests (300m).
	applyDeployment(ctx, t, cl, deployment("spare", poolA, 300, "cand"))
	eventually(t, 90*time.Second, "the spare NodeClaim to register a Node", func() error {
		claims, err := listClaims(ctx, cl, poolA)
		if err != nil {
			return err
		}
		if len(claims) < 2 {
			return fmt.Errorf("have %d claims, want 2", len(claims))
		}
		for i := range claims {
			if claims[i].Name != candClaim && claims[i].Status.NodeName == "" {
				return fmt.Errorf("spare claim %s not registered yet", claims[i].Name)
			}
		}
		return nil
	})
	before, err := listClaims(ctx, cl, poolA)
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := claimNames(before)
	// Identify the pre-existing spare node (the non-candidate registered claim).
	spareNode := ""
	for i := range before {
		if before[i].Name != candClaim {
			spareNode = before[i].Status.NodeName
		}
	}
	if spareNode == "" {
		t.Fatalf("could not resolve the pre-existing spare node from %v", keys(beforeNames))
	}
	t.Logf("two NodeClaims present %v; pre-existing spare node=%s; candidate %s will cross ageThreshold next",
		keys(beforeNames), spareNode, candClaim)

	// 3. Capture the SURGE TARGET in-flight and assert the placeholder ABSORBED
	//    onto the pre-existing spare node — the §3.3 capacity-absorb path: the
	//    surge target is a node that already existed before the rotation started,
	//    NOT a freshly provisioned surge node.
	eventually(t, surgeBindBudget, "the placeholder to bind to the pre-existing spare (absorb)", func() error {
		ph, err := getPlaceholder(ctx, cl, candClaim)
		if err != nil {
			return err
		}
		if ph == nil || ph.Spec.NodeName == "" {
			return fmt.Errorf("placeholder not bound yet")
		}
		if ph.Spec.NodeName != spareNode {
			return fmt.Errorf("placeholder bound to %s, not the pre-existing spare %s", ph.Spec.NodeName, spareNode)
		}
		return nil
	})
	t.Logf("placeholder absorbed onto pre-existing spare node %s (no new surge NodeClaim)", spareNode)

	// 3a. The rotation has reached the drain (the old NodeClaim is being deleted),
	//     but the PDB above holds it — so the in-flight claim set is now stable.
	eventually(t, surgeBindBudget, "the candidate to enter draining (drain held by PDB)", func() error {
		c, err := getClaim(ctx, cl, candClaim)
		if err != nil {
			return fmt.Errorf("candidate %s: %v", candClaim, err)
		}
		if c == nil {
			return fmt.Errorf("candidate %s gone before the drain could be observed held", candClaim)
		}
		if c.DeletionTimestamp == nil {
			return fmt.Errorf("candidate not draining yet (state=%q)", claimAnno(c, annotations.State))
		}
		return nil
	})

	// 3b. §3.3 second-path proof (reviewer): the surge reserved EXISTING capacity,
	//     so it induced NO new NodeClaim. With the drain HELD, the pool's claim set
	//     stays EXACTLY the two pre-existing claims for a sustained window. This is a
	//     by-NAME set-equality check, not a count: a fast drain that deleted the
	//     candidate and provisioned a replacement would keep the count equal while
	//     the set changed, so a count check could false-pass. (A new NodeClaim for
	//     the displaced workload AFTER the drain finishes is legitimate and is not a
	//     surge node — hence the held window, before the drain completes.)
	consistently(t, 20*time.Second, "the surge induces no new NodeClaim (claim set stays exactly the pre-existing two)", func() error {
		cur, err := listClaims(ctx, cl, poolA)
		if err != nil {
			return err
		}
		curNames := claimNames(cur)
		for name := range curNames {
			if !beforeNames[name] {
				return fmt.Errorf("new NodeClaim %q appeared during the surge (set now %v, was %v): the placeholder provisioned new capacity instead of absorbing existing capacity",
					name, keys(curNames), keys(beforeNames))
			}
		}
		for name := range beforeNames {
			if !curNames[name] {
				return fmt.Errorf("pre-existing NodeClaim %q vanished mid-surge (set now %v, was %v): the drain was not held as expected",
					name, keys(curNames), keys(beforeNames))
			}
		}
		return nil
	})
	t.Logf("in-flight claim set held exactly at %v — capacity-absorb induced no new NodeClaim", keys(beforeNames))

	// 3c. Loosen the PDB so the held drain proceeds and the rotation can complete.
	loosenPDB(ctx, t, cl, pdb)

	// 4. The rotation reaches complete via the drain path: last-rotation stamped,
	//    anchor cleared, candidate NodeClaim drained away.
	eventually(t, surgeCompleteBudget, "the candidate rotation to reach complete", func() error {
		pool, err := getNodePool(ctx, cl, poolA)
		if err != nil {
			return err
		}
		if poolAnno(pool, annotations.LastRotationAt) == "" {
			return fmt.Errorf("last-rotation-at not yet stamped (anchor=%q)", poolAnno(pool, annotations.ActiveRotation))
		}
		if poolAnno(pool, annotations.ActiveRotation) != "" {
			return fmt.Errorf("anchor still held: %q", poolAnno(pool, annotations.ActiveRotation))
		}
		if _, err := getClaim(ctx, cl, candClaim); !apierrors.IsNotFound(err) {
			return fmt.Errorf("candidate claim %s not yet gone (err=%v)", candClaim, err)
		}
		return nil
	})

	// 5. The pre-existing spare's NodeClaim is still present — the absorb consumed
	//    no new surge capacity for the reservation itself (the surge target was
	//    pre-existing). The candidate is gone.
	spareClaim := ""
	for i := range before {
		if before[i].Status.NodeName == spareNode {
			spareClaim = before[i].Name
		}
	}
	if _, err := getClaim(ctx, cl, spareClaim); err != nil {
		t.Fatalf("pre-existing spare claim %s vanished — it should survive the absorb: %v", spareClaim, err)
	}

	// 5. Completion side effects: the placeholder Pod is deleted, and the surge
	//    target (the spare's node) is unfrozen — no controller-owned do-not-disrupt
	//    / surge-for / cordon markers linger.
	assertPlaceholderGone(ctx, t, cl, candClaim)
	assertNoLingeringFreeze(ctx, t, cl, candClaim)

	// 6. Metrics: success counter incremented and a drain-phase duration observed.
	assertSuccessAndDrainMetrics(ctx, t, poolA)
}

// ─────────────────────────────────────────────────────────────────────────────
// Multi-NodePool confinement (§3.3): the placeholder's required
// karpenter.sh/nodepool selector confines both the kube-scheduler binding and
// any Karpenter provisioning to the candidate's pool — even when ANOTHER pool
// has spare capacity. Covers the issue #92 "other pool has spare but is not
// absorbed" case.
//
// Setup mirrors the absorb subtest (so a real rotation runs), but additionally
// stages spare capacity in the OUT-OF-SCOPE pool-b. We assert the surge target
// (the node the placeholder binds to) carries pool-a's nodepool label, the
// placeholder itself carries the required pool-a selector, and pool-b's node is
// never touched (its claim count is unchanged, no surge-for marker lands on it).
func testConfinement(ctx context.Context, t *testing.T, cl client.Client) {
	defer resetCluster(ctx, t, cl)
	// Keep the controller reconciling promptly through the static-KWOK quiet
	// periods so an aged candidate is picked within the window (see startNudger).
	defer startNudger(ctx, cl, poolA, poolB)()

	const ageThreshold = 4 * time.Minute

	// Stage spare capacity in pool-b up front (it ages freely; it must never be
	// absorbed regardless of age because the placeholder's nodepool selector
	// excludes it structurally).
	applyDeployment(ctx, t, cl, deployment("poolb", poolB, 300, ""))
	bClaim := waitClaimProvisioned(ctx, t, cl, poolB)
	t.Logf("pool-b spare NodeClaim %s staged (must never absorb pool-a's placeholder)", bClaim)

	// Candidate in pool-a, aged, then a pool-a spare to absorb onto.
	applyDeployment(ctx, t, cl, deployment("cand", poolA, 300, ""))
	candClaim := waitClaimProvisioned(ctx, t, cl, poolA)
	time.Sleep(ageThreshold - 30*time.Second)
	applyDeployment(ctx, t, cl, deployment("spare", poolA, 300, "cand"))
	waitSpareRegistered(ctx, t, cl, candClaim)

	// While the placeholder is in flight, capture its required nodepool selector
	// and its bound host; assert both confine to pool-a.
	var surgeHost string
	eventually(t, surgeBindBudget, "the placeholder to bind within pool-a", func() error {
		ph, err := getPlaceholder(ctx, cl, candClaim)
		if err != nil || ph == nil {
			// Placeholder may already be deleted post-completion; fall back to the
			// surge-for marked node below.
			return fmt.Errorf("placeholder not observed yet")
		}
		if !placeholderSelectsPool(ph, poolA) {
			return fmt.Errorf("placeholder missing required %s In [%s] selector", karpv1.NodePoolLabelKey, poolA)
		}
		if ph.Spec.NodeName == "" {
			return fmt.Errorf("placeholder not bound yet")
		}
		surgeHost = ph.Spec.NodeName
		return nil
	})

	// The surge host is a pool-a node.
	host, err := getNode(ctx, cl, surgeHost)
	if err != nil {
		t.Fatalf("get surge host %s: %v", surgeHost, err)
	}
	if got := host.Labels[karpv1.NodePoolLabelKey]; got != poolA {
		t.Fatalf("surge host %s is in pool %q, want %q (confinement violated)", surgeHost, got, poolA)
	}

	// Drive the rotation to completion (absorb) and assert pool-b was untouched
	// the whole time: its claim set is unchanged and its node never gained a
	// surge-for marker.
	pbClaimsBefore, _ := listClaims(ctx, cl, poolB)
	eventually(t, surgeCompleteBudget, "rotation complete via pool-a absorb", func() error {
		pool, err := getNodePool(ctx, cl, poolA)
		if err != nil {
			return err
		}
		if poolAnno(pool, annotations.LastRotationAt) == "" || poolAnno(pool, annotations.ActiveRotation) != "" {
			return fmt.Errorf("rotation not complete yet")
		}
		return nil
	})

	pbClaimsAfter, _ := listClaims(ctx, cl, poolB)
	if len(pbClaimsAfter) != len(pbClaimsBefore) {
		t.Fatalf("pool-b claim count changed %d→%d: the placeholder leaked into the out-of-scope pool",
			len(pbClaimsBefore), len(pbClaimsAfter))
	}
	bNodes, _ := poolNodes(ctx, cl, poolB)
	for i := range bNodes {
		if _, ok := bNodes[i].Annotations[annotations.SurgeFor]; ok {
			t.Fatalf("pool-b node %s carries a %s marker: it was wrongly enrolled in pool-a's rotation",
				bNodes[i].Name, annotations.SurgeFor)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Voluntary drain honors PDB (§3.3, §3.5 voluntary path). A blocking PDB
// (maxUnavailable: 0) holds the old node's Pod Terminating / the drain stuck;
// loosening it lets the drain finish. Covers issue #92 PDB criterion.
//
// We use the absorb path to actually delete the old NodeClaim (which triggers
// Karpenter's termination controller, the voluntary eviction path where PDBs
// apply). A blocking PDB on the candidate's workload must hold the drain; after
// the PDB is loosened, the drain completes and the rotation reaches `complete`.
func testPDB(ctx context.Context, t *testing.T, cl client.Client) {
	defer resetCluster(ctx, t, cl)
	// Keep the controller reconciling promptly through the static-KWOK quiet
	// periods so an aged candidate is picked within the window (see startNudger).
	defer startNudger(ctx, cl, poolA, poolB)()

	const ageThreshold = 4 * time.Minute

	// Candidate workload carries label app=cand; a blocking PDB selects it.
	applyDeployment(ctx, t, cl, deployment("cand", poolA, 300, ""))
	candClaim := waitClaimProvisioned(ctx, t, cl, poolA)

	pdb := blockingPDB("cand-block", map[string]string{"app": "cand"})
	if err := cl.Create(ctx, pdb); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create blocking PDB: %v", err)
	}
	defer func() { _ = cl.Delete(ctx, pdb) }()

	time.Sleep(ageThreshold - 30*time.Second)
	applyDeployment(ctx, t, cl, deployment("spare", poolA, 300, "cand"))
	waitSpareRegistered(ctx, t, cl, candClaim)

	// The rotation should reach draining (candidate NodeClaim deletionTimestamp set)
	// but NOT complete: the blocking PDB holds the candidate Pod's eviction.
	eventually(t, surgeBindBudget, "the candidate to enter draining (deletionTimestamp set)", func() error {
		c, err := getClaim(ctx, cl, candClaim)
		if err != nil {
			return fmt.Errorf("candidate %s: %v", candClaim, err)
		}
		if c.DeletionTimestamp == nil {
			return fmt.Errorf("candidate not deleting yet (state=%q)", claimAnno(c, annotations.State))
		}
		return nil
	})

	// Prove the drain is HELD: for a sustained window the candidate stays present
	// (its termination is blocked by the PDB) and its workload Pod stays.
	t.Log("asserting the blocking PDB holds the drain (candidate stays terminating)")
	consistently(t, 30*time.Second, "blocked drain keeps the candidate NodeClaim alive", func() error {
		if _, err := getClaim(ctx, cl, candClaim); apierrors.IsNotFound(err) {
			return fmt.Errorf("candidate %s drained while PDB still blocking — PDB NOT honored", candClaim)
		}
		return nil
	})

	// Loosen the PDB (allow disruptions) → the drain proceeds to completion.
	t.Log("loosening the PDB; the drain should now finish")
	loosenPDB(ctx, t, cl, pdb)
	eventually(t, 120*time.Second, "the drain to finish after loosening the PDB", func() error {
		if _, err := getClaim(ctx, cl, candClaim); apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("candidate %s still present after PDB loosened", candClaim)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// do-not-disrupt on the surge pair (§3.3 guard). KWOK form achieved: we ASSERT
// karpenter.sh/do-not-disrupt is present on BOTH the old (candidate) node and
// the surge target during the surge, AND that the controller owns it (its
// noderotation.io/do-not-disrupt-owned marker). We do NOT claim Karpenter
// honored it against voluntary disruption: the NodePools run consolidationPolicy
// WhenEmpty with a very long consolidateAfter (nodepools.yaml), so no voluntary
// Consolidation/Drift is induced under KWOK — the stronger "no disruption while
// the annotation is set" claim is deferred to EKS (#93). This matches the
// issue #92 "assert only that the annotation is set" branch.
func testDoNotDisrupt(ctx context.Context, t *testing.T, cl client.Client) {
	defer resetCluster(ctx, t, cl)
	// Keep the controller reconciling promptly through the static-KWOK quiet
	// periods so an aged candidate is picked within the window (see startNudger).
	defer startNudger(ctx, cl, poolA, poolB)()

	const ageThreshold = 4 * time.Minute

	applyDeployment(ctx, t, cl, deployment("cand", poolA, 300, ""))
	candClaim := waitClaimProvisioned(ctx, t, cl, poolA)
	candClaimObj, _ := getClaim(ctx, cl, candClaim)
	candNode := candClaimObj.Status.NodeName

	// A blocking PDB on the candidate workload holds the drain once the rotation
	// deletes the old NodeClaim, so the surge pair stays frozen for a deterministic
	// window instead of the few seconds KWOK's fast drain would otherwise leave —
	// both nodes (candidate + surge target) keep their do-not-disrupt while we
	// observe. resetCluster removes the PDB (and the placeholder) on teardown.
	pdb := blockingPDB("dnd-block", map[string]string{"app": "cand"})
	if err := cl.Create(ctx, pdb); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create do-not-disrupt hold PDB: %v", err)
	}

	time.Sleep(ageThreshold - 30*time.Second)
	applyDeployment(ctx, t, cl, deployment("spare", poolA, 300, "cand"))
	waitSpareRegistered(ctx, t, cl, candClaim)

	// During the surge — after the surge target is frozen and the old NodeClaim is
	// deleted, with the drain held by the PDB above — BOTH the candidate node and
	// the surge target carry do-not-disrupt with the controller-owned marker.
	var surgeNode string
	eventually(t, 8*time.Minute, "both surge-pair nodes to carry controller-owned do-not-disrupt", func() error {
		// surge target = the node carrying this rotation's surge-for marker that
		// is not the candidate node.
		nodes, err := poolNodes(ctx, cl, poolA)
		if err != nil {
			return err
		}
		for i := range nodes {
			n := &nodes[i]
			if n.Annotations[annotations.SurgeFor] == candClaim && n.Name != candNode {
				surgeNode = n.Name
			}
		}
		if surgeNode == "" {
			return fmt.Errorf("surge target not frozen yet")
		}
		cn, err := getNode(ctx, cl, candNode)
		if err != nil {
			return fmt.Errorf("candidate node gone already (rotation completed too fast to observe): %v", err)
		}
		sn, err := getNode(ctx, cl, surgeNode)
		if err != nil {
			return err
		}
		if !controllerOwnedDoNotDisrupt(cn) {
			return fmt.Errorf("candidate node %s missing controller-owned do-not-disrupt", candNode)
		}
		if !controllerOwnedDoNotDisrupt(sn) {
			return fmt.Errorf("surge node %s missing controller-owned do-not-disrupt", surgeNode)
		}
		return nil
	})
	t.Logf("do-not-disrupt present+owned on candidate node %s and surge target %s "+
		"(KWOK form: annotation set; not asserting Karpenter honored it — see #93)", candNode, surgeNode)
}

// ─────────────────────────────────────────────────────────────────────────────
// Placeholder preemption victim (§3.3, §5.2). The negative-priority placeholder
// (preemptionPolicy: Never) is the preemption victim when a higher-priority
// workload needs its space; it never preempts system Pods; and once preempted it
// is deleted and does not re-pend. Covers issue #92 bare-placeholder cleanup.
//
// We drive a REAL preemption: the rotation is parked in its drain phase (the old
// NodeClaim deleted but its drain held by a PDB), where the controller's
// advanceDraining does NOT recreate the placeholder — so a competing workload can
// evict the bound placeholder and we can prove it stays evicted. A normal
// (priority 0) workload pinned to the surge host, requesting more than the sliver
// of room left beside the placeholder, forces kube-scheduler to preempt the
// placeholder (the only pod there below priority 0). We assert: the placeholder
// is the victim, the spare's own workload is NOT evicted in its place, and the
// placeholder does not re-pend.
func testPreemption(ctx context.Context, t *testing.T, cl client.Client) {
	defer resetCluster(ctx, t, cl)
	defer func() {
		_ = cl.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "preemptor", Namespace: workloadNamespace}})
	}()
	// Keep the controller reconciling promptly through the static-KWOK quiet
	// periods so an aged candidate is picked within the window (see startNudger).
	defer startNudger(ctx, cl, poolA, poolB)()

	const ageThreshold = 4 * time.Minute

	applyDeployment(ctx, t, cl, deployment("cand", poolA, 300, ""))
	candClaim := waitClaimProvisioned(ctx, t, cl, poolA)

	// Hold the candidate drain so the rotation parks in `draining` with the
	// placeholder still bound, instead of completing (which would delete it).
	pdb := blockingPDB("preempt-hold", map[string]string{"app": "cand"})
	if err := cl.Create(ctx, pdb); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create preempt-hold PDB: %v", err)
	}

	time.Sleep(ageThreshold - 30*time.Second)
	// Spare sized so the surge node has room for the placeholder but only a sliver
	// beyond it (e2e-small ~1900m allocatable: spare 1500m + placeholder 300m =
	// 1800m), so a 300m competitor pinned to it cannot fit without evicting the
	// placeholder.
	applyDeployment(ctx, t, cl, deployment("spare", poolA, 1500, "cand"))
	waitSpareRegistered(ctx, t, cl, candClaim)

	// Observe the placeholder bind onto the spare; capture its host + the structural
	// victim guarantees (negative priority + PreemptionPolicy=Never, so it can only
	// ever be a victim, never a preemptor).
	var surgeHost string
	eventually(t, 8*time.Minute, "placeholder to bind to the spare", func() error {
		ph, err := getPlaceholder(ctx, cl, candClaim)
		if err != nil || ph == nil || ph.Spec.NodeName == "" {
			return fmt.Errorf("placeholder not bound yet")
		}
		if ph.Spec.PreemptionPolicy == nil || *ph.Spec.PreemptionPolicy != corev1.PreemptNever {
			return fmt.Errorf("placeholder is not PreemptionPolicy=Never (could preempt other Pods!)")
		}
		if ph.Spec.Priority == nil || *ph.Spec.Priority >= 0 {
			return fmt.Errorf("placeholder priority %v is not negative", ph.Spec.Priority)
		}
		surgeHost = ph.Spec.NodeName
		return nil
	})
	t.Logf("placeholder bound to %s (negative priority, PreemptionPolicy=Never)", surgeHost)

	// Wait until the rotation is in `draining` (old NodeClaim deleted, drain held by
	// the PDB). In this phase advanceDraining leaves the placeholder alone — it is
	// NOT recreated — so a preemption here is permanent, which is what lets us prove
	// "does not re-pend" deterministically (mid-PENDING the controller would recreate
	// it by design).
	eventually(t, 3*time.Minute, "the rotation to reach draining (drain held by PDB)", func() error {
		c, err := getClaim(ctx, cl, candClaim)
		if err != nil {
			return fmt.Errorf("candidate %s: %v", candClaim, err)
		}
		if c == nil {
			return fmt.Errorf("candidate %s gone before draining could be observed", candClaim)
		}
		if c.DeletionTimestamp == nil {
			return fmt.Errorf("candidate not draining yet (state=%q)", claimAnno(c, annotations.State))
		}
		return nil
	})

	// Resolve the surge host's hostname label so the competitor pins to it via the
	// scheduler (hostname nodeSelector, not nodeName — nodeName bypasses scheduling
	// and never triggers preemption).
	host, err := getNode(ctx, cl, surgeHost)
	if err != nil {
		t.Fatalf("get surge host %s: %v", surgeHost, err)
	}
	spareHostname := host.Labels[corev1.LabelHostname]
	if spareHostname == "" {
		spareHostname = host.Name
	}

	// A normal (priority 0) workload pinned to the surge host, requesting more than
	// the sliver beside the placeholder, so the scheduler must preempt to fit it.
	// priority 0 > the placeholder's negative priority, so the placeholder is the
	// only eligible victim.
	if err := cl.Create(ctx, preemptorPod("preemptor", spareHostname, 300)); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create preemptor: %v", err)
	}

	// The placeholder is the VICTIM: the competitor lands on the surge host (which
	// required evicting the placeholder, the only sub-zero-priority pod there) while
	// the spare's own workload (priority 0) is NOT evicted — so the placeholder,
	// never real workload, is what gets preempted.
	eventually(t, 3*time.Minute, "the competitor to preempt the bare placeholder", func() error {
		pre, err := podByApp(ctx, cl, "preemptor")
		if err != nil {
			return err
		}
		if pre == nil {
			return fmt.Errorf("competitor pod not observed yet")
		}
		if pre.Spec.NodeName != surgeHost || pre.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("competitor not running on surge host %s yet (node=%q phase=%q)",
				surgeHost, pre.Spec.NodeName, pre.Status.Phase)
		}
		sp, err := podByApp(ctx, cl, "spare")
		if err != nil {
			return err
		}
		if sp == nil || sp.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("the spare workload pod must stay Running — it must NOT be the preemption victim")
		}
		return nil
	})
	t.Logf("competitor preempted the bare placeholder on %s; the spare workload survived (placeholder is the victim, never real workload)", surgeHost)

	// And it does NOT re-pend: in `draining` the controller does not recreate the
	// placeholder, and the placeholder (PreemptionPolicy=Never) never preempts the
	// competitor back — so for a sustained window there is no placeholder Pod and the
	// competitor stays scheduled.
	consistently(t, 20*time.Second, "the preempted bare placeholder does not re-pend", func() error {
		ph, err := getPlaceholder(ctx, cl, candClaim)
		if err != nil {
			return err
		}
		if ph != nil {
			return fmt.Errorf("a placeholder Pod %s re-appeared after preemption (phase=%q) — it must not re-pend in the drain phase", ph.Name, ph.Status.Phase)
		}
		pre, err := podByApp(ctx, cl, "preemptor")
		if err != nil {
			return err
		}
		if pre == nil || pre.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("competitor no longer Running — the placeholder must never become a preemptor")
		}
		return nil
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Governance loss reaps an in-flight rotation (§5.4, issue #141). When a NodePool
// leaves RotationPolicy governance WHILE a rotation is anchored, the controller
// stops advancing it (governingPolicy yields NoMatch) — so it must reap the
// artifacts it owns (placeholder, freeze markers, anchor) rather than orphan them.
// A left-behind karpenter.sh/do-not-disrupt marker would otherwise silently block
// Karpenter's voluntary Consolidation/Drift on that node indefinitely, with no
// future reconcile to clear it (the startup sweep treats an anchored claim as live).
//
// Determinism: drive a normal absorb rotation and park it in `draining` with a
// blocking PDB (the same hold testDoNotDisrupt/testPreemption use), so the surge
// pair is stably frozen and the anchor is set. THEN drop nodepool-a's in-scope
// label so the governing RotationPolicy no longer selects it (NoMatch). The next
// reconcile must reap: anchor cleared, placeholder deleted, and both surge-pair
// nodes unfrozen. The in-scope label is restored on teardown so later runs (and a
// re-run of the suite against the shared cluster) still govern pool-a.
func testGovernanceLossReap(ctx context.Context, t *testing.T, cl client.Client) {
	defer resetCluster(ctx, t, cl)
	// Re-govern pool-a before resetCluster runs (defers are LIFO) and before the
	// next suite run, so dropping the label here does not leak past this subtest.
	defer restoreInScope(ctx, t, cl)
	// Keep the controller reconciling promptly through the static-KWOK quiet
	// periods so the aged candidate is picked AND the post-label-drop reap fires
	// without waiting on the 30s self-requeue (see startNudger).
	defer startNudger(ctx, cl, poolA, poolB)()

	const ageThreshold = 4 * time.Minute

	applyDeployment(ctx, t, cl, deployment("cand", poolA, 300, ""))
	candClaim := waitClaimProvisioned(ctx, t, cl, poolA)
	candClaimObj, _ := getClaim(ctx, cl, candClaim)
	candNode := candClaimObj.Status.NodeName

	// Hold the candidate drain so the rotation parks in `draining` with the surge
	// pair frozen and the anchor set — a stable in-flight snapshot to drop
	// governance against. resetCluster removes the PDB on teardown.
	pdb := blockingPDB("govloss-hold", map[string]string{"app": "cand"})
	if err := cl.Create(ctx, pdb); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create govloss-hold PDB: %v", err)
	}

	time.Sleep(ageThreshold - 30*time.Second)
	applyDeployment(ctx, t, cl, deployment("spare", poolA, 300, "cand"))
	waitSpareRegistered(ctx, t, cl, candClaim)

	// Confirm the rotation is in-flight: anchor set, the surge target frozen, and
	// the candidate node carrying the controller-owned do-not-disrupt — the marker
	// that would leak if governance were lost without a reap.
	var surgeNode string
	eventually(t, surgeBindBudget, "the rotation to be in-flight with the surge pair frozen", func() error {
		pool, err := getNodePool(ctx, cl, poolA)
		if err != nil {
			return err
		}
		if poolAnno(pool, annotations.ActiveRotation) == "" {
			return fmt.Errorf("no active-rotation anchor yet")
		}
		nodes, err := poolNodes(ctx, cl, poolA)
		if err != nil {
			return err
		}
		surgeNode = ""
		for i := range nodes {
			if nodes[i].Annotations[annotations.SurgeFor] == candClaim && nodes[i].Name != candNode {
				surgeNode = nodes[i].Name
			}
		}
		if surgeNode == "" {
			return fmt.Errorf("surge target not frozen yet")
		}
		cn, err := getNode(ctx, cl, candNode)
		if err != nil {
			return fmt.Errorf("candidate node gone (rotation advanced too fast to observe in-flight): %v", err)
		}
		if !controllerOwnedDoNotDisrupt(cn) {
			return fmt.Errorf("candidate node %s not yet carrying controller-owned do-not-disrupt", candNode)
		}
		return nil
	})
	ph, err := getPlaceholder(ctx, cl, candClaim)
	if err != nil || ph == nil {
		t.Fatalf("expected a placeholder Pod for the in-flight rotation (err=%v)", err)
	}
	t.Logf("rotation in-flight: anchor set, placeholder %s present, candidate node %s and surge target %s frozen",
		ph.Name, candNode, surgeNode)

	// Take pool-a OUT of governance mid-rotation: drop the in-scope label the
	// RotationPolicy selects on, so the governing policy now NoMatches the pool.
	dropInScope(ctx, t, cl)
	t.Logf("dropped %s label from %s — pool is now ungoverned mid-rotation", inScopeLabelKey, poolA)

	// The controller must REAP rather than orphan: the anchor is cleared, the
	// placeholder deleted, and every surge-pair node unfrozen (no surge-for and no
	// controller-owned do-not-disrupt left behind — issue #141).
	eventually(t, surgeBindBudget, "the controller to clear the anchor after losing governance", func() error {
		pool, err := getNodePool(ctx, cl, poolA)
		if err != nil {
			return err
		}
		if a := poolAnno(pool, annotations.ActiveRotation); a != "" {
			return fmt.Errorf("anchor still held %q — pool ungoverned but rotation not reaped", a)
		}
		return nil
	})
	assertPlaceholderGone(ctx, t, cl, candClaim)
	assertNoLingeringFreeze(ctx, t, cl, candClaim)
	t.Logf("governance-loss reap complete: anchor cleared, placeholder gone, surge pair unfrozen — no orphaned do-not-disrupt")
}

// ─────────────────────────────────────────────────────────────────────────────
// Forceful-fallback surge-less rotation (spec §3.3, ADR-0001; issue #156 D4/D5).
//
// Exercises the opt-in window-bounded surge-less forceful fallback end-to-end on
// nodepool-c (governed by a dedicated RotationPolicy with
// surge.forcefulFallback.enabled=true). Surge-less is reached only for a candidate
// that cannot complete a graceful surge before its OWN deadline while the pool
// schedule is otherwise feasible (§3.2 layer 3 / §3.5); a globally-infeasible
// schedule (E < t_rot) is a fatal config error the controller refuses to start
// (§3.2 layer 1), so a single short-expireAfter pool would just be blocked — never
// reaching the surge-less pick.
//
// To create the layer-3 short-lead case deterministically: pool-c starts with a
// short template expireAfter (20m), so Karpenter stamps the candidate NodeClaim
// with E=20m; the test then raises the template to a feasible value (8760h). The
// existing claim keeps its 20m stamp (NodeClaim.spec is immutable), so it becomes
// a short-lead candidate whose deadline is within t_rot (≈62m — tGP unset →
// drainBound = DrainFallback 1h). Once it ages past the 2m ageThreshold the
// controller rotates it SURGE-LESS: deletes the NodeClaim with no placeholder ever
// staged, drains via the voluntary path, and increments
// noderotation_forceful_fallback_total. This mirrors spec §3.2's "raising E heals
// new claims only; existing stamped ones are caught by the layer-3 per-node
// check". A Drifted-blocking disruption budget on pool-c keeps Karpenter from
// disrupting the (now template-drifted) node itself, so only the controller's
// surge-less deletion drives the rotation.
//
// This is the scheduled-candidate gate proof the D4 envtest's disabled case could
// only approximate (review Minor M1); the graceful pool-a subtests (which DO stage
// a placeholder) are the contrast. nodepool-c is isolated by the ff-scope selector
// so rotationpolicy-a never matches it.
func testForcefulFallback(ctx context.Context, t *testing.T, cl client.Client) {
	defer resetCluster(ctx, t, cl)
	// Keep the controller reconciling promptly through the static-KWOK quiet periods
	// so the aged candidate is picked within the (always-open) window.
	defer startNudger(ctx, cl, poolC)()

	const ageThreshold = 2 * time.Minute

	// 1. A single candidate on pool-c. pool-c starts with a short 20m template
	//    expireAfter, so Karpenter stamps this claim with E=20m.
	applyDeployment(ctx, t, cl, deployment("ffcand", poolC, 300, ""))
	candClaim := waitClaimProvisioned(ctx, t, cl, poolC)
	t.Logf("candidate NodeClaim %s present on pool-c (stamped short E=20m)", candClaim)

	// 2. Raise pool-c's template expireAfter to a large feasible value (8760h),
	//    clearing the §3.2 layer-1 OverrideGBelowOne fatal so the pool starts
	//    rotations. The value must clear G >= 1 = floor(((E − t_rot) − A) / P):
	//    the window {00:00–23:59} leaves a 1-minute daily gap so it is NOT
	//    continuous and P = 24h (not the reconcile tick), hence E must exceed
	//    P + t_rot + A ≈ 25h — 8760h (matching pools a/b) clears it with wide
	//    margin. The existing claim keeps its 20m stamp (immutable spec), so it
	//    stays a §3.2 layer-3 short-lead candidate whose deadline is within t_rot →
	//    the pick resolves to surge-less (§3.3).
	raisePoolExpireAfter(ctx, t, cl, poolC, "8760h")

	// 3. Age past the threshold so the short-lead claim is the sole eligible candidate.
	time.Sleep(ageThreshold - 30*time.Second)

	// 4. No placeholder Pod is ever created while the candidate exists — the
	//    load-bearing surge-less signal is that a surge is NEVER staged. The sleep
	//    above ends ~30s before eligibility (age = ageThreshold); this 90s window
	//    therefore spans from just before eligibility to ~60s after it, so a
	//    would-be graceful placeholder (which a wrong impl stages right at
	//    eligibility and holds through readyTimeout) is caught even if the
	//    controller's first post-eligibility reconcile is delayed under CI load.
	//    The metric below is the durable proof; this rules out a graceful surge.
	consistently(t, 90*time.Second, "no placeholder Pod is staged for the surge-less candidate", func() error {
		ph, err := getPlaceholder(ctx, cl, candClaim)
		if err != nil {
			return err
		}
		if ph != nil {
			return fmt.Errorf("placeholder Pod %s exists — the rotation used a surge, not the surge-less path", ph.Name)
		}
		return nil
	})

	// 5. The forceful-fallback counter incremented for pool-c — proof (only
	//    startForcefulFallback sets it) that the surge-less path executed.
	assertForcefulFallbackMetric(ctx, t, poolC)

	// 6. The old candidate NodeClaim is deleted (drained away via the voluntary path).
	eventually(t, surgeCompleteBudget, "the candidate NodeClaim to be deleted surge-less", func() error {
		if _, err := getClaim(ctx, cl, candClaim); !apierrors.IsNotFound(err) {
			return fmt.Errorf("candidate %s not yet deleted (err=%v)", candClaim, err)
		}
		return nil
	})

	// 7. The rotation reaches complete: last-rotation stamped, anchor cleared, and
	//    the rotation-mode marker cleared (spec §5.3 cleanup on completion).
	eventually(t, surgeCompleteBudget, "the surge-less rotation to reach complete", func() error {
		pool, err := getNodePool(ctx, cl, poolC)
		if err != nil {
			return err
		}
		if poolAnno(pool, annotations.LastRotationAt) == "" {
			return fmt.Errorf("last-rotation-at not yet stamped (anchor=%q)", poolAnno(pool, annotations.ActiveRotation))
		}
		if poolAnno(pool, annotations.ActiveRotation) != "" {
			return fmt.Errorf("anchor still held: %q", poolAnno(pool, annotations.ActiveRotation))
		}
		if poolAnno(pool, annotations.RotationMode) != "" {
			return fmt.Errorf("rotation-mode marker not cleared on completion: %q", poolAnno(pool, annotations.RotationMode))
		}
		return nil
	})

	// 8. Completion side effects: success + drain-duration metrics (surge-less still
	//    counts a Success and observes the drain phase from DrainingAt).
	assertSuccessAndDrainMetrics(ctx, t, poolC)
}

// raisePoolExpireAfter patches the NodePool template's expireAfter to dur (an
// RFC "1h"/"2h" duration string) via a server-side merge patch — avoiding a
// read-modify-write race with the nudger, which patches the same NodePool's
// annotations. Raising it clears a §3.2 layer-1 fatal (an OverrideGBelowOne / A<=0
// template) so the pool starts rotations again, while any NodeClaim already
// stamped with a shorter expireAfter keeps it (spec is immutable) and is caught by
// the §3.2 layer-3 short-lead check — the setup for a surge-less pick.
func raisePoolExpireAfter(ctx context.Context, t *testing.T, cl client.Client, pool, dur string) {
	t.Helper()
	np := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: pool}}
	patch := client.RawPatch(types.MergePatchType,
		[]byte(fmt.Sprintf(`{"spec":{"template":{"spec":{"expireAfter":%q}}}}`, dur)))
	if err := cl.Patch(ctx, np, patch); err != nil {
		t.Fatalf("raise %s template expireAfter to %s: %v", pool, dur, err)
	}
	t.Logf("raised %s template expireAfter to %s (clears layer-1 fatal; existing claim stays short-lead)", pool, dur)
}

// inScopeLabelKey is the NodePool label the KWOK RotationPolicy (manifests/
// rotationpolicy.yaml) selects on; dropping it takes a pool out of governance.
const inScopeLabelKey = "noderotation-e2e/in-scope"

// dropInScope removes the in-scope label from nodepool-a via a merge patch (so it
// never races the nudger's annotation patches on resourceVersion), taking the pool
// out of the governing RotationPolicy's selector.
func dropInScope(ctx context.Context, t *testing.T, cl client.Client) {
	t.Helper()
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, inScopeLabelKey))
	if err := cl.Patch(ctx, &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: poolA}},
		client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatalf("drop in-scope label from %s: %v", poolA, err)
	}
}

// restoreInScope re-adds the in-scope label so pool-a is governed again, leaving
// the shared cluster as the manifests intend for any later subtest or suite re-run.
func restoreInScope(ctx context.Context, t *testing.T, cl client.Client) {
	t.Helper()
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"true"}}}`, inScopeLabelKey))
	if err := cl.Patch(ctx, &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: poolA}},
		client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Logf("warning: restore in-scope label on %s: %v", poolA, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared assertion + helper bodies.

func assertPlaceholderGone(ctx context.Context, t *testing.T, cl client.Client, candClaim string) {
	t.Helper()
	eventually(t, 60*time.Second, "placeholder Pod deletion", func() error {
		ph, err := getPlaceholder(ctx, cl, candClaim)
		if err != nil {
			return err
		}
		if ph != nil {
			return fmt.Errorf("placeholder %s still present", ph.Name)
		}
		return nil
	})
}

func assertNoLingeringFreeze(ctx context.Context, t *testing.T, cl client.Client, candClaim string) {
	t.Helper()
	eventually(t, 60*time.Second, "surge target to be unfrozen", func() error {
		nodes, err := poolNodes(ctx, cl, poolA)
		if err != nil {
			return err
		}
		for i := range nodes {
			n := &nodes[i]
			if n.Annotations[annotations.SurgeFor] == candClaim {
				return fmt.Errorf("node %s still carries %s=%s after completion", n.Name, annotations.SurgeFor, candClaim)
			}
			if _, owned := n.Annotations[annotations.DoNotDisruptOwned]; owned {
				return fmt.Errorf("node %s still carries controller-owned do-not-disrupt after completion", n.Name)
			}
		}
		return nil
	})
}

// controllerOwnedDoNotDisrupt reports whether the node carries
// karpenter.sh/do-not-disrupt=true AND the controller's ownership marker.
func controllerOwnedDoNotDisrupt(n *corev1.Node) bool {
	if n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		return false
	}
	_, owned := n.Annotations[annotations.DoNotDisruptOwned]
	return owned
}

func placeholderSelectsPool(ph *corev1.Pod, pool string) bool {
	aff := ph.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			if e.Key == karpv1.NodePoolLabelKey && e.Operator == corev1.NodeSelectorOpIn {
				for _, v := range e.Values {
					if v == pool {
						return true
					}
				}
			}
		}
	}
	return false
}

func blockingPDB(name string, sel map[string]string) *policyv1.PodDisruptionBudget {
	zero := intstr.FromInt32(0)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workloadNamespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &zero,
			Selector:       &metav1.LabelSelector{MatchLabels: sel},
		},
	}
}

func loosenPDB(ctx context.Context, t *testing.T, cl client.Client, pdb *policyv1.PodDisruptionBudget) {
	t.Helper()
	var fresh policyv1.PodDisruptionBudget
	if err := cl.Get(ctx, types.NamespacedName{Namespace: pdb.Namespace, Name: pdb.Name}, &fresh); err != nil {
		t.Fatalf("get PDB to loosen: %v", err)
	}
	one := intstr.FromInt32(1)
	fresh.Spec.MaxUnavailable = &one
	if err := cl.Update(ctx, &fresh); err != nil {
		t.Fatalf("loosen PDB: %v", err)
	}
}

func getPlaceholder(ctx context.Context, cl client.Client, candClaim string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := cl.List(ctx, &pods, client.InNamespace(controllerNamespace),
		client.MatchingLabels{annotations.SurgeFor: candClaim}); err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// resetCluster deletes the sample workloads, PDBs, leftover placeholder Pods, and
// all NodeClaims so the next subtest starts from an empty pool, and waits for the
// controller to settle (no in-flight rotation). It tolerates NotFound on everything.
//
// Order matters. A subtest may return with a rotation still in flight (e.g.
// DoNotDisrupt asserts the in-flight surge pair and does not wait for completion).
// Its placeholder Pod carries the pod-level karpenter.sh/do-not-disrupt annotation
// (spec §3.3); left on a surge node whose NodeClaim we then delete, Karpenter's
// drain stalls on it and the claim never finalizes. So: clear the pool anchors
// first (the controller stops driving the rotation and won't recreate the
// placeholder), then delete the placeholder Pods, then the workloads/claims.
func resetCluster(ctx context.Context, t *testing.T, cl client.Client) {
	t.Helper()
	for _, pool := range []string{poolA, poolB, poolC} {
		clearPoolRotationState(ctx, cl, pool)
	}
	deletePlaceholders(ctx, cl)
	for _, name := range []string{"cand", "spare", "poolb", "preemptor", "ffcand"} {
		deleteDeployment(ctx, cl, name)
	}
	_ = cl.DeleteAllOf(ctx, &policyv1.PodDisruptionBudget{}, client.InNamespace(workloadNamespace))
	_ = cl.DeleteAllOf(ctx, &karpv1.NodeClaim{})
	eventually(t, 4*time.Minute, "the cluster to drain to zero NodeClaims and no in-flight rotation", func() error {
		// Re-clear residue every pass: a slow in-flight reconcile can re-anchor or
		// recreate a placeholder between the initial sweep and now.
		deletePlaceholders(ctx, cl)
		for _, pool := range []string{poolA, poolB, poolC} {
			clearPoolRotationState(ctx, cl, pool)
			claims, err := listClaims(ctx, cl, pool)
			if err != nil {
				return err
			}
			if len(claims) != 0 {
				return fmt.Errorf("%s still has %d NodeClaims", pool, len(claims))
			}
		}
		return nil
	})
}

// deletePlaceholders removes every controller-owned placeholder Pod (those
// carrying the surge-for marker) from the controller namespace, tolerating
// NotFound. Used by resetCluster so a placeholder left on a node by an in-flight
// rotation cannot block that node's drain via its pod-level do-not-disrupt.
func deletePlaceholders(ctx context.Context, cl client.Client) {
	_ = cl.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(controllerNamespace),
		client.HasLabels{annotations.SurgeFor})
}

// clearPoolRotationState strips the controller's per-NodePool rotation
// annotations so a fresh subtest is not gated by a prior run's cooldown anchors.
func clearPoolRotationState(ctx context.Context, cl client.Client, pool string) {
	var p karpv1.NodePool
	if err := cl.Get(ctx, types.NamespacedName{Name: pool}, &p); err != nil {
		return
	}
	if p.Annotations == nil {
		return
	}
	for _, key := range []string{
		annotations.ActiveRotation, annotations.ActiveRotationState, annotations.DrainingAt,
		annotations.LastRotationAt, annotations.LastFailureAt, annotations.Freeze,
	} {
		delete(p.Annotations, key)
	}
	_ = cl.Update(ctx, &p)
}

// unused guards against accidental import pruning of resource (used via cpuQ in
// workloads_test.go) and metav1 helpers across build-tag boundaries.
var _ = resource.MustParse
var _ = metav1Now
