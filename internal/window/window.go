// Package window evaluates the maintenance-window schedule (spec §3.1): whether
// a given instant falls inside the union of windows, and the worst-case period P
// (the largest gap between consecutive window occurrences) that feeds the
// ageThreshold derivation in internal/schedule. Each entry is evaluated in its
// own timezone. DST is treated as a wall-clock approximation per §3.1 — a ±1h
// transition is not special-cased.
//
// The binary that loads a Schedule must make IANA timezone data available; a
// distroless image needs `import _ "time/tzdata"` in the entrypoint for
// time.LoadLocation to resolve names like "Asia/Tokyo".
package window

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/policy"
)

const week = 7 * 24 * time.Hour

// continuousWindowPeriod is the worst-case period P reported for a continuously-
// open (full-week) maintenance window. Such a union has no gap between rotation
// opportunities, so P is effectively zero — but the derivation needs a *positive*
// P (a zero P is undefined and surfaced as a NoWindows fatal, §3.2), and the
// generic weekly-wrap below would otherwise report a spurious 7d. One reconcile
// tick (the controller's longRequeue cadence, §5.2) is the true granularity at
// which an always-open window offers a rotation chance, so P collapses to that
// rather than the week wrap, keeping leadTime ≈ t_rot and avoiding a spurious
// feasibility fatal (issue #62).
const continuousWindowPeriod = time.Minute

// Entry is a single normalized recurrence: a set of weekdays in one timezone
// with a daily [StartMin, EndMin) wall-clock interval (minutes since midnight).
type Entry struct {
	Loc      *time.Location
	Days     []time.Weekday
	StartMin int
	EndMin   int
}

// Schedule is the union of all entries — the effective maintenance window (§3.1).
type Schedule struct {
	entries []Entry
}

// anchorMonday is a reference Monday 00:00 UTC (2024-01-01 was a Monday) used to
// project every entry's recurring start onto a common weekly timeline.
var anchorMonday = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// New builds a Schedule from structurally-valid policy windows. It returns an
// error only on timezone load or time-parse failure (defense in depth;
// policy.Validate already checks these).
func New(ws []policy.MaintenanceWindow) (*Schedule, error) {
	s := &Schedule{}
	for i, w := range ws {
		loc, err := time.LoadLocation(w.Timezone)
		if err != nil {
			return nil, fmt.Errorf("maintenanceWindows[%d].timezone %q: %w", i, w.Timezone, err)
		}
		e := Entry{Loc: loc}
		for _, d := range w.Days {
			wd, err := policy.ParseWeekday(d)
			if err != nil {
				return nil, fmt.Errorf("maintenanceWindows[%d]: %w", i, err)
			}
			e.Days = append(e.Days, wd)
		}
		if e.StartMin, err = policy.ParseHHMM(w.Start); err != nil {
			return nil, fmt.Errorf("maintenanceWindows[%d].start: %w", i, err)
		}
		if e.EndMin, err = policy.ParseHHMM(w.End); err != nil {
			return nil, fmt.Errorf("maintenanceWindows[%d].end: %w", i, err)
		}
		s.entries = append(s.entries, e)
	}
	return s, nil
}

// InWindow reports whether now falls inside the union of entries. Each entry is
// evaluated in its own timezone; the interval is half-open [start, end).
func (s *Schedule) InWindow(now time.Time) bool {
	for _, e := range s.entries {
		local := now.In(e.Loc)
		mins := local.Hour()*60 + local.Minute()
		if mins < e.StartMin || mins >= e.EndMin {
			continue
		}
		if slices.Contains(e.Days, local.Weekday()) {
			return true
		}
	}
	return false
}

// occurrence is one occurrence of the effective maintenance window on the
// canonical weekly timeline: it begins at start (in [0, week)) and lasts length,
// possibly wrapping past the week boundary back toward 0.
type occurrence struct {
	start  time.Duration
	length time.Duration
}

// mergedOccurrences projects every entry occurrence onto the canonical weekly
// timeline (via startOffset, so cross-timezone overlaps are handled the same way
// WorstCasePeriod always has) and merges overlapping/adjacent ones: the effective
// maintenance window is the union of all entries (spec §3.1), so two adjacent
// entries are one occurrence, not two. The result drives both P (gaps between
// occurrence starts) and D (occurrence durations), keeping them consistent. It is
// empty when the schedule has no occurrences.
func (s *Schedule) mergedOccurrences() []occurrence {
	type span struct{ start, end time.Duration } // 0 <= start < end <= week
	var spans []span
	for _, e := range s.entries {
		length := time.Duration(e.EndMin-e.StartMin) * time.Minute
		if length <= 0 {
			continue // defensive: policy.Validate already rejects start >= end
		}
		for _, d := range e.Days {
			st := e.startOffset(d) // [0, week)
			if en := st + length; en > week {
				// Wraps the week boundary (possible after a cross-tz projection):
				// split into a head at the week start and a tail at the week end;
				// the circular join below reconnects them.
				spans = append(spans, span{0, en - week}, span{st, week})
			} else {
				spans = append(spans, span{st, en})
			}
		}
	}
	if len(spans) == 0 {
		return nil
	}

	slices.SortFunc(spans, func(a, b span) int {
		if a.start != b.start {
			return cmp.Compare(a.start, b.start)
		}
		return cmp.Compare(a.end, b.end)
	})
	// Merge overlapping/adjacent spans (next.start <= cur.end joins them).
	merged := spans[:1]
	for _, x := range spans[1:] {
		last := &merged[len(merged)-1]
		if x.start <= last.end {
			if x.end > last.end {
				last.end = x.end
			}
		} else {
			merged = append(merged, x)
		}
	}

	// Circular join: a span touching the week end (…, week] continues into one
	// starting at 0 — a single occurrence that wraps. Anchor it at its true
	// (late-week) start so the gap and duration both see one occurrence, not two.
	n := len(merged)
	if n > 1 && merged[0].start == 0 && merged[n-1].end == week {
		occs := []occurrence{{start: merged[n-1].start, length: (week - merged[n-1].start) + merged[0].end}}
		for i := 1; i < n-1; i++ {
			occs = append(occs, occurrence{merged[i].start, merged[i].end - merged[i].start})
		}
		return occs
	}
	occs := make([]occurrence, len(merged))
	for i, m := range merged {
		occs[i] = occurrence{m.start, m.end - m.start}
	}
	return occs
}

// WorstCasePeriod returns P: the largest gap between the start of one effective
// window occurrence and the start of the next, over the recurring weekly cycle
// and across the union of all entries (and all timezones). Occurrences are the
// merged union (spec §3.1), so adjacent/overlapping entries count as one
// occurrence — their internal entry starts are not separate occurrences. The
// second return is false when the schedule has no occurrences.
func (s *Schedule) WorstCasePeriod() (time.Duration, bool) {
	occs := s.mergedOccurrences()
	if len(occs) == 0 {
		return 0, false
	}
	// A continuously-open union is a single occurrence spanning the whole week
	// (overlapping/adjacent spans always merge into one, so full coverage ⟹ one
	// week-long occurrence). There is no gap between rotation opportunities, so the
	// 7d week-wrap the generic logic below would compute is spurious; collapse P to
	// the reconcile-tick granularity (issue #62).
	if len(occs) == 1 && occs[0].length >= week {
		return continuousWindowPeriod, true
	}
	starts := make([]time.Duration, len(occs))
	for i, o := range occs {
		starts[i] = o.start
	}
	slices.Sort(starts)

	// The cycle is weekly: the wrap gap (first start one week later, minus the
	// last) measures the span across the week boundary.
	var maxGap time.Duration
	for i := 1; i < len(starts); i++ {
		if g := starts[i] - starts[i-1]; g > maxGap {
			maxGap = g
		}
	}
	if wrap := (starts[0] + week) - starts[len(starts)-1]; wrap > maxGap {
		maxGap = wrap
	}
	return maxGap, true
}

// ShortestWindow returns D: the representative window-occurrence duration fed to
// the layer-2 throughput check (spec §3.2). It is the duration of the shortest
// occurrence of the **effective** maintenance window — the union of all entries
// (§3.1) — so adjacent or overlapping entries that form one continuous window are
// counted as a single occurrence, not split into raw entry pieces. The shortest
// occurrence is the conservative worst case (a shorter D fits fewer rotations,
// C = floor(D/(t_rot+cooldown))) without that false pessimism. The second return
// is false when the schedule has no occurrences.
func (s *Schedule) ShortestWindow() (time.Duration, bool) {
	occs := s.mergedOccurrences()
	if len(occs) == 0 {
		return 0, false
	}
	shortest := occs[0].length
	for _, o := range occs[1:] {
		if o.length < shortest {
			shortest = o.length
		}
	}
	return shortest, true
}

// startOffset projects one (weekday, start-time) occurrence in the entry's tz
// onto the canonical weekly timeline as a duration in [0, week) since the anchor
// Monday 00:00 UTC.
func (e Entry) startOffset(d time.Weekday) time.Duration {
	// Days from the anchor Monday to weekday d (Monday=0 .. Sunday=6).
	dayOff := (int(d) - int(time.Monday) + 7) % 7
	y, mo, dd := anchorMonday.Date()
	occ := time.Date(y, mo, dd+dayOff, e.StartMin/60, e.StartMin%60, 0, 0, e.Loc)
	off := occ.Sub(anchorMonday) % week
	if off < 0 {
		off += week
	}
	return off
}
