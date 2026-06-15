// Package policy models the v1 ConfigMap configuration schema (spec §5.4):
// parsing, defaulting, and structural validation of data.policy.yaml. It owns
// only the wire format and structural correctness (shape, enum strings, time
// and timezone formats, the v1 surge constraints). Scheduling feasibility — the
// ageThreshold derivation and its layered warnings — lives in internal/schedule,
// and the temporal evaluation of maintenance windows lives in internal/window.
package policy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Policy is the parsed data.policy.yaml document (spec §5.4). Field tags are
// json: because sigs.k8s.io/yaml routes YAML through encoding/json.
type Policy struct {
	NodePoolSelectors  []Selector          `json:"nodepoolSelectors"`
	AgeThreshold       string              `json:"ageThreshold"`       // "auto" or a Go duration (e.g. "120h")
	MinRotationChances *int                `json:"minRotationChances"` // K; floor 1. Pointer distinguishes unset (default 2) from an explicit 0 (invalid).
	MaintenanceWindows []MaintenanceWindow `json:"maintenanceWindows"`
	Surge              Surge               `json:"surge"`
	PrePull            FeatureToggle       `json:"prePull"` // v2; must be disabled in v1
	Warmup             FeatureToggle       `json:"warmup"`  // v3; must be disabled in v1
}

// Selector matches in-scope NodePools by label (spec §3.2, §5.4).
type Selector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

// MaintenanceWindow is one recurrence entry; the effective window is the union
// of all entries (spec §3.1).
type MaintenanceWindow struct {
	Timezone string   `json:"timezone"` // IANA tz database name
	Days     []string `json:"days"`     // ISO weekday names Mon..Sun (case-insensitive)
	Start    string   `json:"start"`    // "HH:MM" 24-hour
	End      string   `json:"end"`      // "HH:MM" 24-hour, must be after Start (no overnight wrap)
}

// Surge holds the v1 surge-orchestration knobs (spec §3.3, §5.4).
type Surge struct {
	MaxUnavailable        int                   `json:"maxUnavailable"` // v1: fixed at 1 (serial)
	ReadyTimeout          metav1.Duration       `json:"readyTimeout"`
	CooldownAfter         metav1.Duration       `json:"cooldownAfter"`
	RetryBackoff          metav1.Duration       `json:"retryBackoff"`
	MatchNodeRequirements MatchNodeRequirements `json:"matchNodeRequirements"`
}

// MatchNodeRequirements selects which candidate-node requirements the
// placeholder Pod replicates (spec §3.3). The required karpenter.sh/nodepool
// selector is NOT listed here — it is always applied unconditionally.
type MatchNodeRequirements struct {
	Required  []string `json:"required"`  // hard nodeAffinity, copied from the candidate node
	Preferred []string `json:"preferred"` // soft nodeAffinity, relaxed under capacity pressure
}

// FeatureToggle gates the reserved v2/v3 expansion points (spec §5.4).
type FeatureToggle struct {
	Enabled bool `json:"enabled"`
}

// ageThresholdAuto is the sentinel meaning "derive ageThreshold per NodePool".
const ageThresholdAuto = "auto"

// defaultRequiredRequirements is the §5.4 default hard-affinity set replicated
// onto the placeholder when matchNodeRequirements.required is unset.
var defaultRequiredRequirements = []string{
	"topology.kubernetes.io/zone",
	"kubernetes.io/arch",
	"karpenter.sh/capacity-type",
}

// isoWeekdays maps lower-cased ISO weekday names to time.Weekday.
var isoWeekdays = map[string]time.Weekday{
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
	"sun": time.Sunday,
}

// Load parses data.policy.yaml, applies the §5.4 defaults, and runs structural
// validation. Unknown keys are rejected (UnmarshalStrict) so a typo'd field is
// loud rather than silently dropped. A non-nil error means the document is
// structurally invalid; scheduling feasibility is checked separately.
func Load(data []byte) (*Policy, error) {
	var p Policy
	if err := yaml.UnmarshalStrict(data, &p); err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// ApplyDefaults fills zero-valued fields with the §5.4 defaults. It is exported
// so callers can default a hand-built Policy without round-tripping YAML. Only
// zero values are touched, so an explicit setting is never overwritten.
func (p *Policy) ApplyDefaults() {
	if p.AgeThreshold == "" {
		p.AgeThreshold = ageThresholdAuto
	}
	if p.MinRotationChances == nil {
		two := 2
		p.MinRotationChances = &two
	}
	// An unset maxUnavailable defaults to 1; an explicit 0 is also coerced to 1
	// here. Unlike minRotationChances, v1 permits only the single value 1
	// (Validate rejects anything else), so the unset-vs-explicit-0 distinction
	// carries no meaning worth a pointer.
	if p.Surge.MaxUnavailable == 0 {
		p.Surge.MaxUnavailable = 1
	}
	if p.Surge.ReadyTimeout.Duration == 0 {
		p.Surge.ReadyTimeout = metav1.Duration{Duration: 15 * time.Minute}
	}
	if p.Surge.CooldownAfter.Duration == 0 {
		p.Surge.CooldownAfter = metav1.Duration{Duration: 10 * time.Minute}
	}
	if p.Surge.RetryBackoff.Duration == 0 {
		p.Surge.RetryBackoff = metav1.Duration{Duration: 30 * time.Minute}
	}
	if len(p.Surge.MatchNodeRequirements.Required) == 0 {
		p.Surge.MatchNodeRequirements.Required = append([]string(nil), defaultRequiredRequirements...)
	}
}

// Validate runs structural checks only (shape, enums, formats, the v1 surge
// constraints). Scheduling feasibility (A<=0, G<K, throughput) is the job of
// internal/schedule. Call ApplyDefaults first (Load does).
func (p *Policy) Validate() error {
	var errs []error

	if len(p.NodePoolSelectors) == 0 {
		errs = append(errs, errors.New("nodepoolSelectors must not be empty"))
	}
	for i, s := range p.NodePoolSelectors {
		if len(s.MatchLabels) == 0 {
			errs = append(errs, fmt.Errorf("nodepoolSelectors[%d].matchLabels must not be empty", i))
		}
	}

	// K floor is structural: K < 1 is an invalid config (spec §3.2 layer 1
	// fatal). The K == 1 warning is a scheduling concern (internal/schedule).
	if p.MinRotationChances != nil && *p.MinRotationChances < 1 {
		errs = append(errs, fmt.Errorf("minRotationChances must be >= 1, got %d", *p.MinRotationChances))
	}

	// v1 is surge-only and serial per NodePool (CLAUDE.md invariant; spec §5.4).
	if p.Surge.MaxUnavailable != 1 {
		errs = append(errs, fmt.Errorf("surge.maxUnavailable must be 1 in v1, got %d", p.Surge.MaxUnavailable))
	}

	if _, _, err := p.AgeThresholdOverride(); err != nil {
		errs = append(errs, err)
	}

	if len(p.MaintenanceWindows) == 0 {
		errs = append(errs, errors.New("maintenanceWindows must not be empty"))
	}
	for i, w := range p.MaintenanceWindows {
		errs = append(errs, w.validate(i)...)
	}

	// v2/v3 features are not implemented in v1; a true value is a misconfig.
	if p.PrePull.Enabled {
		errs = append(errs, errors.New("prePull.enabled must be false in v1"))
	}
	if p.Warmup.Enabled {
		errs = append(errs, errors.New("warmup.enabled must be false in v1"))
	}

	return errors.Join(errs...)
}

// K returns the resolved minRotationChances, or 0 when unset (before
// ApplyDefaults). Callers should run ApplyDefaults/Load first.
func (p *Policy) K() int {
	if p.MinRotationChances == nil {
		return 0
	}
	return *p.MinRotationChances
}

// AgeThresholdOverride resolves the ageThreshold field: ("auto") => isAuto=true;
// a Go duration string => the positive override; anything else => error.
func (p *Policy) AgeThresholdOverride() (override time.Duration, isAuto bool, err error) {
	if p.AgeThreshold == ageThresholdAuto {
		return 0, true, nil
	}
	d, perr := time.ParseDuration(p.AgeThreshold)
	if perr != nil {
		return 0, false, fmt.Errorf("ageThreshold %q is neither %q nor a valid duration: %w", p.AgeThreshold, ageThresholdAuto, perr)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("ageThreshold override must be positive, got %v", d)
	}
	return d, false, nil
}

// validate checks one window entry; index i is woven into messages.
func (w MaintenanceWindow) validate(i int) []error {
	var errs []error

	if _, err := time.LoadLocation(w.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("maintenanceWindows[%d].timezone %q is not a valid IANA name: %w", i, w.Timezone, err))
	}

	if len(w.Days) == 0 {
		errs = append(errs, fmt.Errorf("maintenanceWindows[%d].days must not be empty", i))
	}
	for _, d := range w.Days {
		if _, err := ParseWeekday(d); err != nil {
			errs = append(errs, fmt.Errorf("maintenanceWindows[%d]: %w", i, err))
		}
	}

	start, serr := ParseHHMM(w.Start)
	if serr != nil {
		errs = append(errs, fmt.Errorf("maintenanceWindows[%d].start: %w", i, serr))
	}
	end, eerr := ParseHHMM(w.End)
	if eerr != nil {
		errs = append(errs, fmt.Errorf("maintenanceWindows[%d].end: %w", i, eerr))
	}
	if serr == nil && eerr == nil {
		switch {
		case start == end:
			errs = append(errs, fmt.Errorf("maintenanceWindows[%d]: start and end must differ", i))
		case end < start:
			// Overnight wrap is not supported in v1; split into two entries.
			errs = append(errs, fmt.Errorf("maintenanceWindows[%d]: end (%s) must be after start (%s); overnight windows must be split into two entries", i, w.End, w.Start))
		}
	}

	return errs
}

// ParseWeekday converts an ISO weekday name (case-insensitive) to time.Weekday.
func ParseWeekday(s string) (time.Weekday, error) {
	if wd, ok := isoWeekdays[strings.ToLower(strings.TrimSpace(s))]; ok {
		return wd, nil
	}
	return 0, fmt.Errorf("invalid weekday %q (want one of Mon Tue Wed Thu Fri Sat Sun)", s)
}

// ParseHHMM parses a strict "HH:MM" 24-hour time into minutes since midnight.
func ParseHHMM(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("time %q must be HH:MM", s)
	}
	h, herr := strconv.Atoi(parts[0])
	m, merr := strconv.Atoi(parts[1])
	if herr != nil || merr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q is out of range (00:00–23:59)", s)
	}
	return h*60 + m, nil
}
