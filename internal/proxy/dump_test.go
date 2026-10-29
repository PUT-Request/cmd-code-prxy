package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/config"
)

func TestDumpWritesJSONLLine(t *testing.T) {
	dir := t.TempDir()
	d := NewDumper(dir, false, 0)
	if d == nil {
		t.Fatal("dumper not created")
	}
	req := json.RawMessage(`{"params":{"model":"m"}}`)
	d.Record(DumpRecord{
		TS:        "2026-10-29T00:00:00Z",
		Client:    "****1111",
		Requested: "free:auto",
		Model:     "prov/model-b:free",
		Status:    200,
		CCRequest: &req,
		Response: &DumpResponse{
			Text:  "hello",
			Usage: DumpUsage{Input: 10, Output: 5},
		},
		MS: 42,
	})
	d.Close() // drains the queue

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v, want exactly one", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var rec DumpRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("invalid JSONL: %v", err)
	}
	if rec.Model != "prov/model-b:free" || rec.Client != "****1111" || rec.Status != 200 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Response == nil || rec.Response.Text != "hello" || rec.Response.Usage.Output != 5 {
		t.Fatalf("response = %+v", rec.Response)
	}
	if rec.CCRequest == nil || !strings.Contains(string(*rec.CCRequest), `"model":"m"`) {
		t.Fatalf("cc_request = %v", rec.CCRequest)
	}
}

func TestDumpAppendsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		d := NewDumper(dir, false, 0)
		if d == nil {
			t.Fatal("dumper not created")
		}
		d.Record(DumpRecord{TS: "t", Status: 200})
		d.Close()
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v, want one file across reopens", files)
	}
	data, _ := os.ReadFile(files[0])
	if n := strings.Count(strings.TrimSpace(string(data)), "\n"); n != 1 {
		t.Fatalf("lines = %d, want 2 (append)", n+1)
	}
}

func TestDumpStripsImagesByDefault(t *testing.T) {
	dir := t.TempDir()
	d := NewDumper(dir, false, 0)
	blob := strings.Repeat("QUJD", 1000)
	raw := []byte(`{"m":"data:image/png;base64,` + blob + `"}`)
	stripped := d.rawJSON(raw)
	if strings.Contains(string(*stripped), blob) {
		t.Fatal("base64 not stripped")
	}
	if !strings.Contains(string(*stripped), "image stripped") {
		t.Fatalf("placeholder missing: %s", string(*stripped))
	}
	d.Close()

	d2 := NewDumper(dir, true, 0)
	kept := d2.rawJSON(raw)
	if !strings.Contains(string(*kept), blob) {
		t.Fatal("include_images=true stripped the payload")
	}
	d2.Close()
}

func TestDumpPartNamingAndResume(t *testing.T) {
	dir := t.TempDir()
	// Existing parts from a prior run: day.jsonl and day-3.jsonl.
	day := "2099-01-02"
	os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, day+"-3.jsonl"), []byte("{}\n"), 0o644)

	d := NewDumper(dir, false, 0)
	d.mu.Lock()
	d.fileDay = day
	d.filePart = d.newestPartLocked(day)
	if got := d.partNameLocked(); got != day+"-3.jsonl" {
		d.mu.Unlock()
		t.Fatalf("resume part = %q, want %s-3.jsonl", got, day)
	}
	d.mu.Unlock()
	d.Close()
}

func TestDumpQueueOverflowDropsNotBlocks(t *testing.T) {
	dir := t.TempDir()
	d := NewDumper(dir, false, 0)
	// Fill the queue far beyond capacity; Record must never block.
	for i := 0; i < dumpQueueSize+5000; i++ {
		d.Record(DumpRecord{TS: "t", Status: 200})
	}
	d.mu.Lock()
	dropped := d.dropped
	d.mu.Unlock()
	if dropped == 0 {
		t.Fatal("expected drops when the queue overflows")
	}
	d.Close()
}

func TestDumpPruneOldestFiles(t *testing.T) {
	dir := t.TempDir()
	old1 := filepath.Join(dir, "2000-01-01.jsonl")
	old2 := filepath.Join(dir, "2000-01-02.jsonl")
	newF := filepath.Join(dir, "2099-01-01.jsonl")
	os.WriteFile(old1, make([]byte, 600), 0o644)
	os.WriteFile(old2, make([]byte, 600), 0o644)
	os.WriteFile(newF, make([]byte, 600), 0o644)

	d := NewDumper(dir, false, 0)
	d.maxBytes = 1000 // force pruning of the two oldest
	d.prune()
	if _, err := os.Stat(old1); !os.IsNotExist(err) {
		t.Fatal("oldest file not pruned")
	}
	if _, err := os.Stat(newF); err != nil {
		t.Fatal("newest file should survive")
	}
	d.Close()
}

// --- handler-level archiving ---

func TestHandlerDumpsSuccessfulRequest(t *testing.T) {
	dir := t.TempDir()
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, []string{"prov/model-b:free"})
	d := NewDumper(dir, false, 0)
	p.SetDumper(d)

	w := chatRequest(t, p, `{"model":"free:auto","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	d.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	data, _ := os.ReadFile(files[0])
	var rec DumpRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("bad record: %v (%s)", err, string(data))
	}
	if rec.Model != "prov/model-b:free" {
		t.Fatalf("model = %q", rec.Model)
	}
	if rec.Requested != "free:auto" {
		t.Fatalf("requested = %q", rec.Requested)
	}
	if rec.Response == nil || rec.Response.Text != "pong" {
		t.Fatalf("response = %+v", rec.Response)
	}
	if rec.Response.Usage.Input != 10 || rec.Response.Usage.Output != 5 {
		t.Fatalf("usage = %+v", rec.Response.Usage)
	}
	if rec.CCRequest == nil || !strings.Contains(string(*rec.CCRequest), "prov/model-b:free") {
		t.Fatalf("cc_request missing winning model: %v", rec.CCRequest)
	}
	if rec.Client != "****1111" {
		t.Fatalf("client = %q", rec.Client)
	}
}

func TestHandlerDumpsFailedRequest(t *testing.T) {
	dir := t.TempDir()
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, []string{"prov/model-a:free"}) // model-a always 400s
	d := NewDumper(dir, false, 0)
	p.SetDumper(d)

	w := chatRequest(t, p, `{"model":"free:auto","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != 400 {
		t.Fatalf("status = %d", w.Code)
	}
	d.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	data, _ := os.ReadFile(files[0])
	var rec DumpRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("bad record: %v", err)
	}
	if rec.Status != 400 || rec.Error == "" {
		t.Fatalf("failure record = %+v", rec)
	}
	if rec.Response != nil {
		t.Fatalf("failure should carry no response: %+v", rec.Response)
	}
}

// TestHandlerSkipsDirectModels: the archive is router-only — direct model
// requests must not be dumped.
func TestHandlerSkipsDirectModels(t *testing.T) {
	dir := t.TempDir()
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, []string{"prov/model-b:free"})
	d := NewDumper(dir, false, 0)
	p.SetDumper(d)

	// Success path (direct) and failure path (direct model-a, always 400s).
	if w := chatRequest(t, p, `{"model":"prov/model-b:free","messages":[{"role":"user","content":"hi"}],"stream":false}`); w.Code != 200 {
		t.Fatalf("direct success status = %d", w.Code)
	}
	if w := chatRequest(t, p, `{"model":"prov/model-a:free","messages":[{"role":"user","content":"hi"}],"stream":false}`); w.Code != 400 {
		t.Fatalf("direct failure status = %d", w.Code)
	}
	d.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 0 {
		data, _ := os.ReadFile(files[0])
		t.Fatalf("direct requests were archived: %s", string(data))
	}
}

func TestHandlerDumpStreamsToolCalls(t *testing.T) {
	dir := t.TempDir()
	srv := newSSEUpstream(t, `{"type":"text-delta","text":"let me check"}
`+
		`{"type":"tool-use","toolCallID":"call_1","toolName":"bash"}
`+
		`{"type":"tool-delta","text":"{\"cmd\":\"ls\"}"}
`+
		"{\"type\":\"finish\",\"finishReason\":\"tool_calls\",\"totalUsage\":{\"inputTokens\":3,\"outputTokens\":4}}\n")
	p := NewProxy(func(r *http.Request) *config.APIKeyDef {
		return &config.APIKeyDef{Key: "sk-test-1111", CommandCodeKeys: []string{"cc-key-1"}}
	})
	p.BaseURL = srv.URL
	p.SetLimits(0, 0)
	p.SetRouter(true, []string{"prov/m:free"}, 5, time.Minute)
	d := NewDumper(dir, false, 0)
	p.SetDumper(d)

	w := chatRequest(t, p, `{"model":"free:auto","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	d.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	data, _ := os.ReadFile(files[0])
	var rec DumpRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("bad record: %v", err)
	}
	if rec.Response == nil {
		t.Fatal("no response captured")
	}
	if len(rec.Response.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", rec.Response.ToolCalls)
	}
	tc := rec.Response.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "bash" || !strings.Contains(tc.Function.Arguments, "ls") {
		t.Fatalf("tool call = %+v", tc)
	}
	if rec.Response.Text != "let me check" {
		t.Fatalf("text = %q", rec.Response.Text)
	}
	if rec.Response.Finish != "tool_calls" {
		t.Fatalf("finish = %q", rec.Response.Finish)
	}
}

// newSSEUpstream serves a fixed NDJSON body for any generate request.
func newSSEUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(body))
	}))
}
