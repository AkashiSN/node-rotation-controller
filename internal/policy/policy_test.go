package policy

import (
	"testing"
	"time"
)

// fullYAML is the §5.4 ConfigMap example (durations in Go format; the spec's
// prose "4d"-style values are explanatory only — time.ParseDuration rejects "d").
const fullYAML = `
nodepoolSelectors:
  - matchLabels:
      workload: api
ageThreshold: auto
minRotationChances: 2
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [Wed, Sat]
    start: "02:00"
    end:   "06:00"
surge:
  maxUnavailable: 1
  readyTimeout: 15m
  cooldownAfter: 10m
  retryBackoff: 30m
  matchNodeRequirements:
    required:
      - topology.kubernetes.io/zone
      - kubernetes.io/arch
      - karpenter.sh/capacity-type
    preferred: []
prePull:
  enabled: false
warmup:
  enabled: false
`

// minimalYAML carries only the structurally-required fields; everything else
// must come from ApplyDefaults.
const minimalYAML = `
nodepoolSelectors:
  - matchLabels:
      workload: api
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [Wed, Sat]
    start: "02:00"
    end:   "06:00"
`

func TestLoadFull(t *testing.T) {
	p, err := Load([]byte(fullYAML))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(p.NodePoolSelectors) != 1 || p.NodePoolSelectors[0].MatchLabels["workload"] != "api" {
		t.Errorf("nodepoolSelectors not parsed: %+v", p.NodePoolSelectors)
	}
	if p.AgeThreshold != "auto" {
		t.Errorf("ageThreshold = %q, want auto", p.AgeThreshold)
	}
	if p.K() != 2 {
		t.Errorf("minRotationChances = %d, want 2", p.K())
	}
	if got := len(p.MaintenanceWindows); got != 1 {
		t.Fatalf("maintenanceWindows len = %d, want 1", got)
	}
	w := p.MaintenanceWindows[0]
	if w.Timezone != "Asia/Tokyo" || w.Start != "02:00" || w.End != "06:00" {
		t.Errorf("window not parsed: %+v", w)
	}
	if p.SurgeMaxUnavailable() != 1 {
		t.Errorf("maxUnavailable = %d, want 1", p.SurgeMaxUnavailable())
	}
	if p.Surge.ReadyTimeout.Duration != 15*time.Minute {
		t.Errorf("readyTimeout = %v, want 15m", p.Surge.ReadyTimeout.Duration)
	}
	if p.Surge.CooldownAfter.Duration != 10*time.Minute {
		t.Errorf("cooldownAfter = %v, want 10m", p.Surge.CooldownAfter.Duration)
	}
	if p.Surge.RetryBackoff.Duration != 30*time.Minute {
		t.Errorf("retryBackoff = %v, want 30m", p.Surge.RetryBackoff.Duration)
	}
	if got := len(p.Surge.MatchNodeRequirements.Required); got != 3 {
		t.Errorf("required reqs len = %d, want 3", got)
	}
}

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	p, err := Load([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
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

func TestLoadRejectsUnknownKey(t *testing.T) {
	const y = minimalYAML + "\nmaxUnavailble: 2\n" // deliberate typo at top level
	if _, err := Load([]byte(y)); err == nil {
		t.Fatal("Load accepted unknown key, want error")
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

func TestValidateStructuralErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "no selectors",
			yaml: `
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [Wed]
    start: "02:00"
    end:   "06:00"
`,
		},
		{
			name: "empty matchLabels",
			yaml: `
nodepoolSelectors:
  - matchLabels: {}
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [Wed]
    start: "02:00"
    end:   "06:00"
`,
		},
		{
			name: "no windows",
			yaml: `
nodepoolSelectors:
  - matchLabels:
      workload: api
`,
		},
		{
			name: "maxUnavailable not 1",
			yaml: minimalYAML + `
surge:
  maxUnavailable: 2
`,
		},
		{
			name: "maxUnavailable explicit zero",
			yaml: minimalYAML + `
surge:
  maxUnavailable: 0
`,
		},
		{
			name: "bad timezone",
			yaml: `
nodepoolSelectors:
  - matchLabels:
      workload: api
maintenanceWindows:
  - timezone: Asia/Nowhere
    days: [Wed]
    start: "02:00"
    end:   "06:00"
`,
		},
		{
			name: "bad weekday",
			yaml: `
nodepoolSelectors:
  - matchLabels:
      workload: api
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [Funday]
    start: "02:00"
    end:   "06:00"
`,
		},
		{
			name: "bad start time",
			yaml: `
nodepoolSelectors:
  - matchLabels:
      workload: api
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [Wed]
    start: "26:61"
    end:   "06:00"
`,
		},
		{
			name: "start equals end",
			yaml: `
nodepoolSelectors:
  - matchLabels:
      workload: api
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [Wed]
    start: "02:00"
    end:   "02:00"
`,
		},
		{
			name: "overnight wrap forbidden",
			yaml: `
nodepoolSelectors:
  - matchLabels:
      workload: api
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [Wed]
    start: "22:00"
    end:   "02:00"
`,
		},
		{
			name: "empty days",
			yaml: `
nodepoolSelectors:
  - matchLabels:
      workload: api
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: []
    start: "02:00"
    end:   "06:00"
`,
		},
		{
			name: "prePull enabled",
			yaml: minimalYAML + `
prePull:
  enabled: true
`,
		},
		{
			name: "warmup enabled",
			yaml: minimalYAML + `
warmup:
  enabled: true
`,
		},
		{
			name: "K below 1",
			yaml: minimalYAML + `
minRotationChances: 0
`,
		},
		{
			name: "negative readyTimeout",
			yaml: minimalYAML + `
surge:
  readyTimeout: -1m
`,
		},
		{
			name: "negative cooldownAfter",
			yaml: minimalYAML + `
surge:
  cooldownAfter: -1m
`,
		},
		{
			name: "negative retryBackoff",
			yaml: minimalYAML + `
surge:
  retryBackoff: -1m
`,
		},
		{
			name: "explicit zero readyTimeout",
			yaml: minimalYAML + `
surge:
  readyTimeout: 0m
`,
		},
		{
			name: "explicit zero cooldownAfter",
			yaml: minimalYAML + `
surge:
  cooldownAfter: 0s
`,
		},
		{
			name: "explicit zero retryBackoff",
			yaml: minimalYAML + `
surge:
  retryBackoff: 0h
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load([]byte(tt.yaml)); err == nil {
				t.Fatalf("Load(%s) err = nil, want structural error", tt.name)
			}
		})
	}
}

func TestWeekdayCaseInsensitive(t *testing.T) {
	const y = `
nodepoolSelectors:
  - matchLabels:
      workload: api
maintenanceWindows:
  - timezone: Asia/Tokyo
    days: [wed, SAT]
    start: "02:00"
    end:   "06:00"
`
	if _, err := Load([]byte(y)); err != nil {
		t.Fatalf("Load with mixed-case weekdays err = %v", err)
	}
}
