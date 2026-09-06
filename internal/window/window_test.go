package window

import (
	"testing"
	"time"

	"github.com/AkashiSN/node-rotation-controller/internal/policy"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// tokyoWedSat is the §3.2 worked-example window: {Wed,Sat} 02:00–06:00 JST.
func tokyoWedSat() []policy.MaintenanceWindow {
	return []policy.MaintenanceWindow{{
		Timezone: "Asia/Tokyo",
		Days:     []string{"Wed", "Sat"},
		Start:    "02:00",
		End:      "06:00",
	}}
}

func newSchedule(t *testing.T, ws []policy.MaintenanceWindow) *Schedule {
	t.Helper()
	s, err := New(ws)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestInWindowBoundaries(t *testing.T) {
	jst := mustLoad(t, "Asia/Tokyo")
	s := newSchedule(t, tokyoWedSat())

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"inside Wed 03:00", time.Date(2024, 1, 3, 3, 0, 0, 0, jst), true},
		{"start inclusive Wed 02:00", time.Date(2024, 1, 3, 2, 0, 0, 0, jst), true},
		{"end exclusive Wed 06:00", time.Date(2024, 1, 3, 6, 0, 0, 0, jst), false},
		{"before Wed 01:59", time.Date(2024, 1, 3, 1, 59, 0, 0, jst), false},
		{"wrong day Tue 03:00", time.Date(2024, 1, 2, 3, 0, 0, 0, jst), false},
		{"other day Sat 03:00", time.Date(2024, 1, 6, 3, 0, 0, 0, jst), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.InWindow(tt.now); got != tt.want {
				t.Errorf("InWindow(%s) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

// TestInWindowPerEntryTimezone proves each entry is evaluated in its own tz: the
// same instant is Wed 03:00 in Asia/Tokyo but Tue 18:00 in UTC.
func TestInWindowPerEntryTimezone(t *testing.T) {
	jst := mustLoad(t, "Asia/Tokyo")
	now := time.Date(2024, 1, 3, 3, 0, 0, 0, jst) // Wed 03:00 JST == Tue 18:00 UTC

	tokyo := newSchedule(t, tokyoWedSat())
	if !tokyo.InWindow(now) {
		t.Error("Asia/Tokyo entry should match Wed 03:00 JST")
	}

	utc := newSchedule(t, []policy.MaintenanceWindow{{
		Timezone: "UTC",
		Days:     []string{"Wed"},
		Start:    "02:00",
		End:      "06:00",
	}})
	if utc.InWindow(now) {
		t.Error("UTC entry must not match (instant is Tue 18:00 UTC)")
	}
}

func TestInWindowUnion(t *testing.T) {
	jst := mustLoad(t, "Asia/Tokyo")
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "Asia/Tokyo", Days: []string{"Mon"}, Start: "09:00", End: "17:00"},
		{Timezone: "Asia/Tokyo", Days: []string{"Sat"}, Start: "02:00", End: "06:00"},
	})

	if !s.InWindow(time.Date(2024, 1, 1, 10, 0, 0, 0, jst)) { // Mon 10:00 → first entry
		t.Error("Mon 10:00 should be in union")
	}
	if !s.InWindow(time.Date(2024, 1, 6, 3, 0, 0, 0, jst)) { // Sat 03:00 → second entry
		t.Error("Sat 03:00 should be in union")
	}
	if s.InWindow(time.Date(2024, 1, 3, 3, 0, 0, 0, jst)) { // Wed 03:00 → neither
		t.Error("Wed 03:00 should not be in union")
	}
}

// TestInWindowMultiTimezoneUnion proves the union spans entries in DIFFERENT
// timezones (TestInWindowUnion only covers single-tz unions): one instant can
// satisfy a UTC entry, another a JST entry, and an instant matching neither is
// out. The instant 2024-01-03 18:00 UTC is Wed 18:00 UTC and simultaneously Thu
// 03:00 JST, so it matches the JST entry but not the UTC one.
func TestInWindowMultiTimezoneUnion(t *testing.T) {
	jst := mustLoad(t, "Asia/Tokyo")
	utc := mustLoad(t, "UTC")
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "UTC", Days: []string{"Wed"}, Start: "09:00", End: "12:00"},
		{Timezone: "Asia/Tokyo", Days: []string{"Thu"}, Start: "02:00", End: "06:00"},
	})

	if !s.InWindow(time.Date(2024, 1, 3, 10, 0, 0, 0, utc)) { // Wed 10:00 UTC → UTC entry
		t.Error("Wed 10:00 UTC should match the UTC entry")
	}
	if !s.InWindow(time.Date(2024, 1, 4, 3, 0, 0, 0, jst)) { // Thu 03:00 JST → JST entry
		t.Error("Thu 03:00 JST should match the JST entry")
	}
	// Wed 18:00 UTC == Thu 03:00 JST: the JST entry matches even though the UTC
	// entry (Wed 09:00–12:00) does not at that wall-clock hour.
	if !s.InWindow(time.Date(2024, 1, 3, 18, 0, 0, 0, utc)) {
		t.Error("Wed 18:00 UTC (== Thu 03:00 JST) should match the JST entry via the union")
	}
	// Tue 10:00 UTC (== Tue 19:00 JST) matches neither entry's day.
	if s.InWindow(time.Date(2024, 1, 2, 10, 0, 0, 0, utc)) {
		t.Error("Tue 10:00 UTC should match neither entry")
	}
}

// TestStartOffsetDSTPinnedToAnchorWeek documents the §3.1 DST wall-clock
// approximation for startOffset: every occurrence is projected onto the
// anchor-Monday (2024-01-01, a winter EST week) timeline, so a summer-DST (EDT)
// occurrence and a winter (EST) occurrence of the same wall-clock start land on
// the SAME canonical offset — the projection uses the anchor week's UTC offset
// (EST, UTC−5) for both. NY Wed 02:00 → 50h (UTC Wed 02:00) + 5h = 55h.
func TestStartOffsetDSTPinnedToAnchorWeek(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	e := Entry{Loc: ny, Days: []time.Weekday{time.Wednesday}, StartMin: 2 * 60, EndMin: 6 * 60}

	if got, want := e.startOffset(time.Wednesday), 55*time.Hour; got != want {
		t.Errorf("startOffset(Wed 02:00 America/New_York) = %v, want %v (anchored to the EST week)", got, want)
	}
	// A UTC entry at the same wall-clock start is 5h earlier on the timeline,
	// confirming the EST (not EDT) offset is what gets baked in.
	utc := Entry{Loc: time.UTC, Days: []time.Weekday{time.Wednesday}, StartMin: 2 * 60, EndMin: 6 * 60}
	if got, want := utc.startOffset(time.Wednesday), 50*time.Hour; got != want {
		t.Errorf("startOffset(Wed 02:00 UTC) = %v, want %v", got, want)
	}
}

// TestInWindowAcrossDSTTransitions verifies InWindow stays a correct wall-clock
// membership test across spring-forward and fall-back instants (the §3.1 ±1h
// approximation) without crashing. Because membership is evaluated on the local
// wall clock, the repeated fall-back hour reads as in-window on BOTH passes and
// the skipped spring-forward hour simply never occurs.
func TestInWindowAcrossDSTTransitions(t *testing.T) {
	ny := mustLoad(t, "America/New_York")

	// Spring-forward 2024-03-10: clocks jump 02:00 → 03:00 EDT. Window Sun
	// 03:00–05:00 NY. 03:30 EDT exists and is in-window.
	springWin := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "America/New_York", Days: []string{"Sun"}, Start: "03:00", End: "05:00"},
	})
	if !springWin.InWindow(time.Date(2024, 3, 10, 3, 30, 0, 0, ny)) {
		t.Error("spring-forward 03:30 EDT should be in the Sun 03:00–05:00 window")
	}
	// 02:30 NY does not exist on spring-forward; Go normalizes it (to 01:30 EST),
	// which falls outside the window — no crash, sane membership.
	if springWin.InWindow(time.Date(2024, 3, 10, 2, 30, 0, 0, ny)) {
		t.Error("the skipped spring-forward 02:30 must not read as in-window")
	}

	// Fall-back 2024-11-03: clocks fall 02:00 → 01:00 EST, so 01:30 wall time
	// occurs twice. Window Sun 01:00–02:00 NY. Both 01:30 instances are in-window.
	fallWin := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "America/New_York", Days: []string{"Sun"}, Start: "01:00", End: "02:00"},
	})
	firstEDT := time.Date(2024, 11, 3, 5, 30, 0, 0, time.UTC)  // 01:30 EDT (first pass)
	secondEST := time.Date(2024, 11, 3, 6, 30, 0, 0, time.UTC) // 01:30 EST (second pass)
	if !fallWin.InWindow(firstEDT) {
		t.Error("fall-back 01:30 EDT (first pass) should be in window")
	}
	if !fallWin.InWindow(secondEST) {
		t.Error("fall-back 01:30 EST (second/repeated pass) should also be in window (wall-clock approx)")
	}
	if fallWin.InWindow(time.Date(2024, 11, 3, 8, 0, 0, 0, time.UTC)) { // 03:00 EST
		t.Error("fall-back 03:00 EST is outside the Sun 01:00–02:00 window")
	}
}

func TestWorstCasePeriod(t *testing.T) {
	tests := []struct {
		name string
		ws   []policy.MaintenanceWindow
		want time.Duration
		ok   bool
	}{
		{
			name: "worked example Wed,Sat",
			ws:   tokyoWedSat(),
			want: 96 * time.Hour, // Sat→Wed = 4d, the largest gap
			ok:   true,
		},
		{
			name: "weekly only Sat",
			ws: []policy.MaintenanceWindow{{
				Timezone: "Asia/Tokyo", Days: []string{"Sat"}, Start: "02:00", End: "06:00",
			}},
			want: 168 * time.Hour, // a single weekly occurrence → 7d
			ok:   true,
		},
		{
			name: "Mon Wed Fri",
			ws: []policy.MaintenanceWindow{{
				Timezone: "UTC", Days: []string{"Mon", "Wed", "Fri"}, Start: "00:00", End: "01:00",
			}},
			want: 72 * time.Hour, // Fri→Mon = 3d is the largest gap
			ok:   true,
		},
		{
			// Cross-tz projection: UTC Wed 02:00 → offset 50h; Asia/Tokyo Wed
			// 02:00 → offset 41h (== Tue 17:00 UTC). Sorted {41h,50h}; the wrap
			// gap 41h→(50h prev week) dominates: 168h-9h = 159h.
			name: "cross timezone",
			ws: []policy.MaintenanceWindow{
				{Timezone: "UTC", Days: []string{"Wed"}, Start: "02:00", End: "06:00"},
				{Timezone: "Asia/Tokyo", Days: []string{"Wed"}, Start: "02:00", End: "06:00"},
			},
			want: 159 * time.Hour,
			ok:   true,
		},
		{
			// Adjacent entries are ONE effective occurrence (§3.1 union): their
			// internal 02:00 start is not a separate occurrence, so the only start
			// is Mon 00:00 and P is the full weekly cycle, not 6d22h.
			name: "adjacent entries are one weekly occurrence",
			ws: []policy.MaintenanceWindow{
				{Timezone: "UTC", Days: []string{"Mon"}, Start: "00:00", End: "02:00"},
				{Timezone: "UTC", Days: []string{"Mon"}, Start: "02:00", End: "06:00"},
			},
			want: 168 * time.Hour, // 7d
			ok:   true,
		},
		{
			// A single occurrence that wraps the week boundary on the canonical UTC
			// timeline (Asia/Tokyo Mon 06:00–10:00 = Sun 21:00 UTC + 4h, crossing
			// the Monday-midnight wrap). The circular join reassembles it into ONE
			// occurrence anchored at its true (late-week) start, so there is exactly
			// one rotation opportunity per weekly cycle ⇒ P = the full 7d, not a
			// spurious split (ShortestWindow covers the duration of this same wrap).
			name: "single occurrence wraps the week boundary",
			ws: []policy.MaintenanceWindow{
				{Timezone: "Asia/Tokyo", Days: []string{"Mon"}, Start: "06:00", End: "10:00"},
			},
			want: 168 * time.Hour, // 7d — one occurrence per week
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSchedule(t, tt.ws)
			got, ok := s.WorstCasePeriod()
			if ok != tt.ok {
				t.Fatalf("WorstCasePeriod ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("WorstCasePeriod = %v, want %v", got, tt.want)
			}
		})
	}
}

// allWeekdays is Sunday..Saturday, for building a full-week window.
func allWeekdays() []time.Weekday {
	return []time.Weekday{
		time.Sunday, time.Monday, time.Tuesday, time.Wednesday,
		time.Thursday, time.Friday, time.Saturday,
	}
}

// TestWorstCasePeriodFullWeekIsNotSevenDays covers the issue #62 regression: a
// continuously-open (24/7) union must not report P = 7d (a spurious week wrap)
// and must not report P = 0 (which would surface as a NoWindows fatal, §3.2). The
// entry is built directly with a full 24h day (EndMin = 1440) so the per-day
// spans abut with no midnight gap and merge into one week-long occurrence — the
// shape a real cross-timezone full-week union produces.
func TestWorstCasePeriodFullWeekIsNotSevenDays(t *testing.T) {
	s := &Schedule{entries: []Entry{{
		Loc: time.UTC, Days: allWeekdays(), StartMin: 0, EndMin: 24 * 60,
	}}}
	got, ok := s.WorstCasePeriod()
	if !ok {
		t.Fatal("full-week schedule must have a worst-case period")
	}
	if got == 7*24*time.Hour {
		t.Error("full-week union must not report the spurious 7d week wrap")
	}
	if got <= 0 {
		t.Errorf("full-week P must be positive (a zero P trips a NoWindows fatal); got %v", got)
	}
	if got != continuousWindowPeriod {
		t.Errorf("full-week P = %v, want continuousWindowPeriod %v", got, continuousWindowPeriod)
	}
	// D is unchanged: the window is genuinely open the whole week.
	if d, ok := s.ShortestWindow(); !ok || d != week {
		t.Errorf("full-week D = %v (ok=%v), want %v", d, ok, week)
	}
}

// TestWorstCasePeriodFullWeekViaMergedSpans builds the full week from two tiling
// entries (00:00–12:00 and 12:00–24:00 every day) that merge into a single
// week-long occurrence, exercising the merge path rather than a single span.
func TestWorstCasePeriodFullWeekViaMergedSpans(t *testing.T) {
	s := &Schedule{entries: []Entry{
		{Loc: time.UTC, Days: allWeekdays(), StartMin: 0, EndMin: 12 * 60},
		{Loc: time.UTC, Days: allWeekdays(), StartMin: 12 * 60, EndMin: 24 * 60},
	}}
	got, ok := s.WorstCasePeriod()
	if !ok || got != continuousWindowPeriod {
		t.Errorf("tiled full-week P = %v (ok=%v), want %v", got, ok, continuousWindowPeriod)
	}
}

func TestWorstCasePeriodEmpty(t *testing.T) {
	s := newSchedule(t, nil)
	if got, ok := s.WorstCasePeriod(); ok {
		t.Errorf("WorstCasePeriod on empty schedule = (%v, %v), want (0, false)", got, ok)
	}
	if s.InWindow(time.Now()) {
		t.Error("empty schedule InWindow should be false")
	}
}

// TestShortestWindow covers the representative window length D fed to the
// schedule's layer-2 throughput check (§3.2): the shortest occurrence of the
// effective window union, the conservative worst case (a shorter D fits fewer
// rotations). Overlapping/adjacent entries are merged into one occurrence.
func TestShortestWindow(t *testing.T) {
	tests := []struct {
		name string
		ws   []policy.MaintenanceWindow
		want time.Duration
		ok   bool
	}{
		{
			name: "single occurrence",
			ws:   tokyoWedSat(), // 02:00–06:00
			want: 4 * time.Hour,
			ok:   true,
		},
		{
			name: "shortest of several entries",
			ws: []policy.MaintenanceWindow{
				{Timezone: "UTC", Days: []string{"Wed"}, Start: "02:00", End: "06:00"}, // 4h
				{Timezone: "UTC", Days: []string{"Sat"}, Start: "01:00", End: "02:30"}, // 1h30m — the min
			},
			want: 90 * time.Minute,
			ok:   true,
		},
		{
			name: "all-week long window",
			ws: []policy.MaintenanceWindow{{
				Timezone: "UTC",
				Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
				Start:    "00:00",
				End:      "23:59",
			}},
			want: 23*time.Hour + 59*time.Minute,
			ok:   true,
		},
		{
			// Adjacent entries form one effective occurrence (§3.1 union):
			// Mon 00:00–02:00 + Mon 02:00–06:00 = a single 6h window, not 2h.
			name: "adjacent entries merge",
			ws: []policy.MaintenanceWindow{
				{Timezone: "UTC", Days: []string{"Mon"}, Start: "00:00", End: "02:00"},
				{Timezone: "UTC", Days: []string{"Mon"}, Start: "02:00", End: "06:00"},
			},
			want: 6 * time.Hour,
			ok:   true,
		},
		{
			// Overlapping entries merge to their span: Wed 01:00–04:00 ∪
			// Wed 03:00–06:00 = 01:00–06:00 = 5h.
			name: "overlapping entries merge",
			ws: []policy.MaintenanceWindow{
				{Timezone: "UTC", Days: []string{"Wed"}, Start: "01:00", End: "04:00"},
				{Timezone: "UTC", Days: []string{"Wed"}, Start: "03:00", End: "06:00"},
			},
			want: 5 * time.Hour,
			ok:   true,
		},
		{
			// A single occurrence that crosses the week boundary on the canonical
			// UTC timeline (Asia/Tokyo Mon 06:00–10:00 = Sun 21:00–Mon 01:00 UTC)
			// must read as one 4h window, not its 3h/1h split halves.
			name: "occurrence wraps the week boundary",
			ws: []policy.MaintenanceWindow{
				{Timezone: "Asia/Tokyo", Days: []string{"Mon"}, Start: "06:00", End: "10:00"},
			},
			want: 4 * time.Hour,
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSchedule(t, tt.ws)
			got, ok := s.ShortestWindow()
			if ok != tt.ok {
				t.Fatalf("ShortestWindow ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("ShortestWindow = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShortestWindowEmpty(t *testing.T) {
	s := newSchedule(t, nil)
	if got, ok := s.ShortestWindow(); ok {
		t.Errorf("ShortestWindow on empty schedule = (%v, %v), want (0, false)", got, ok)
	}
}

// TestShortestIdleGap covers the closed interval between consecutive occurrences
// of the effective window union — the quantity the layer-2 carry-over check
// compares t_rot against (§3.2, issue #211). Occurrences are merged first, so
// adjacent entries share one gap rather than manufacturing a zero-length one.
func TestShortestIdleGap(t *testing.T) {
	tests := []struct {
		name string
		ws   []policy.MaintenanceWindow
		want time.Duration
		ok   bool
	}{
		{
			// {Wed,Sat} 02:00–06:00: gaps between occurrence starts are 3d and 4d,
			// so the closed intervals are 3d−4h = 68h and 4d−4h = 92h.
			name: "worked example",
			ws:   tokyoWedSat(),
			want: 68 * time.Hour,
			ok:   true,
		},
		{
			// The issue #211 reproduction: consecutive days, 90m each. Sat 03:30 →
			// Sun 02:00 is 22h30m closed; Sun 03:30 → Sat 02:00 is 6d−1h30m.
			name: "adjacent days leave a short gap",
			ws: []policy.MaintenanceWindow{
				{Timezone: "Asia/Tokyo", Days: []string{"Sat", "Sun"}, Start: "02:00", End: "03:30"},
			},
			want: 22*time.Hour + 30*time.Minute,
			ok:   true,
		},
		{
			// A single weekly occurrence: the only gap is the week wrap.
			name: "single weekly occurrence wraps",
			ws: []policy.MaintenanceWindow{
				{Timezone: "UTC", Days: []string{"Sat"}, Start: "02:00", End: "06:00"},
			},
			want: week - 4*time.Hour,
			ok:   true,
		},
		{
			// Daily 00:00–23:59 leaves a one-minute closed interval each midnight.
			name: "daily near-continuous leaves one minute",
			ws: []policy.MaintenanceWindow{{
				Timezone: "UTC",
				Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
				Start:    "00:00",
				End:      "23:59",
			}},
			want: time.Minute,
			ok:   true,
		},
		{
			// Adjacent entries merge into one occurrence (§3.1 union), so the gap is
			// the week wrap around the merged 6h window — never the 0 the raw
			// entry boundary at 02:00 would suggest.
			name: "adjacent entries merge into one occurrence",
			ws: []policy.MaintenanceWindow{
				{Timezone: "UTC", Days: []string{"Mon"}, Start: "00:00", End: "02:00"},
				{Timezone: "UTC", Days: []string{"Mon"}, Start: "02:00", End: "06:00"},
			},
			want: week - 6*time.Hour,
			ok:   true,
		},
		{
			// The occurrence crosses the canonical week boundary (Asia/Tokyo Mon
			// 06:00–10:00 = Sun 21:00–Mon 01:00 UTC): the wrap gap must be measured
			// from its true end, giving week−4h, not a negative or split value.
			name: "occurrence wraps the week boundary",
			ws: []policy.MaintenanceWindow{
				{Timezone: "Asia/Tokyo", Days: []string{"Mon"}, Start: "06:00", End: "10:00"},
			},
			want: week - 4*time.Hour,
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSchedule(t, tt.ws)
			got, ok := s.ShortestIdleGap()
			if ok != tt.ok {
				t.Fatalf("ShortestIdleGap ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("ShortestIdleGap = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestShortestIdleGapWrappedAmongSeveral exercises the sort-by-start path: the
// wrapped occurrence is returned first by mergedOccurrences but is chronologically
// last, so an unsorted scan would pair the wrong neighbours. Asia/Tokyo Mon
// 06:00–10:00 is Sun 21:00–Mon 01:00 UTC; the Wed and Fri UTC occurrences sit
// between. The shortest closed interval is Wed 06:00 → Fri 02:00 = 1d20h.
func TestShortestIdleGapWrappedAmongSeveral(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "Asia/Tokyo", Days: []string{"Mon"}, Start: "06:00", End: "10:00"},
		{Timezone: "UTC", Days: []string{"Wed"}, Start: "02:00", End: "06:00"},
		{Timezone: "UTC", Days: []string{"Fri"}, Start: "02:00", End: "06:00"},
	})
	got, ok := s.ShortestIdleGap()
	if !ok {
		t.Fatal("ShortestIdleGap ok = false, want true")
	}
	if want := 44 * time.Hour; got != want { // Wed 06:00 → Fri 02:00
		t.Errorf("ShortestIdleGap = %v, want %v", got, want)
	}
}

// TestShortestIdleGapContinuous: a continuously-open union never closes, so no
// rotation can carry over "into the next occurrence" — there is only one. The
// gap is undefined and the carry-over check must be skipped, mirroring the
// WorstCasePeriod special case (issue #62).
func TestShortestIdleGapContinuous(t *testing.T) {
	s := &Schedule{entries: []Entry{{
		Loc: time.UTC, Days: allWeekdays(), StartMin: 0, EndMin: 24 * 60,
	}}}
	if got, ok := s.ShortestIdleGap(); ok {
		t.Errorf("ShortestIdleGap on a continuous window = (%v, %v), want (0, false)", got, ok)
	}
}

func TestShortestIdleGapEmpty(t *testing.T) {
	s := newSchedule(t, nil)
	if got, ok := s.ShortestIdleGap(); ok {
		t.Errorf("ShortestIdleGap on empty schedule = (%v, %v), want (0, false)", got, ok)
	}
}

// The one-minute coarse walk in OccurrenceBounds is only complete when every
// membership boundary lands on a whole UTC minute, which holds exactly when every
// zone offset in effect and every zone transition is minute-aligned. Historical
// IANA offsets break it: Africa/Monrovia ran at -00:44:30 until 1972-01-07, so a
// Monrovia entry's boundaries fall 30 seconds off the minute lattice and a union
// containing one can have a sub-minute gap the walk would step over.
func TestMinuteAlignedZonesRejectsSubMinuteOffsets(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "Africa/Monrovia", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "00:00", End: "12:00"},
	})
	dirty := time.Date(1971, 6, 1, 12, 0, 0, 0, time.UTC)
	if s.minuteAlignedZones(dirty, dirty.Add(24*time.Hour)) {
		t.Error("Monrovia at -00:44:30 must be rejected")
	}
	clean := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if !s.minuteAlignedZones(clean, clean.Add(7*24*time.Hour)) {
		t.Error("contemporary Monrovia (offset 0) must be accepted")
	}
}

// A DST transition is minute-aligned, so a week containing one must still pass.
// If this fails the audit is over-strict and would disable the clamp for every
// schedule twice a year.
func TestMinuteAlignedZonesAcceptsDSTWeek(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "America/New_York", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "01:00", End: "04:00"},
	})
	// 2026-03-08 is the US spring-forward; 2026-11-01 the fall-back.
	for _, when := range []time.Time{
		time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
	} {
		if !s.minuteAlignedZones(when, when.Add(7*24*time.Hour)) {
			t.Errorf("a week containing a DST transition must be accepted; from %s", when)
		}
	}
}

// The audit must cover the span the search actually walks, which runs BACKWARD
// for the occurrence start as well as forward for its end. Monrovia leaves
// -00:44:30 at 1972-01-07T00:44:30Z, so at this instant the forward week is clean
// while the backward week is not: a forward-only audit passes and the start search
// can then merge across a sub-minute gap.
func TestMinuteAlignedZonesCoversTheBackwardSpan(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "Africa/Monrovia", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "00:00", End: "12:00"},
	})
	now := time.Date(1972, 1, 10, 12, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	if !s.minuteAlignedZones(now, now.Add(week)) {
		t.Fatal("fixture invalid: the forward span must look clean, or this proves nothing")
	}
	if s.minuteAlignedZones(now.Add(-week), now) {
		t.Error("the backward span reaches -00:44:30 and must be rejected")
	}
}

// The ordinary case, and the one that pins exactness: now carries a sub-minute
// phase, so a bracket-only implementation returns bounds offset by that phase and
// passes nothing else in this file. 05:10:37.123456789 must still yield exactly
// [05:00:00, 06:30:00).
func TestOccurrenceBoundsAreExactRegardlessOfSamplingPhase(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "05:00", End: "06:30"},
	})
	now := time.Date(2026, 9, 4, 5, 10, 37, 123456789, time.UTC)
	start, end, ok := s.OccurrenceBounds(now)
	if !ok {
		t.Fatal("want ok")
	}
	wantStart := time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 9, 4, 6, 30, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Errorf("got [%s, %s), want [%s, %s)",
			start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano),
			wantStart.Format(time.RFC3339Nano), wantEnd.Format(time.RFC3339Nano))
	}
}

// Out of window there is no occurrence to report.
func TestOccurrenceBoundsOutOfWindow(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "05:00", End: "06:30"},
	})
	if _, _, ok := s.OccurrenceBounds(time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC)); ok {
		t.Error("want ok=false outside every window")
	}
}

// The union is the effective window (spec §3.1), so two adjacent entries are ONE
// occurrence. 05:00-06:00 followed by 06:00-07:00 must report [05:00, 07:00).
func TestOccurrenceBoundsJoinsAdjacentEntries(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "05:00", End: "06:00"},
		{Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "06:00", End: "07:00"},
	})
	start, end, ok := s.OccurrenceBounds(time.Date(2026, 9, 4, 5, 30, 0, 0, time.UTC))
	if !ok {
		t.Fatal("want ok")
	}
	if want := time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start: got %s, want %s", start, want)
	}
	if want := time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Errorf("end: got %s, want %s", end, want)
	}
}

// A union that genuinely spans midnight needs a SECOND TIMEZONE. ParseHHMM caps
// End at 23:59 and Validate rejects start >= end, so an entry can never include
// the 23:59-00:00 minute; two same-zone entries therefore always leave that minute
// as a gap and never join. Here a Europe/Berlin Saturday entry (UTC+1 in December)
// covers UTC Friday 23:59 onward, so the union runs from Friday 23:00 to Saturday
// 01:00 UTC. Verified before this plan was written: without the Berlin entry the
// occurrence stops at 23:59.
func TestOccurrenceBoundsSpansMidnightAcrossTimezones(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "UTC", Days: []string{"Fri"}, Start: "23:00", End: "23:59"},
		{Timezone: "Europe/Berlin", Days: []string{"Sat"}, Start: "00:59", End: "02:00"},
	})
	// 2026-12-04 is a Friday and Berlin is UTC+1 that month.
	start, end, ok := s.OccurrenceBounds(time.Date(2026, 12, 4, 23, 30, 0, 0, time.UTC))
	if !ok {
		t.Fatal("want ok")
	}
	if want := time.Date(2026, 12, 4, 23, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start: got %s, want %s", start.UTC(), want)
	}
	if want := time.Date(2026, 12, 5, 1, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Errorf("end: got %s, want %s — the union must cross midnight, not stop at 23:59", end.UTC(), want)
	}
}

// Entries in different zones overlap in real time; the union of both is one
// occurrence. Tokyo 14:00-15:00 JST is 05:00-06:00 UTC, so with a UTC 05:30-07:00
// entry the union is [05:00, 07:00) UTC.
func TestOccurrenceBoundsUnionAcrossTimezones(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "Asia/Tokyo", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "14:00", End: "15:00"},
		{Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "05:30", End: "07:00"},
	})
	start, end, ok := s.OccurrenceBounds(time.Date(2026, 9, 4, 5, 45, 0, 0, time.UTC))
	if !ok {
		t.Fatal("want ok")
	}
	if want := time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start: got %s, want %s", start, want)
	}
	if want := time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Errorf("end: got %s, want %s", end, want)
	}
}

// DST, the case the discarded wall-clock construction would have failed — with a
// sub-minute sampling phase on top, so the DST boundary and the refinement are
// exercised together rather than one at a time. An entry
// whose local close falls INSIDE the spring-forward gap has no such local instant;
// the boundary is where membership actually changes, which is the transition.
// New_York normalizes a constructed 02:30 backward to 01:30 EST, Berlin forward to
// 03:30 CEST, so both directions are covered.
func TestOccurrenceBoundsCloseInsideSpringForwardGap(t *testing.T) {
	for _, tc := range []struct {
		name string
		tz   string
		now  time.Time // an in-window instant before the gap
		want time.Time // the real membership boundary
	}{
		{
			// 2026-03-08: 02:00 EST -> 03:00 EDT. Entry 01:00-02:30 local.
			// Membership ends when the local clock leaves [01:00, 02:30): at the
			// transition instant, whose local reading jumps to 03:00.
			name: "america/new_york normalizes backward",
			tz:   "America/New_York",
			now:  time.Date(2026, 3, 8, 6, 45, 37, 123456789, time.UTC), // 01:45:37.123456789 EST
			want: time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC),           // 02:00 EST = 03:00 EDT
		},
		{
			// 2026-03-29: 02:00 CET -> 03:00 CEST. Entry 01:00-02:30 local.
			name: "europe/berlin normalizes forward",
			tz:   "Europe/Berlin",
			now:  time.Date(2026, 3, 29, 0, 45, 37, 123456789, time.UTC), // 01:45:37.123456789 CET
			want: time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),           // 02:00 CET = 03:00 CEST
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSchedule(t, []policy.MaintenanceWindow{
				{Timezone: tc.tz, Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "01:00", End: "02:30"},
			})
			if !s.InWindow(tc.now) {
				t.Fatalf("fixture invalid: %s must be in window", tc.now)
			}
			_, end, ok := s.OccurrenceBounds(tc.now)
			if !ok {
				t.Fatal("want ok")
			}
			if !end.Equal(tc.want) {
				t.Errorf("end: got %s, want %s", end.UTC(), tc.want)
			}
			if !end.After(tc.now) {
				t.Errorf("end %s must be after now %s — a constructed wall-clock close can land in the past", end, tc.now)
			}
		})
	}
}

// The fall-back fold repeats a local hour, so an entry ending inside it is in
// window twice. The bounds must differ between the two copies, as membership does.
func TestOccurrenceBoundsInEachCopyOfAFallBackFold(t *testing.T) {
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "America/New_York", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "01:00", End: "01:45"},
	})
	// 2026-11-01: 02:00 EDT -> 01:00 EST. 01:30 happens twice.
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)  // 01:30 EDT
	second := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // 01:30 EST
	s1, e1, ok1 := s.OccurrenceBounds(first)
	s2, e2, ok2 := s.OccurrenceBounds(second)
	if !ok1 || !ok2 {
		t.Fatalf("want ok for both copies; got %v %v", ok1, ok2)
	}
	if s1.Equal(s2) || e1.Equal(e2) {
		t.Errorf("the two copies of the repeated hour must be different occurrences: [%s,%s) vs [%s,%s)",
			s1.UTC(), e1.UTC(), s2.UTC(), e2.UTC())
	}
}

// The coarse-walk precondition, both halves. Africa/Monrovia at -00:44:30 closes a
// 00:00-12:00 entry at 12:44:30Z; with a UTC 12:45-23:00 entry the union has a
// 30-second gap. now is phased so both coarse samples land in window, so a walk
// without the audit merges the two occurrences and reports one long span.
func TestOccurrenceBoundsRefusesASubMinuteGap(t *testing.T) {
	monroviaUTC := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "Africa/Monrovia", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "00:00", End: "12:00"},
		{Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "12:45", End: "23:00"},
	})
	// Africa/Lagos runs at offset 0 (aligned) until 1908-07-01T00:00:00Z and
	// +00:13:35 (misaligned — 35s past the minute) after. now is phased so the
	// backward span (now-horizon..now) stays entirely before the transition while
	// the forward span (now..now+horizon) crosses into the misaligned segment,
	// isolating the FORWARD half as the only one that can reject.
	lagos := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "Africa/Lagos", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "00:00", End: "12:00"},
	})
	for _, tc := range []struct {
		name string
		s    *Schedule
		now  time.Time
	}{
		// Both halves are inside Monrovia's -00:44:30 segment here, so this does NOT
		// isolate which half of the audit rejects — see "forward half, isolated"
		// below for a variant that does.
		{"before the gap", monroviaUTC, time.Date(1971, 6, 1, 12, 30, 20, 0, time.UTC)},
		// After Monrovia leaves -00:44:30 (1972-01-07T00:44:30Z) the forward span is
		// clean at offset 0 while the backward span still is not: only the BACKWARD
		// half rejects here.
		{"after the segment ends", monroviaUTC, time.Date(1972, 1, 10, 13, 0, 20, 0, time.UTC)},
		// The backward span sits in Lagos's aligned (offset-0) segment while the
		// forward span crosses into the +00:13:35 misaligned one: only the FORWARD
		// half rejects here, pinning the half TestOccurrenceBoundsRefusesASubMinuteGap
		// otherwise never isolates.
		{"forward half, isolated", lagos, time.Date(1908, 6, 28, 6, 0, 20, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.s.InWindow(tc.now) {
				t.Fatalf("fixture invalid: %s must be in window", tc.now)
			}
			if _, _, ok := tc.s.OccurrenceBounds(tc.now); ok {
				t.Error("want ok=false: the minute-alignment precondition does not hold over the searched span")
			}
		})
	}
}

// A union with no boundary inside the horizon reports no clamp. This is a search
// bound, not a claim that the union is continuously open.
func TestOccurrenceBoundsGivesUpAtTheHorizon(t *testing.T) {
	// Two 23:59-long entries in zones nine hours apart cover each other's midnight
	// minute, leaving no gap.
	s := newSchedule(t, []policy.MaintenanceWindow{
		{Timezone: "UTC", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "00:00", End: "23:59"},
		{Timezone: "Asia/Tokyo", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, Start: "00:00", End: "23:59"},
	})
	if _, _, ok := s.OccurrenceBounds(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)); ok {
		t.Error("want ok=false when no boundary is found within the horizon")
	}
}
