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
