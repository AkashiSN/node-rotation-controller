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

// WorstCasePeriod returns P: the largest gap between the start of one window
// occurrence and the start of the next, over the recurring weekly cycle and
// across the union of all entries (and all timezones). The second return is
// false when the schedule has no occurrences.
func (s *Schedule) WorstCasePeriod() (time.Duration, bool) {
	var offsets []time.Duration
	for _, e := range s.entries {
		for _, d := range e.Days {
			offsets = append(offsets, e.startOffset(d))
		}
	}
	if len(offsets) == 0 {
		return 0, false
	}

	slices.Sort(offsets)
	// Deduplicate so coincident starts don't create spurious zero gaps.
	uniq := offsets[:1]
	for _, o := range offsets[1:] {
		if o != uniq[len(uniq)-1] {
			uniq = append(uniq, o)
		}
	}

	// The cycle is weekly: append the first occurrence one week later as the
	// wrap sentinel so the gap spanning the week boundary is measured too.
	var maxGap time.Duration
	for i := 1; i < len(uniq); i++ {
		if g := uniq[i] - uniq[i-1]; g > maxGap {
			maxGap = g
		}
	}
	if wrap := (uniq[0] + week) - uniq[len(uniq)-1]; wrap > maxGap {
		maxGap = wrap
	}
	return maxGap, true
}

// ShortestWindow returns D: the representative window-occurrence duration fed to
// the layer-2 throughput check (spec §3.2). It is the duration of the shortest
// occurrence of the **effective** maintenance window — the union of all entries
// (§3.1) — so adjacent or overlapping entries that form one continuous window are
// counted as a single occurrence. The shortest occurrence is the conservative
// worst case (a shorter D fits fewer rotations, C = floor(D/(t_rot+cooldown))),
// without the false pessimism of splitting a contiguous window into raw entry
// pieces. The second return is false when the schedule has no occurrences.
//
// Occurrences are merged on the same canonical weekly timeline WorstCasePeriod
// uses (each entry projected through startOffset, so cross-timezone overlaps are
// handled). An occurrence whose end crosses the week boundary is split and
// rejoined by the circular merge below.
func (s *Schedule) ShortestWindow() (time.Duration, bool) {
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
		return 0, false
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

	// Circular join: a span touching the week end (… , week] continues into a span
	// starting at the week start [0, …) — one occurrence spanning the boundary.
	shortest := time.Duration(-1)
	n := len(merged)
	if n > 1 && merged[0].start == 0 && merged[n-1].end == week {
		shortest = (week - merged[n-1].start) + merged[0].end
		merged = merged[1 : n-1] // the two joined ends are accounted for above
	}
	for _, m := range merged {
		if d := m.end - m.start; shortest < 0 || d < shortest {
			shortest = d
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
