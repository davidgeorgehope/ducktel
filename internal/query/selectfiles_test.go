package query

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// The fixture spans days around a fixed reference "today". selectFiles compares
// dates from the since value alone, so these need not be near the real clock.
const (
	dayBeforeRef = "2030-01-01"
	dayTarget    = "2030-01-02" // three days before dayRef
	dayAfter3    = "2030-01-03"
	dayYesterday = "2030-01-04"
	dayRef       = "2030-01-05"
	dayTomorrow  = "2030-01-06"
)

var fixtureDays = []string{
	dayBeforeRef, dayTarget, dayAfter3, dayYesterday, dayRef, dayTomorrow,
}

// fixtureNames is every file kind a day directory can hold, plus a dot file
// (.tmp-* in-flight writes) and a non-parquet file that must be ignored.
var fixtureNames = []string{
	"00-00.parquet",
	"07-00.parquet",
	"07-00-1.parquet",
	"07.parquet",
	"11-00.parquet",
	"11.parquet",
	"12-00.parquet",
	"12.parquet",
	"23-59.parquet",
	"23.parquet",
	"day.parquet",
	"foo.parquet",
	".tmp-x.parquet",
	"notes.txt",
}

// Every selected file kind in a day directory, with dot and non-parquet names
// removed.
var allDayNames = []string{
	"00-00.parquet",
	"07-00.parquet",
	"07-00-1.parquet",
	"07.parquet",
	"11-00.parquet",
	"11.parquet",
	"12-00.parquet",
	"12.parquet",
	"23-59.parquet",
	"23.parquet",
	"day.parquet",
	"foo.parquet",
}

// Files kept when b's hour is 11: hours at or after 11, the day merge, and the
// unrecognised name.
var namesFromHour11 = []string{
	"11-00.parquet",
	"11.parquet",
	"12-00.parquet",
	"12.parquet",
	"23-59.parquet",
	"23.parquet",
	"day.parquet",
	"foo.parquet",
}

// Files kept when b's hour is 23: only the last hour, the day merge, and the
// unrecognised name.
var namesFromHour23 = []string{
	"23-59.parquet",
	"23.parquet",
	"day.parquet",
	"foo.parquet",
}

func pathsForDays(signalDir string, days, names []string) []string {
	var out []string
	for _, day := range days {
		for _, name := range names {
			out = append(out, filepath.Join(signalDir, day, name))
		}
	}
	return out
}

func writeFixtureFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestSelectFiles(t *testing.T) {
	root := t.TempDir()
	signalDir := filepath.Join(root, "metrics")

	for _, day := range fixtureDays {
		for _, name := range fixtureNames {
			writeFixtureFile(t, filepath.Join(signalDir, day, name))
		}
	}

	// A direct file, a non-date directory (the old glob included it), and an
	// unknown nested directory inside a date-shaped day: all are kept for every
	// bound, because none of them fits the date/hour naming layout.
	rootFile := filepath.Join(signalDir, "root.parquet")
	archiveFile := filepath.Join(signalDir, "archive", "legacy.parquet")
	archiveHidden := filepath.Join(signalDir, "archive", ".tmp-y.parquet")
	archiveNested := filepath.Join(signalDir, "archive", "2030-01-01", "old.parquet")
	nestedInDay := filepath.Join(signalDir, dayTarget, "extra", "keep.parquet")
	for _, path := range []string{rootFile, archiveFile, archiveHidden, archiveNested, nestedInDay} {
		writeFixtureFile(t, path)
	}
	alwaysKept := []string{rootFile, archiveFile, archiveNested}

	// The dayTarget nested file is outside the flat day layout too, so it is
	// kept only when dayTarget itself is not skipped as an old day.

	tests := []struct {
		name  string
		since time.Time
		want  []string
	}{
		{
			name:  "zero since keeps every parquet file",
			since: time.Time{},
			want: append(append([]string{},
				pathsForDays(signalDir, fixtureDays, allDayNames)...),
				append(alwaysKept, nestedInDay)...),
		},
		{
			name:  "since in the middle of today",
			since: time.Date(2030, 1, 5, 12, 20, 0, 0, time.UTC),
			want: append(append([]string{},
				pathsForDays(signalDir, []string{dayRef}, namesFromHour11)...),
				append(pathsForDays(signalDir, []string{dayTomorrow}, allDayNames),
					alwaysKept...)...),
		},
		{
			name:  "since just after midnight reaches yesterday's last hour",
			since: time.Date(2030, 1, 5, 0, 30, 0, 0, time.UTC),
			want: append(append([]string{},
				pathsForDays(signalDir, []string{dayYesterday}, namesFromHour23)...),
				append(pathsForDays(signalDir, []string{dayRef, dayTomorrow}, allDayNames),
					alwaysKept...)...),
		},
		{
			name:  "since three days ago",
			since: time.Date(2030, 1, 2, 12, 20, 0, 0, time.UTC),
			want: append(append([]string{},
				pathsForDays(signalDir, []string{dayTarget}, namesFromHour11)...),
				append(pathsForDays(signalDir, []string{dayAfter3, dayYesterday, dayRef, dayTomorrow}, allDayNames),
					append(alwaysKept, nestedInDay)...)...),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectFiles(signalDir, tc.since)
			if err != nil {
				t.Fatalf("selectFiles: %v", err)
			}
			want := append([]string{}, tc.want...)
			sort.Strings(got)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("selected %d file(s), want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("selected[%d] = %s, want %s", i, got[i], want[i])
				}
			}
		})
	}
}

func TestSelectFilesMissingSignalDir(t *testing.T) {
	files, err := selectFiles(filepath.Join(t.TempDir(), "traces"), time.Time{})
	if err != nil {
		t.Fatalf("missing signal dir should be an empty list, not an error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("files = %v, want none", files)
	}
}
