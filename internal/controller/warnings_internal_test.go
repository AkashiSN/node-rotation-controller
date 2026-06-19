package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
	"github.com/AkashiSN/node-rotation-controller/internal/selection"
)

// drain returns all events currently buffered in the fake recorder.
func drain(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func warnPool() *karpv1.NodePool {
	return &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "np"}}
}

func TestEmitFindingsWarnOnlyAndDedup(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	w := newWarningEmitter(rec)
	ctx := context.Background()
	pool := warnPool()
	findings := []schedule.Finding{
		{Severity: schedule.Warn, Code: "KBelowTwo", Message: "k=1 risky"},
		{Severity: schedule.Fatal, Code: "ANonPositive", Message: "fatal, skipped"},
	}

	w.EmitFindings(ctx, pool, findings)
	got := drain(rec)
	if len(got) != 1 {
		t.Fatalf("want 1 event (warn only, fatal skipped), got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "KBelowTwo") || !strings.Contains(got[0], "Warning") {
		t.Fatalf("event missing reason/type: %q", got[0])
	}

	// Same findings again: no re-fire.
	w.EmitFindings(ctx, pool, findings)
	if got := drain(rec); len(got) != 0 {
		t.Fatalf("want 0 events on unchanged repeat, got %d: %v", len(got), got)
	}
}

func TestEmitFindingsRefiresAfterClear(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	w := newWarningEmitter(rec)
	ctx := context.Background()
	pool := warnPool()
	warn := []schedule.Finding{{Severity: schedule.Warn, Code: "TGPUnset", Message: "tgp unset"}}

	w.EmitFindings(ctx, pool, warn)
	drain(rec)
	w.EmitFindings(ctx, pool, nil) // cleared
	if got := drain(rec); len(got) != 0 {
		t.Fatalf("clearing should emit nothing, got %v", got)
	}
	w.EmitFindings(ctx, pool, warn) // returns → re-fire
	if got := drain(rec); len(got) != 1 {
		t.Fatalf("want re-fire after clear, got %d: %v", len(got), got)
	}
}

func TestEmitShortLeadPerClaimAndDedup(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	w := newWarningEmitter(rec)
	ctx := context.Background()
	pool := warnPool()
	short := time.Hour
	long := 30 * 24 * time.Hour
	claims := []karpv1.NodeClaim{
		shortLeadClaim("a", short),
		shortLeadClaim("b", short),
		shortLeadClaim("ample", long),
	}
	lead := selection.LeadTime{Base: 24 * time.Hour}

	w.EmitShortLead(ctx, pool, claims, lead)
	got := drain(rec)
	if len(got) != 2 {
		t.Fatalf("want 2 short-lead events, got %d: %v", len(got), got)
	}

	// Repeat: no re-fire.
	w.EmitShortLead(ctx, pool, claims, lead)
	if got := drain(rec); len(got) != 0 {
		t.Fatalf("want 0 on unchanged repeat, got %v", got)
	}
}

func TestForgetClearsDedup(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	w := newWarningEmitter(rec)
	ctx := context.Background()
	pool := warnPool()
	warn := []schedule.Finding{{Severity: schedule.Warn, Code: "KBelowTwo", Message: "m"}}

	w.EmitFindings(ctx, pool, warn)
	drain(rec)
	w.Forget("np")
	w.EmitFindings(ctx, pool, warn)
	if got := drain(rec); len(got) != 1 {
		t.Fatalf("want re-fire after Forget, got %d: %v", len(got), got)
	}
}

// A fresh emitter has no in-memory dedup state, so an active finding that was
// already warned by a previous instance re-fires exactly once. This models a
// controller restart (spec §4.2: "The dedup state is in-memory, so a controller
// restart re-emits each active warning once").
func TestEmitFindingsRestartReEmitsActiveOnce(t *testing.T) {
	ctx := context.Background()
	pool := warnPool()
	warn := []schedule.Finding{{Severity: schedule.Warn, Code: "KBelowTwo", Message: "k=1 risky"}}

	rec1 := events.NewFakeRecorder(16)
	w1 := newWarningEmitter(rec1)
	w1.EmitFindings(ctx, pool, warn)
	if got := drain(rec1); len(got) != 1 {
		t.Fatalf("first instance: want 1 event, got %d: %v", len(got), got)
	}
	// Same instance, still active: no re-fire.
	w1.EmitFindings(ctx, pool, warn)
	if got := drain(rec1); len(got) != 0 {
		t.Fatalf("first instance: want 0 on unchanged repeat, got %v", got)
	}

	// Restart: a brand-new emitter (empty in-memory state) re-emits once.
	rec2 := events.NewFakeRecorder(16)
	w2 := newWarningEmitter(rec2)
	w2.EmitFindings(ctx, pool, warn)
	got := drain(rec2)
	if len(got) != 1 {
		t.Fatalf("after restart: want 1 re-emitted event, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "KBelowTwo") {
		t.Fatalf("after restart: event missing reason: %q", got[0])
	}
	// And the restarted instance does not re-fire while still active.
	w2.EmitFindings(ctx, pool, warn)
	if got := drain(rec2); len(got) != 0 {
		t.Fatalf("after restart: want 0 on unchanged repeat, got %v", got)
	}
}

// Restart semantics for the §3.2 layer-3 ShortLead surface: a fresh emitter
// re-emits each active short-lead claim once.
func TestEmitShortLeadRestartReEmitsActiveOnce(t *testing.T) {
	ctx := context.Background()
	pool := warnPool()
	claims := []karpv1.NodeClaim{shortLeadClaim("a", time.Hour)}
	lead := selection.LeadTime{Base: 24 * time.Hour}

	rec1 := events.NewFakeRecorder(16)
	w1 := newWarningEmitter(rec1)
	w1.EmitShortLead(ctx, pool, claims, lead)
	if got := drain(rec1); len(got) != 1 {
		t.Fatalf("first instance: want 1 event, got %d: %v", len(got), got)
	}

	// Restart: fresh emitter re-emits the still-active claim once.
	rec2 := events.NewFakeRecorder(16)
	w2 := newWarningEmitter(rec2)
	w2.EmitShortLead(ctx, pool, claims, lead)
	if got := drain(rec2); len(got) != 1 {
		t.Fatalf("after restart: want 1 re-emitted event, got %d: %v", len(got), got)
	}
	w2.EmitShortLead(ctx, pool, claims, lead)
	if got := drain(rec2); len(got) != 0 {
		t.Fatalf("after restart: want 0 on unchanged repeat, got %v", got)
	}
}

// A short-lead claim that clears and later returns re-fires — the transition
// dedup applies per-claim just as it does for findings.
func TestEmitShortLeadRefiresAfterClear(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	w := newWarningEmitter(rec)
	ctx := context.Background()
	pool := warnPool()
	claims := []karpv1.NodeClaim{shortLeadClaim("a", time.Hour)}
	lead := selection.LeadTime{Base: 24 * time.Hour}

	w.EmitShortLead(ctx, pool, claims, lead)
	drain(rec)
	w.EmitShortLead(ctx, pool, nil, lead) // cleared (no claims short-lead)
	if got := drain(rec); len(got) != 0 {
		t.Fatalf("clearing should emit nothing, got %v", got)
	}
	w.EmitShortLead(ctx, pool, claims, lead) // returns → re-fire
	if got := drain(rec); len(got) != 1 {
		t.Fatalf("want re-fire after clear, got %d: %v", len(got), got)
	}
}

// Forget drops the per-NodePool dedup state, so a recreated pool warns cleanly
// from a clean slate for both findings and short-lead claims.
func TestForgetLetsRecreatedPoolWarnCleanly(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	w := newWarningEmitter(rec)
	ctx := context.Background()
	pool := warnPool()
	warn := []schedule.Finding{{Severity: schedule.Warn, Code: "KBelowTwo", Message: "m"}}
	claims := []karpv1.NodeClaim{shortLeadClaim("a", time.Hour)}
	lead := selection.LeadTime{Base: 24 * time.Hour}

	w.EmitFindings(ctx, pool, warn)
	w.EmitShortLead(ctx, pool, claims, lead)
	drain(rec)

	// NodePool deleted → drop its dedup state.
	w.Forget("np")

	// Recreated pool of the same name re-warns from a clean slate.
	w.EmitFindings(ctx, pool, warn)
	w.EmitShortLead(ctx, pool, claims, lead)
	if got := drain(rec); len(got) != 2 {
		t.Fatalf("want finding+short-lead re-fire after Forget, got %d: %v", len(got), got)
	}
}

func TestNilRecorderIsSafe(t *testing.T) {
	w := newWarningEmitter(nil)
	// Must not panic.
	w.EmitFindings(context.Background(), warnPool(),
		[]schedule.Finding{{Severity: schedule.Warn, Code: "KBelowTwo", Message: "m"}})
	w.EmitShortLead(context.Background(), warnPool(),
		[]karpv1.NodeClaim{shortLeadClaim("a", time.Hour)}, selection.LeadTime{Base: 24 * time.Hour})
}

func shortLeadClaim(name string, expire time.Duration) karpv1.NodeClaim {
	e := expire
	return karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       karpv1.NodeClaimSpec{ExpireAfter: karpv1.NillableDuration{Duration: &e}},
	}
}
