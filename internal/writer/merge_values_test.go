package writer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"
)

// TestMergeKeepsValues writes many per-flush files with high-cardinality
// resource attributes, merges them like the hourly and the daily compactor,
// and checks every row of the merged file against what was written. The
// writers once used parquet.DictionaryMaxBytes: in parquet-go v0.28.0, once a
// column's dictionary passed that cap, the rows after it were written with
// another column's values (span attributes became resource attributes).
func TestMergeKeepsValues(t *testing.T) {
	for _, merge := range []string{"hour", "day"} {
		t.Run(merge, func(t *testing.T) { checkMergeKeepsValues(t, merge) })
	}
}

func checkMergeKeepsValues(t *testing.T, merge string) {
	dir := t.TempDir()
	want := map[string]string{} // key -> resource attributes
	var sources []string
	ts := int64(0)
	for f := 0; f < 60; f++ {
		rows := make([]MetricPoint, 2000)
		for i := range rows {
			ts++
			rows[i] = MetricPoint{
				MetricName: fmt.Sprintf("m%d", i%7), MetricType: "gauge", Timestamp: ts,
				Attributes:         fmt.Sprintf(`{"cpu":"%d"}`, i%8),
				ResourceAttributes: fmt.Sprintf(`{"host.name":"node-%03d","k8s.pod.name":"pod-%d-%d"}`, i%90, f, i),
				ServiceName:        "svc", ScopeName: "node-exporter", ScopeVersion: "1",
			}
			want[fmt.Sprint(ts)] = rows[i].ResourceAttributes
		}
		p := filepath.Join(dir, fmt.Sprintf("10-%02d.parquet", f))
		fh, _ := os.Create(p)
		w := parquet.NewGenericWriter[MetricPoint](fh, writerOptions()...)
		if _, err := w.Write(rows); err != nil {
			t.Fatal(err)
		}
		w.Close()
		fh.Close()
		sources = append(sources, p)
	}
	out, _ := os.Create(filepath.Join(dir, "10.parquet"))
	merger := mergeHourSignal
	if merge == "day" {
		merger = mergeDaySignal
	}
	if err := merger(context.Background(), out, "metrics", sources); err != nil {
		t.Fatal(err)
	}
	out.Close()
	got, err := parquet.ReadFile[MetricPoint](out.Name())
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, r := range got {
		if want[fmt.Sprint(r.Timestamp)] != r.ResourceAttributes || r.ScopeName != "node-exporter" {
			if bad < 2 {
				t.Logf("ts=%d got ra=%q scope=%q", r.Timestamp, r.ResourceAttributes, r.ScopeName)
			}
			bad++
		}
	}
	t.Logf("merged %d rows, bad %d", len(got), bad)
	if bad > 0 || len(got) != len(want) {
		t.Fatalf("%d of %d rows corrupted", bad, len(got))
	}
}
