package dashboard

import (
	"testing"
	"time"
)

func TestStoreAggregatesTotals(t *testing.T) {
	s := NewStore()
	s.Record("****aaaa", "deepseek/deepseek-v4-pro", 100, 20)
	s.Record("****aaaa", "deepseek/deepseek-v4-pro", 50, 10)
	s.Record("****bbbb", "zai-org/GLM-5", 30, 5)

	snap := s.Snapshot(Range24h)
	if snap.Totals.Requests != 3 {
		t.Errorf("expected 3 requests, got %d", snap.Totals.Requests)
	}
	if snap.Totals.Input != 180 {
		t.Errorf("expected 180 input, got %d", snap.Totals.Input)
	}
	if snap.Totals.Output != 35 {
		t.Errorf("expected 35 output, got %d", snap.Totals.Output)
	}
	if snap.Totals.Total != 215 {
		t.Errorf("expected 215 total, got %d", snap.Totals.Total)
	}
}

func TestStorePerKeyBreakdown(t *testing.T) {
	s := NewStore()
	s.Record("****aaaa", "m1", 100, 1)
	s.Record("****aaaa", "m1", 100, 1)
	s.Record("****bbbb", "m2", 5, 5)

	snap := s.Snapshot(Range24h)
	if len(snap.ByKey) != 2 {
		t.Fatalf("expected 2 accounts, got %d (%+v)", len(snap.ByKey), snap.ByKey)
	}
	// Sorted by total tokens descending.
	if snap.ByKey[0].Key != "****aaaa" {
		t.Errorf("expected ****aaaa first, got %q", snap.ByKey[0].Key)
	}
	if snap.ByKey[0].Totals.Total != 202 {
		t.Errorf("expected 202 for ****aaaa, got %d", snap.ByKey[0].Totals.Total)
	}
	if snap.ByKey[1].Totals.Total != 10 {
		t.Errorf("expected 10 for ****bbbb, got %d", snap.ByKey[1].Totals.Total)
	}
}

func TestStorePerModelBreakdown(t *testing.T) {
	s := NewStore()
	s.Record("****aaaa", "deepseek/deepseek-v4-pro", 10, 2)
	s.Record("****bbbb", "zai-org/GLM-5", 7, 3)

	snap := s.Snapshot(Range24h)
	if len(snap.ByModel) != 2 {
		t.Fatalf("expected 2 models, got %d", len(snap.ByModel))
	}
	seen := map[string]int{}
	for _, m := range snap.ByModel {
		seen[m.Key] = m.Totals.Total
	}
	if seen["deepseek/deepseek-v4-pro"] != 12 || seen["zai-org/GLM-5"] != 10 {
		t.Errorf("unexpected model totals: %+v", seen)
	}
}

func TestStoreBucketsCoverRange(t *testing.T) {
	s := NewStore()
	s.Record("****aaaa", "m", 10, 1)

	for _, r := range []string{Range24h, Range7d, Range30d} {
		snap := s.Snapshot(r)
		if len(snap.Buckets) == 0 {
			t.Fatalf("range %s produced no buckets", r)
		}
		if snap.Buckets[0].Label == "" {
			t.Errorf("range %s bucket has no label", r)
		}
	}
	if got := len(s.Snapshot(Range24h).Buckets); got != 24 {
		t.Errorf("24h should bucket hourly (24), got %d", got)
	}
	if got := len(s.Snapshot(Range7d).Buckets); got != 7 {
		t.Errorf("7d should bucket daily (7), got %d", got)
	}
	if got := len(s.Snapshot(Range30d).Buckets); got != 30 {
		t.Errorf("30d should bucket daily (30), got %d", got)
	}
}

func TestStoreIgnoresZeroUsage(t *testing.T) {
	s := NewStore()
	s.Record("****aaaa", "m", 0, 0)
	if snap := s.Snapshot(Range24h); snap.Totals.Requests != 0 {
		t.Errorf("zero-usage events should not be recorded, got %d", snap.Totals.Requests)
	}
}

func TestStoreRingEviction(t *testing.T) {
	s := NewStore()
	total := maxEvents + 25
	for i := 0; i < total; i++ {
		s.Record("****aaaa", "m", 1, 0)
	}
	snap := s.Snapshot(Range24h)
	// Only the last maxEvents survive; older ones were overwritten.
	if snap.Totals.Input > maxEvents {
		t.Errorf("ring buffer exceeded capacity: %d events counted", snap.Totals.Input)
	}
	if snap.Totals.Requests != maxEvents {
		t.Errorf("expected %d live events, got %d", maxEvents, snap.Totals.Requests)
	}
}

func TestStoreSnapshotIgnoresForeignRange(t *testing.T) {
	s := NewStore()
	s.Record("****aaaa", "m", 5, 5)
	if snap := s.Snapshot("nonsense"); snap.Range != Range24h {
		t.Errorf("unknown range should fall back to 24h, got %q", snap.Range)
	}
}

func TestStoreOldEventsExcludedFrom24h(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.push(Event{At: now.Add(-48 * time.Hour), Key: "old", Input: 1000, Output: 1000})
	s.push(Event{At: now.Add(-time.Minute), Key: "new", Input: 5, Output: 5})

	snap := s.Snapshot(Range24h)
	if snap.Totals.Total != 10 {
		t.Errorf("24h should exclude the 48h-old event, got total %d", snap.Totals.Total)
	}
	snap30 := s.Snapshot(Range30d)
	if snap30.Totals.Total != 2010 {
		t.Errorf("30d should include both events, got total %d", snap30.Totals.Total)
	}
}
