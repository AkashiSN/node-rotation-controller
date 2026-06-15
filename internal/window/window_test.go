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

func TestWorstCasePeriodEmpty(t *testing.T) {
	s := newSchedule(t, nil)
	if got, ok := s.WorstCasePeriod(); ok {
		t.Errorf("WorstCasePeriod on empty schedule = (%v, %v), want (0, false)", got, ok)
	}
	if s.InWindow(time.Now()) {
		t.Error("empty schedule InWindow should be false")
	}
}
