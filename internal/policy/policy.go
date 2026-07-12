// Package policy models the per-NodePool rotation policy (spec §5.4): the
// in-memory value object that a RotationPolicy CRD spec is converted into (see
// internal/resolve), with defaulting and structural validation. It owns only
// structural correctness (shape, enum strings, time and timezone formats, the v1
// surge constraints) — admission-time enforcement of the same rules lives on the
// CRD's OpenAPI/CEL schema (api/v1alpha1). Scheduling feasibility — the
// ageThreshold derivation and its layered warnings — lives in internal/schedule,
// and the temporal evaluation of maintenance windows lives in internal/window.
//
// NodePool targeting (which policy governs which NodePool) is NOT modeled here:
// the selector lives on the CRD object (spec.nodePoolSelector) and matching /
// conflict resolution is internal/resolve's job (spec §5.4).
package policy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Policy is the resolved per-NodePool rotation policy (spec §5.4) — the validated
// value object carrying the rotation gates and timeouts. Field tags are json: to
// match the CRD spec field names one-to-one (the carrier the values come from).
type Policy struct {
	AgeThreshold       string              `json:"ageThreshold"`       // "auto" or a Go duration (e.g. "120h")
	MinRotationChances *int                `json:"minRotationChances"` // K; floor 1. Pointer distinguishes unset (default 2) from an explicit 0 (invalid).
	MaintenanceWindows []MaintenanceWindow `json:"maintenanceWindows"`
	Surge              Surge               `json:"surge"`
	PrePull            FeatureToggle       `json:"prePull"` // v2; must be disabled in v1
}

// MaintenanceWindow is one recurrence entry; the effective window is the union
// of all entries (spec §3.1).
type MaintenanceWindow struct {
	Timezone string   `json:"timezone"` // IANA tz database name
	Days     []string `json:"days"`     // ISO weekday names Mon..Sun (case-insensitive)
	Start    string   `json:"start"`    // "HH:MM" 24-hour
	End      string   `json:"end"`      // "HH:MM" 24-hour, must be after Start (no overnight wrap)
}

// Surge holds the v1 surge-orchestration knobs (spec §3.3, §5.4). The duration
// fields are pointers so an unset field (nil → defaulted) is distinguishable
// from an explicitly configured non-positive value (e.g. 0m), which Validate
// rejects rather than silently defaulting (issue #30).
type Surge struct {
	MaxUnavailable        *int                  `json:"maxUnavailable"` // v1: fixed at 1 (serial). Pointer distinguishes unset (default 1) from an explicit 0 (invalid).
	ReadyTimeout          *metav1.Duration      `json:"readyTimeout"`
	CooldownAfter         *metav1.Duration      `json:"cooldownAfter"`
	RetryBackoff          *metav1.Duration      `json:"retryBackoff"`
	MatchNodeRequirements MatchNodeRequirements `json:"matchNodeRequirements"`
	ForcefulFallback      FeatureToggle         `json:"forcefulFallback"` // §3.3, ADR-0001; opt-in surge-less window-bounded forceful fallback (default off)
	// DrainEstimate is the EXPECTED drain duration used by the layer-2 throughput
	// forecast (spec §3.2). It is not a bound: the deadline stays terminationGracePeriod.
	// nil means unset and is NOT defaulted here — the default is min(tGP, 10m) and tGP
	// lives on the NodePool template, so resolution happens in internal/schedule.
	DrainEstimate *metav1.Duration `json:"drainEstimate"`
	// ProvisioningEstimate is the EXPECTED surge-provisioning duration (candidate →
	// Ready) used by the layer-2 throughput forecast (spec §3.2, ADR-0003). It is not a
	// bound: the deadline stays readyTimeout. nil means unset and is NOT defaulted here
	// — the default is min(readyTimeout, 5m), resolved in internal/schedule alongside
	// drainEstimate so both forecast terms share one resolution path.
	ProvisioningEstimate *metav1.Duration `json:"provisioningEstimate"`
	// FailurePause is the pool-level inter-attempt pause after a FAILED attempt (gate B,
	// spec §5.2, §4.4, ADR-0004). It is read only against last-failure-at and feeds no
	// throughput forecast. nil means unset and is NOT defaulted here — the default is
	// max(FailurePauseFloor, cooldownAfter), resolved in the controller alongside the
	// other gate values (it depends on cooldownAfter, which an operator may override).
	FailurePause *metav1.Duration `json:"failurePause"`
}

// FailurePauseFloor is the lower bound of surge.failurePause's default: an unset
// failurePause resolves to max(FailurePauseFloor, cooldownAfter), so no install's
// post-failure pause shortens on upgrade even if cooldownAfter is lowered for
// throughput, and a pause is always at least this long (spec §4.4, ADR-0004).
const FailurePauseFloor = 10 * time.Minute

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

// ApplyDefaults fills zero-valued fields with the §5.4 defaults. The CRD's
// OpenAPI schema already defaults these at admission time, so a Policy converted
// from an apiserver read is normally complete; this re-default is defense in
// depth for a hand-built or fake-client-sourced Policy. Only zero values are
// touched, so an explicit setting is never overwritten.
func (p *Policy) ApplyDefaults() {
	if p.AgeThreshold == "" {
		p.AgeThreshold = ageThresholdAuto
	}
	if p.MinRotationChances == nil {
		two := 2
		p.MinRotationChances = &two
	}
	// An unset maxUnavailable defaults to 1. An explicit 0 is preserved (not
	// coerced) so Validate can reject it: v1 permits only the literal value 1,
	// and silently rewriting 0 → 1 would hide a misconfiguration and make the
	// controller laxer than the Helm schema (which requires const 1). The pointer
	// is what makes the unset-vs-explicit-0 distinction.
	if p.Surge.MaxUnavailable == nil {
		one := 1
		p.Surge.MaxUnavailable = &one
	}
	// Only an UNSET (nil) duration is defaulted. An explicitly configured value —
	// including a non-positive one like 0m or -1m — is preserved so Validate can
	// reject it; silently rewriting it to the default would hide a misconfiguration
	// (issue #30).
	if p.Surge.ReadyTimeout == nil {
		p.Surge.ReadyTimeout = &metav1.Duration{Duration: 15 * time.Minute}
	}
	if p.Surge.CooldownAfter == nil {
		p.Surge.CooldownAfter = &metav1.Duration{Duration: 10 * time.Minute}
	}
	if p.Surge.RetryBackoff == nil {
		p.Surge.RetryBackoff = &metav1.Duration{Duration: 30 * time.Minute}
	}
	if len(p.Surge.MatchNodeRequirements.Required) == 0 {
		p.Surge.MatchNodeRequirements.Required = append([]string(nil), defaultRequiredRequirements...)
	}
	// surge.drainEstimate, surge.provisioningEstimate and surge.failurePause are
	// deliberately NOT defaulted here: their defaults are min(tGP, DrainEstimateDefault),
	// min(readyTimeout, ProvisioningEstimateDefault) and max(FailurePauseFloor,
	// cooldownAfter) — each depends on a value resolved elsewhere or on another field.
	// nil is carried onward: the two estimates into schedule.Derive (issues #212, #220),
	// failurePause into the controller's resolve() (issue #216).
}

// Validate runs structural checks only (shape, enums, formats, the v1 surge
// constraints). Scheduling feasibility (A<=0, G<K, throughput) is the job of
// internal/schedule. Call ApplyDefaults first (resolve.ToPolicy does). NodePool
// targeting is not checked here — the selector lives on the CRD, not on Policy.
func (p *Policy) Validate() error {
	var errs []error

	// K floor is structural: K < 1 is an invalid config (spec §3.2 layer 1
	// fatal). The K == 1 warning is a scheduling concern (internal/schedule).
	if p.MinRotationChances != nil && *p.MinRotationChances < 1 {
		errs = append(errs, fmt.Errorf("minRotationChances must be >= 1, got %d", *p.MinRotationChances))
	}

	// v1 is surge-only and serial per NodePool (CLAUDE.md invariant; spec §5.4).
	// The only valid explicit value is 1; an explicit 0 (or any other value)
	// fails. After ApplyDefaults an unset field is already 1, so nil here only
	// occurs when Validate is called without defaulting.
	if p.Surge.MaxUnavailable == nil || *p.Surge.MaxUnavailable != 1 {
		got := "unset"
		if p.Surge.MaxUnavailable != nil {
			got = strconv.Itoa(*p.Surge.MaxUnavailable)
		}
		errs = append(errs, fmt.Errorf("surge.maxUnavailable must be 1 in v1, got %s", got))
	}

	// Surge durations drive safety-critical timing in the rotation state machine
	// and schedule derivation (spec §5.2/§3.2). An unset field is defaulted in
	// ApplyDefaults; an explicitly configured non-positive value (0m, -1m) is
	// preserved and rejected here rather than silently entering an unsafe mode — a
	// non-positive readyTimeout fails attempts instantly, a non-positive retryBackoff
	// makes failed-claim retry timing nonsensical, a non-positive failurePause disables
	// the §4.4 cost bound on candidate cycling, and a non-positive drainEstimate or
	// provisioningEstimate would make the layer-2 throughput forecast meaningless
	// (t_rot_est is their sum). The nil guard only matters when Validate runs without
	// ApplyDefaults; Load always defaults first. drainEstimate, provisioningEstimate and
	// failurePause are exempt from that defaulting (their fallbacks depend on values
	// resolved elsewhere — §3.2 for the estimates, cooldownAfter for failurePause), so
	// nil reaches this loop as the normal, valid "unset" case and is skipped like the
	// others. cooldownAfter is checked separately below: it may be 0 (ADR-0004).
	for _, d := range []struct {
		name string
		val  *metav1.Duration
	}{
		{"surge.readyTimeout", p.Surge.ReadyTimeout},
		{"surge.retryBackoff", p.Surge.RetryBackoff},
		{"surge.drainEstimate", p.Surge.DrainEstimate},
		{"surge.provisioningEstimate", p.Surge.ProvisioningEstimate},
		{"surge.failurePause", p.Surge.FailurePause},
	} {
		if d.val != nil && d.val.Duration <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %v", d.name, d.val.Duration))
		}
	}

	// cooldownAfter is the post-success settle only (gate A, ADR-0004). It may be 0 —
	// PDBs are the primary settle mechanism, so an operator who serializes drains with
	// PDBs can reclaim the window throughput this pause would otherwise consume. A
	// negative value is still nonsensical and rejected.
	if p.Surge.CooldownAfter != nil && p.Surge.CooldownAfter.Duration < 0 {
		errs = append(errs, fmt.Errorf("surge.cooldownAfter must not be negative, got %v", p.Surge.CooldownAfter.Duration))
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

	// The v2 pre-pull feature is not implemented in v1; a true value is a misconfig.
	if p.PrePull.Enabled {
		errs = append(errs, errors.New("prePull.enabled must be false in v1"))
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

// SurgeMaxUnavailable returns the resolved surge.maxUnavailable, or 0 when unset
// (before ApplyDefaults). Callers should run ApplyDefaults/Load first.
func (p *Policy) SurgeMaxUnavailable() int {
	if p.Surge.MaxUnavailable == nil {
		return 0
	}
	return *p.Surge.MaxUnavailable
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
