package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/AkashiSN/node-rotation-controller/internal/annotations"
)

// captureLogger returns a logr.Logger that records every emitted line (with its
// V level) into *lines, with verbosity 1 so V(1) debug lines are kept. It is the
// test seam for the issue #100 per-pass debug logging: production code reads the
// logger from the context (log.FromContext), so injecting one here lets a pure
// unit test assert what each reconcile emits without envtest.
func captureLogger(lines *[]string) logr.Logger {
	return funcr.New(func(prefix, args string) {
		*lines = append(*lines, args)
	}, funcr.Options{Verbosity: 1})
}

// containsLine reports whether any captured line contains all of the substrings.
func containsLine(lines []string, subs ...string) bool {
	for _, l := range lines {
		all := true
		for _, s := range subs {
			if !strings.Contains(l, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// TestDebugLoggingEmitsHeartbeatAndUndedupFindingsEachPass asserts the issue #100
// additive debug visibility: every reconcile pass emits, at V(1), a per-pass
// heartbeat and the current findings un-deduplicated — even on the second pass,
// where the transition-deduped INFO warning/Event stays silent. A NodePool with
// a never-set terminationGracePeriod yields the (non-fatal) TGPUnset Warn finding,
// so a single Warn finding is present every pass while the schedule still starts.
func TestDebugLoggingEmitsHeartbeatAndUndedupFindingsEachPass(t *testing.T) {
	// Healthy schedule (14d E) but tGP unset → a TGPUnset Warn finding present
	// every pass; eligible candidate so the pool also has work to report.
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode))
	pool := withTemplateE(testNodePool(nil), 14*24*time.Hour)
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, false))

	var pass1 []string
	ctx1 := log.IntoContext(context.Background(), captureLogger(&pass1))
	if _, err := r.reconcileNodePool(ctx1, pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 1 reconcileNodePool: %v", err)
	}

	if !containsLine(pass1, "reconcile", "phase", "candidates") {
		t.Errorf("pass 1: missing per-pass heartbeat; lines = %v", pass1)
	}
	if !containsLine(pass1, "schedule feasibility warning (debug, per-pass)", "TGPUnset") {
		t.Errorf("pass 1: missing un-deduplicated debug finding; lines = %v", pass1)
	}

	// Second pass: the finding is unchanged, so the transition dedup suppresses the
	// INFO "schedule feasibility warning" line — but the per-pass debug heartbeat
	// and the un-deduplicated debug finding MUST still fire (the whole point of #100).
	var pass2 []string
	ctx2 := log.IntoContext(context.Background(), captureLogger(&pass2))
	if _, err := r.reconcileNodePool(ctx2, pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("pass 2 reconcileNodePool: %v", err)
	}

	if !containsLine(pass2, "reconcile", "phase", "candidates") {
		t.Errorf("pass 2: heartbeat must fire every pass; lines = %v", pass2)
	}
	if !containsLine(pass2, "schedule feasibility warning (debug, per-pass)", "TGPUnset") {
		t.Errorf("pass 2: un-deduplicated debug finding must fire every pass; lines = %v", pass2)
	}
	// The deduped INFO line must NOT repeat on the unchanged second pass.
	if containsLine(pass2, `"schedule feasibility warning"`) {
		t.Errorf("pass 2: deduped INFO warning must stay silent on unchanged repeat; lines = %v", pass2)
	}
}

// TestDebugHeartbeatReportsInFlightPhase: when a rotation is anchored, the
// heartbeat's phase reflects the active-rotation state rather than "idle".
func TestDebugHeartbeatReportsInFlightPhase(t *testing.T) {
	cand := testClaim("nc-old", 20*24*time.Hour, ncNode(candNode),
		ncAnn(annotations.State, annotations.StateDraining),
		ncFinalizer())
	pool := withTGP(testNodePool(map[string]string{
		annotations.ActiveRotation:      "nc-old",
		annotations.ActiveRotationState: annotations.StateDraining,
	}))
	r := newReconciler(t, testNow, nil, pool, cand, testK8sNode(candNode, true, nil, true))

	var lines []string
	ctx := log.IntoContext(context.Background(), captureLogger(&lines))
	if _, err := r.reconcileNodePool(ctx, pool, testPolicy(), mustSchedule(t)); err != nil {
		t.Fatalf("reconcileNodePool: %v", err)
	}
	if !containsLine(lines, "reconcile", "phase", annotations.StateDraining) {
		t.Errorf("heartbeat must report the in-flight phase %q; lines = %v", annotations.StateDraining, lines)
	}
}

// guard against an accidentally-unused import if the helpers above are trimmed.
var _ = time.Second
