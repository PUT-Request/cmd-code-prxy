package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

// Dump limits: a single file never exceeds dumpFileMaxBytes (a new part is
// started), and the whole folder is pruned to dumpMaxBytes when configured.
const (
	// dumpFileMaxBytes caps one JSONL file before rotating to a new part.
	dumpFileMaxBytes = 100 << 20 // 100 MB
	// dumpSyncInterval is how often the archive file is fsynced. Per-line
	// appends are already durable enough; the sync batches the tail.
	dumpSyncInterval = 10 * time.Second
	// dumpQueueSize bounds how many records may wait for the writer. On
	// overflow records are dropped (best-effort archive; never block
	// request serving).
	dumpQueueSize = 10000
)

// dumpDataURLRe matches embedded base64 image payloads for stripping.
var dumpDataURLRe = regexp.MustCompile(`data:[a-zA-Z0-9.+-]+/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]+`)

// DumpResponse is the assembled upstream answer, ready for distillation.
type DumpResponse struct {
	Text      string         `json:"text,omitempty"`
	ToolCalls []api.ToolCall `json:"tool_calls,omitempty"`
	Reasoning string         `json:"reasoning,omitempty"`
	Finish    string         `json:"finish,omitempty"`
	Usage     DumpUsage      `json:"usage,omitempty"`
}

// DumpUsage is the upstream-reported token usage.
type DumpUsage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// DumpRecord is one archived request, 1:1 with what crossed the wire.
type DumpRecord struct {
	TS        string           `json:"ts"`
	Client    string           `json:"client"`
	Requested string           `json:"requested"`
	Model     string           `json:"model,omitempty"`
	Stream    bool             `json:"stream"`
	Status    int              `json:"status"`
	Error     string           `json:"error,omitempty"`
	CCRequest *json.RawMessage `json:"cc_request,omitempty"`
	Response  *DumpResponse    `json:"response,omitempty"`
	MS        int64            `json:"ms"`
}

// dumpCapture accumulates the final response while a request streams, so the
// archive gets the assembled answer without re-parsing anything.
type dumpCapture struct {
	Text         string
	ToolCalls    []api.ToolCall
	Reasoning    string
	Finish       string
	InputTokens  int
	OutputTokens int
}

// Dumper archives completed requests as JSONL, one file per day, off the
// request path. Safe for concurrent Record calls. A nil *Dumper is a no-op,
// so an unconfigured proxy needs no branching at call sites.
type Dumper struct {
	dir           string
	includeImages bool
	maxBytes      int64

	ch        chan DumpRecord
	done      chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	file     *os.File
	fileDay  string
	filePart int
	written  int64
	lastSync time.Time
	dropped  int64
}

// NewDumper starts an archive writer in dir. maxGB <= 0 disables pruning.
func NewDumper(dir string, includeImages bool, maxGB int) *Dumper {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[DUMP] Cannot create archive dir %s: %v; archiving disabled", dir, err)
		return nil
	}
	d := &Dumper{
		dir:           dir,
		includeImages: includeImages,
		ch:            make(chan DumpRecord, dumpQueueSize),
		done:          make(chan struct{}),
	}
	if maxGB > 0 {
		d.maxBytes = int64(maxGB) << 30
	}
	d.prune()
	go d.run()
	log.Printf("[DUMP] Archiving requests to %s (images: %v)", dir, includeImages)
	return d
}

// Record queues a record for archiving. Never blocks: on overflow the
// record is dropped and counted.
func (d *Dumper) Record(rec DumpRecord) {
	if d == nil {
		return
	}
	select {
	case d.ch <- rec:
	default:
		d.mu.Lock()
		d.dropped++
		n := d.dropped
		d.mu.Unlock()
		if n == 1 || n%1000 == 0 {
			log.Printf("[DUMP] Archive queue full, dropped %d records so far", n)
		}
	}
}

// Close drains the queue, syncs and closes the file. Idempotent.
func (d *Dumper) Close() {
	if d == nil {
		return
	}
	d.closeOnce.Do(func() {
		close(d.ch)
		<-d.done
	})
}

// run consumes queued records and writes them to the daily JSONL file.
func (d *Dumper) run() {
	defer close(d.done)
	syncTick := time.NewTicker(dumpSyncInterval)
	defer syncTick.Stop()
	for {
		select {
		case rec, ok := <-d.ch:
			if !ok {
				d.syncNow()
				d.closeFile()
				return
			}
			d.write(rec)
		case <-syncTick.C:
			d.syncNow()
			// Rotate-style retention: prune on every tick so max_gb bounds
			// the folder continuously, not just at startup or part changes.
			if d.maxBytes > 0 {
				d.mu.Lock()
				d.pruneLocked()
				d.mu.Unlock()
			}
		}
	}
}

// write appends one record as a single JSONL line, rotating first if the
// current file is for another day or is full.
func (d *Dumper) write(rec DumpRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		log.Printf("[DUMP] Cannot marshal record: %v", err)
		return
	}
	line = append(line, '\n')

	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureFileLocked(); err != nil {
		log.Printf("[DUMP] Cannot open archive file: %v", err)
		return
	}
	if d.file == nil {
		return
	}
	if _, err := d.file.Write(line); err != nil {
		log.Printf("[DUMP] Archive write failed: %v", err)
		return
	}
	d.written += int64(len(line))
	if d.written >= dumpFileMaxBytes {
		// Rotate to a new part of the same day.
		d.closeFileLocked()
		d.filePart++
		if err := d.ensureFileLocked(); err != nil {
			log.Printf("[DUMP] Cannot open next archive part: %v", err)
			return
		}
		d.pruneLocked()
	}
}

// ensureFileLocked opens today's file, appending to the newest existing part
// if one is small enough, else starting the next part. Caller holds d.mu.
func (d *Dumper) ensureFileLocked() error {
	if d.file != nil {
		return nil
	}
	day := time.Now().Format("2006-01-02")
	if day != d.fileDay {
		d.fileDay = day
		d.filePart = d.newestPartLocked(day)
	}
	path := filepath.Join(d.dir, d.partNameLocked())
	// Continue appending across restarts; the part number continues from
	// whatever already exists on disk.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err == nil {
		d.written = st.Size()
		if d.written >= dumpFileMaxBytes {
			f.Close()
			d.filePart++
			path = filepath.Join(d.dir, d.partNameLocked())
			f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return err
			}
			if st, err := f.Stat(); err == nil {
				d.written = st.Size()
			} else {
				d.written = 0
			}
		}
	} else {
		d.written = 0
	}
	d.file = f
	d.lastSync = time.Now()
	return nil
}

// partNameLocked builds the current filename: 2026-10-29.jsonl, then
// 2026-10-29-2.jsonl, 2026-10-29-3.jsonl, ... Caller holds d.mu.
func (d *Dumper) partNameLocked() string {
	if d.filePart <= 1 {
		return d.fileDay + ".jsonl"
	}
	return fmt.Sprintf("%s-%d.jsonl", d.fileDay, d.filePart)
}

// newestPartLocked scans the dir for existing parts of day and returns the
// highest part number found (1 when none). Caller holds d.mu.
func (d *Dumper) newestPartLocked(day string) int {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return 1
	}
	newest := 1
	prefix := day + ".jsonl"
	prefixN := day + "-"
	suffix := ".jsonl"
	for _, e := range entries {
		name := e.Name()
		if name == prefix {
			if newest < 1 {
				newest = 1
			}
			continue
		}
		if strings.HasPrefix(name, prefixN) && strings.HasSuffix(name, suffix) {
			var n int
			if _, err := fmt.Sscanf(name, prefixN+"%d"+suffix, &n); err == nil && n > newest {
				newest = n
			}
		}
	}
	return newest
}

// pruneLocked deletes oldest files until the folder is under maxBytes.
// Caller holds d.mu. Safe no-op when maxBytes is 0.
func (d *Dumper) pruneLocked() {
	if d.maxBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	type archFile struct {
		name string
		size int64
	}
	var files []archFile
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, archFile{name: e.Name(), size: info.Size()})
		total += info.Size()
	}
	if total <= d.maxBytes {
		return
	}
	// File names sort chronologically (YYYY-MM-DD...), so ascending name
	// order is oldest-first.
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	for _, f := range files {
		if total <= d.maxBytes {
			break
		}
		// Never delete the file currently open for writing.
		if d.file != nil && f.name == d.partNameLocked() {
			continue
		}
		if err := os.Remove(filepath.Join(d.dir, f.name)); err != nil {
			log.Printf("[DUMP] Cannot prune %s: %v", f.name, err)
			continue
		}
		total -= f.size
		log.Printf("[DUMP] Pruned old archive %s (%d MB)", f.name, f.size>>20)
	}
}

// prune runs retention at startup (no file open yet). Caller is the constructor.
func (d *Dumper) prune() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked()
}

// syncNow fsyncs the archive file if open.
func (d *Dumper) syncNow() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file != nil {
		d.file.Sync()
		d.lastSync = time.Now()
	}
}

// closeFile closes the archive file (locked wrapper).
func (d *Dumper) closeFile() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeFileLocked()
}

// closeFileLocked closes the current file. Caller holds d.mu.
func (d *Dumper) closeFileLocked() {
	if d.file != nil {
		d.file.Sync()
		d.file.Close()
		d.file = nil
	}
}

// stripImages replaces base64 payloads with a size placeholder so archives
// stay text-usable without storing megabytes of screenshots per request.
func stripImages(raw []byte) []byte {
	return dumpDataURLRe.ReplaceAllFunc(raw, func(m []byte) []byte {
		return []byte(fmt.Sprintf("[image stripped: %d bytes]", len(m)))
	})
}

// rawJSON returns a json.RawMessage copy of raw with images handled per the
// dumper's configuration. Returns nil for empty input.
func (d *Dumper) rawJSON(raw []byte) *json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if d != nil && !d.includeImages {
		raw = stripImages(raw)
	}
	r := json.RawMessage(raw)
	return &r
}
