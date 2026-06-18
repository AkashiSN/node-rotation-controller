package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/AkashiSN/node-rotation-controller/internal/schedule"
)

// drain returns all events currently buffered in the fake recorder.
func drain(rec *record.FakeRecorder) []string {
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
	rec := record.NewFakeRecorder(16)
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
	rec := record.NewFakeRecorder(16)
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
	rec := record.NewFakeRecorder(16)
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
	lead := 24 * time.Hour

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
	rec := record.NewFakeRecorder(16)
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

func TestNilRecorderIsSafe(t *testing.T) {
	w := newWarningEmitter(nil)
	// Must not panic.
	w.EmitFindings(context.Background(), warnPool(),
		[]schedule.Finding{{Severity: schedule.Warn, Code: "KBelowTwo", Message: "m"}})
	w.EmitShortLead(context.Background(), warnPool(),
		[]karpv1.NodeClaim{shortLeadClaim("a", time.Hour)}, 24*time.Hour)
}

func shortLeadClaim(name string, expire time.Duration) karpv1.NodeClaim {
	e := expire
	return karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       karpv1.NodeClaimSpec{ExpireAfter: karpv1.NillableDuration{Duration: &e}},
	}
}
