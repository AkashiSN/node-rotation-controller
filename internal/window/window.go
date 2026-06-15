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
	"fmt"
	"sort"
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
		for _, d := range e.Days {
			if local.Weekday() == d {
				return true
			}
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

	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
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
