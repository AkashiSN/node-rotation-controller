package policy

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// validPolicy is the §5.4 example reduced to the structurally-required fields plus
// a well-formed window. ApplyDefaults fills the rest. Tests mutate a copy to
// exercise individual validation branches.
func validPolicy() *Policy {
	return &Policy{
		MaintenanceWindows: []MaintenanceWindow{{
			Timezone: "Asia/Tokyo",
			Days:     []string{"Wed", "Sat"},
			Start:    "02:00",
			End:      "06:00",
		}},
	}
}

func durPtr(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func TestApplyDefaultsAndFields(t *testing.T) {
	p := &Policy{
		AgeThreshold:       "auto",
		MinRotationChances: new(2),
		MaintenanceWindows: []MaintenanceWindow{{
			Timezone: "Asia/Tokyo", Days: []string{"Wed", "Sat"}, Start: "02:00", End: "06:00",
		}},
		Surge: Surge{
			MaxUnavailable: new(1),
			ReadyTimeout:   durPtr(15 * time.Minute),
			CooldownAfter:  durPtr(10 * time.Minute),
			RetryBackoff:   durPtr(30 * time.Minute),
			MatchNodeRequirements: MatchNodeRequirements{Required: []string{
				"topology.kubernetes.io/zone",
				"kubernetes.io/arch",
				"karpenter.sh/capacity-type",
			}},
		},
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if p.AgeThreshold != "auto" {
		t.Errorf("ageThreshold = %q, want auto", p.AgeThreshold)
	}
	if p.K() != 2 {
		t.Errorf("minRotationChances = %d, want 2", p.K())
	}
	if p.SurgeMaxUnavailable() != 1 {
		t.Errorf("maxUnavailable = %d, want 1", p.SurgeMaxUnavailable())
	}
	if got := len(p.Surge.MatchNodeRequirements.Required); got != 3 {
		t.Errorf("required reqs len = %d, want 3", got)
	}
}

func TestApplyDefaultsFillsZeroValues(t *testing.T) {
	p := validPolicy()
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if p.AgeThreshold != "auto" {
		t.Errorf("ageThreshold default = %q, want auto", p.AgeThreshold)
	}
	if p.K() != 2 {
		t.Errorf("minRotationChances default = %d, want 2", p.K())
	}
	if p.SurgeMaxUnavailable() != 1 {
		t.Errorf("maxUnavailable default = %d, want 1", p.SurgeMaxUnavailable())
	}
	if p.Surge.ReadyTimeout.Duration != 15*time.Minute {
		t.Errorf("readyTimeout default = %v, want 15m", p.Surge.ReadyTimeout.Duration)
	}
	if p.Surge.CooldownAfter.Duration != 10*time.Minute {
		t.Errorf("cooldownAfter default = %v, want 10m", p.Surge.CooldownAfter.Duration)
	}
	if p.Surge.RetryBackoff.Duration != 30*time.Minute {
		t.Errorf("retryBackoff default = %v, want 30m", p.Surge.RetryBackoff.Duration)
	}
	want := []string{
		"topology.kubernetes.io/zone",
		"kubernetes.io/arch",
		"karpenter.sh/capacity-type",
	}
	got := p.Surge.MatchNodeRequirements.Required
	if len(got) != len(want) {
		t.Fatalf("required default = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("required[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAgeThresholdOverride(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantAuto bool
		wantDur  time.Duration
		wantErr  bool
	}{
		{name: "auto", value: "auto", wantAuto: true},
		{name: "override", value: "120h", wantDur: 120 * time.Hour},
		{name: "negative", value: "-1h", wantErr: true},
		{name: "zero", value: "0s", wantErr: true},
		{name: "bogus", value: "soon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Policy{AgeThreshold: tt.value}
			dur, isAuto, err := p.AgeThresholdOverride()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("AgeThresholdOverride(%q) err = nil, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("AgeThresholdOverride(%q) err = %v", tt.value, err)
			}
			if isAuto != tt.wantAuto {
				t.Errorf("isAuto = %v, want %v", isAuto, tt.wantAuto)
			}
			if dur != tt.wantDur {
				t.Errorf("override = %v, want %v", dur, tt.wantDur)
			}
		})
	}
}

// TestValidateStructuralErrors mutates a structurally-valid Policy into each
// invalid shape and asserts ApplyDefaults+Validate rejects it. ApplyDefaults runs
// first (as resolve.ToPolicy does): it never overwrites an explicitly set value,
// so an explicit 0m duration or maxUnavailable=0 survives to be rejected.
func TestValidateStructuralErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Policy)
	}{
		{"maxUnavailable not 1", func(p *Policy) { p.Surge.MaxUnavailable = new(2) }},
		{"maxUnavailable explicit zero", func(p *Policy) { p.Surge.MaxUnavailable = new(0) }},
		{"no windows", func(p *Policy) { p.MaintenanceWindows = nil }},
		{"bad timezone", func(p *Policy) { p.MaintenanceWindows[0].Timezone = "Asia/Nowhere" }},
		{"bad weekday", func(p *Policy) { p.MaintenanceWindows[0].Days = []string{"Funday"} }},
		{"bad start time", func(p *Policy) { p.MaintenanceWindows[0].Start = "26:61" }},
		{"empty days", func(p *Policy) { p.MaintenanceWindows[0].Days = nil }},
		{"start equals end", func(p *Policy) {
			p.MaintenanceWindows[0].Start = "02:00"
			p.MaintenanceWindows[0].End = "02:00"
		}},
		{"overnight wrap forbidden", func(p *Policy) {
			p.MaintenanceWindows[0].Start = "22:00"
			p.MaintenanceWindows[0].End = "02:00"
		}},
		{"prePull enabled", func(p *Policy) { p.PrePull.Enabled = true }},
		{"K below 1", func(p *Policy) { p.MinRotationChances = new(0) }},
		{"negative readyTimeout", func(p *Policy) { p.Surge.ReadyTimeout = durPtr(-1 * time.Minute) }},
		{"negative cooldownAfter", func(p *Policy) { p.Surge.CooldownAfter = durPtr(-1 * time.Minute) }},
		{"negative retryBackoff", func(p *Policy) { p.Surge.RetryBackoff = durPtr(-1 * time.Minute) }},
		{"explicit zero readyTimeout", func(p *Policy) { p.Surge.ReadyTimeout = durPtr(0) }},
		{"explicit zero cooldownAfter", func(p *Policy) { p.Surge.CooldownAfter = durPtr(0) }},
		{"explicit zero retryBackoff", func(p *Policy) { p.Surge.RetryBackoff = durPtr(0) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPolicy()
			tt.mutate(p)
			p.ApplyDefaults()
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate(%s) err = nil, want structural error", tt.name)
			}
		})
	}
}

func TestWeekdayCaseInsensitive(t *testing.T) {
	p := validPolicy()
	p.MaintenanceWindows[0].Days = []string{"wed", "SAT"}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate with mixed-case weekdays err = %v", err)
	}
}
