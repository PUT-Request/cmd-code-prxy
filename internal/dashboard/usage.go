package dashboard

import (
	"database/sql"
	"log"
	"sort"
	"sync"
	"time"
)

// maxEvents bounds the in-memory ring buffer. Usage is a live view, not an
// audit log: at ~64 bytes per event this is a few MB and cannot grow unbounded.
const maxEvents = 50000

// Event is one completed upstream request.
type Event struct {
	At     time.Time
	Key    string
	Model  string
	Input  int
	Output int
}

// Store accumulates usage. Events are written through to the SQLite
// database when one is attached; the ring is a bounded in-memory
// cache so aggregation stays fast. Without a database the store is
// in-memory only.
type Store struct {
	mu     sync.Mutex
	db     *sql.DB
	events []Event
	start  int // next write position
	count  int // number of live events
}

// NewStore returns a store backed by the SQLite database at path.
// An empty path, an unopenable database, or a database with no
// readable history all fall back to in-memory-only behavior.
func NewStore(path string) *Store {
	db, events := initUsageDB(path)
	s := &Store{events: make([]Event, maxEvents), db: db}
	for _, e := range events {
		s.push(e)
	}
	return s
}

// Close releases the underlying database. Safe to call twice and
// on a store with no database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Record adds a completed request. Empty keys are stored as-is; callers pass
// the client key fingerprint, never the raw key.
func (s *Store) Record(key, model string, input, output int) {
	if input <= 0 && output <= 0 {
		return
	}
	s.push(Event{At: time.Now(), Key: key, Model: model, Input: input, Output: output})
}

// push writes an event into the ring and the database. Used by
// Record and by tests that need to inject events at a specific time.
func (s *Store) push(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeDBLocked(e)
	s.events[s.start] = e
	s.start = (s.start + 1) % len(s.events)
	if s.count < len(s.events) {
		s.count++
	}
}

// writeDBLocked persists an event. Errors are logged once and then
// suppressed: a failing database must not break request serving.
func (s *Store) writeDBLocked(e Event) {
	if s.db == nil {
		return
	}
	if _, err := s.db.Exec(`INSERT INTO usage_events
		(at, key_fp, model, input, output) VALUES (?, ?, ?, ?, ?)`,
		e.At.Unix(), e.Key, e.Model, e.Input, e.Output); err != nil {
		log.Printf("[WARN] Could not persist usage event: %v", err)
	}
}

// snapshot returns live events in chronological order.
func (s *Store) snapshot() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, s.count)
	if s.count < len(s.events) {
		out = append(out, s.events[:s.count]...)
	} else {
		out = append(out, s.events[s.start:]...)
		out = append(out, s.events[:s.start]...)
	}
	return out
}

// Totals are aggregate numbers for a time range.
type Totals struct {
	Requests int `json:"requests"`
	Input    int `json:"input_tokens"`
	Output   int `json:"output_tokens"`
	Total    int `json:"total_tokens"`
}

// KeyUsage is per-account usage for a time range.
type KeyUsage struct {
	Key    string `json:"key"`
	Model  string `json:"model,omitempty"`
	Totals Totals `json:"totals"`
}

// Bucket is one point on the usage graph.
type Bucket struct {
	Label    string `json:"label"`
	Input    int    `json:"input_tokens"`
	Output   int    `json:"output_tokens"`
	Total    int    `json:"total_tokens"`
	Requests int    `json:"requests"`
	Totals   Totals `json:"-"`
}

// Snapshot is the full dashboard payload for a range.
type Snapshot struct {
	Range      string     `json:"range"`
	Since      time.Time  `json:"since"`
	Stored     int        `json:"stored_events"`
	Totals     Totals     `json:"totals"`
	Buckets    []Bucket   `json:"buckets"`
	ByKey      []KeyUsage `json:"by_key"`
	ByModel    []KeyUsage `json:"by_model"`
	FirstEvent time.Time  `json:"first_event,omitempty"`
}

// Ranges supported by the dashboard.
const (
	Range24h = "24h"
	Range7d  = "7d"
	Range30d = "30d"
)

func rangeDuration(rangeKey string) time.Duration {
	switch rangeKey {
	case Range7d:
		return 7 * 24 * time.Hour
	case Range30d:
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// bucketWidth returns the graph resolution for a range: hourly up to 48 buckets,
// then daily so the graph stays readable.
func bucketWidth(d time.Duration) time.Duration {
	if d <= 48*time.Hour {
		return time.Hour
	}
	return 24 * time.Hour
}

// normalizeRange maps a request value to a supported range key.
func normalizeRange(rangeKey string) string {
	switch rangeKey {
	case Range7d, Range30d:
		return rangeKey
	default:
		return Range24h
	}
}

// Snapshot aggregates usage for the given range ("24h", "7d", "30d").
func (s *Store) Snapshot(rangeKey string) Snapshot {
	rangeKey = normalizeRange(rangeKey)
	d := rangeDuration(rangeKey)
	now := time.Now()
	since := now.Add(-d)
	width := bucketWidth(d)

	events := s.snapshot()
	live := 0
	var first time.Time
	for _, e := range events {
		if e.At.After(since) {
			live++
			if first.IsZero() || e.At.Before(first) {
				first = e.At
			}
		}
	}

	// Pre-create every bucket so gaps render as zero rather than vanishing.
	count := int(d / width)
	buckets := make([]Bucket, count)
	base := time.Date(since.Year(), since.Month(), since.Day(), since.Hour(), 0, 0, 0, since.Location())
	if width >= 24*time.Hour {
		base = time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, since.Location())
	}
	for i := 0; i < count; i++ {
		at := base.Add(time.Duration(i) * width)
		buckets[i].Label = at.Format("01-02 15:04")
		if width >= 24*time.Hour {
			buckets[i].Label = at.Format("01-02")
		}
	}

	byKey := map[string]*KeyUsage{}
	byModel := map[string]*KeyUsage{}
	var totals Totals

	for _, e := range events {
		if e.At.Before(since) || e.At.After(now) {
			continue
		}
		idx := int(e.At.Sub(base) / width)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(buckets) {
			idx = len(buckets) - 1
		}
		addTotal(&buckets[idx].Totals, e)
		addTotal(&totals, e)

		k := byKey[e.Key]
		if k == nil {
			k = &KeyUsage{Key: e.Key}
			byKey[e.Key] = k
		}
		addTotal(&k.Totals, e)

		m := byModel[e.Model]
		if m == nil {
			m = &KeyUsage{Key: e.Model}
			byModel[e.Model] = m
		}
		addTotal(&m.Totals, e)
	}

	// Flatten bucket totals into the graph shape the chart consumes.
	for i := range buckets {
		buckets[i].Requests = buckets[i].Totals.Requests
		buckets[i].Input = buckets[i].Totals.Input
		buckets[i].Output = buckets[i].Totals.Output
		buckets[i].Total = buckets[i].Totals.Total
	}

	return Snapshot{
		Range:      rangeKey,
		Since:      since,
		Stored:     live,
		Totals:     totals,
		Buckets:    buckets,
		ByKey:      sortedUsage(byKey),
		ByModel:    sortedUsage(byModel),
		FirstEvent: first,
	}
}

func addTotal(t *Totals, e Event) {
	t.Requests++
	t.Input += e.Input
	t.Output += e.Output
	t.Total += e.Input + e.Output
}

func sortedUsage(m map[string]*KeyUsage) []KeyUsage {
	out := make([]KeyUsage, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Totals.Total != out[j].Totals.Total {
			return out[i].Totals.Total > out[j].Totals.Total
		}
		return out[i].Key < out[j].Key
	})
	return out
}
