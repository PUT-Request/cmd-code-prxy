package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
	"github.com/dev2k6/command-code-proxy-server/internal/config"
	"github.com/dev2k6/command-code-proxy-server/internal/version"
	"github.com/google/uuid"
)

const defaultBaseURL = "https://api.commandcode.ai"

// Timeouts for upstream requests. NOTE: there is deliberately NO overall
// http.Client timeout — Client.Timeout covers the entire response body, so
// it killed long streams after 5 minutes. Large contexts (e.g. ~500k tokens)
// legitimately stream for many minutes (long reasoning, then a tool call),
// so the deadline fired mid-tool-call and the message randomly ended.
// Dial/header timeouts bound hung connections instead; the body streams
// until upstream finishes or the client disconnects (request context).
const upstreamDialTimeout = 30 * time.Second
const upstreamResponseHeaderTimeout = 300 * time.Second
const debugLogLimit = 20000

const (
	// keyCooldownDuration is how long a failed upstream CommandCode key is
	// skipped by subsequent requests. This keeps one bad key from adding
	// latency (or failures) to every request for 20 minutes.
	keyCooldownDuration = 20 * time.Minute
	// errBodyReadLimit caps how much of an upstream error body is read for
	// logging and error responses.
	errBodyReadLimit = 4096
	// retryRoundBackoff is the pause before the second pass over all keys
	// when every key failed transiently.
	retryRoundBackoff = 1 * time.Second
)

func truncateLog(s string) string {
	if len(s) <= debugLogLimit {
		return s
	}
	return s[:debugLogLimit] + fmt.Sprintf("... [truncated %d bytes]", len(s)-debugLogLimit)
}

func (p *Proxy) debugf(format string, args ...any) {
	if p.Debug {
		log.Printf(format, args...)
	}
}

// lineScanner reads newline-delimited upstream events without any token size
// limit. bufio.Scanner caps a single line (previously 30MB here); a huge
// line — e.g. a giant tool-call input at large context sizes — exceeded the
// cap and silently truncated the stream mid-tool-call. ReadBytes grows as
// needed instead.
type lineScanner struct {
	r    *bufio.Reader
	line string
	err  error
	done bool
}

func newLineScanner(rd io.Reader) *lineScanner {
	return &lineScanner{r: bufio.NewReaderSize(rd, 64*1024)}
}

func nilIfEOF(err error) error {
	if err == io.EOF {
		return nil
	}
	return err
}

// Scan advances to the next line (empty lines included, like Scanner).
func (s *lineScanner) Scan() bool {
	if s.done {
		return false
	}
	raw, err := s.r.ReadBytes('\n')
	if len(raw) > 0 {
		s.line = string(raw)
		if err != nil {
			// Data followed by EOF/error: deliver this line, then stop.
			s.err = nilIfEOF(err)
			s.done = true
		}
		return true
	}
	if err != nil {
		s.err = nilIfEOF(err)
		s.done = true
		return false
	}
	s.line = ""
	return true
}

// Text returns the most recent line.
func (s *lineScanner) Text() string { return s.line }

// Err returns the first non-EOF error encountered, if any.
func (s *lineScanner) Err() error { return s.err }

func (p *Proxy) writeOpenAIError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.OpenAIErrorResponse{Error: api.OpenAIError{
		Message: message,
		Type:    errType,
		Param:   nil,
		Code:    nil,
	}})
}

func normalizeFinishReason(reason string) string {
	switch reason {
	case "tool_calls", "tool-calls":
		return "tool_calls"
	case "length", "max_tokens":
		return "length"
	case "content_filter", "content-filter":
		return "content_filter"
	default:
		// Pass through unknown reasons (e.g. context-limit variants) instead
		// of misreporting them as a clean "stop", which hid truncated
		// generations from clients.
		if reason != "" {
			return reason
		}
		return "stop"
	}
}

// Proxy struct
type Proxy struct {
	BaseURL string
	Client  *http.Client
	Debug   bool

	// keyDefFn is called per-request to retrieve the matched APIKeyDef.
	keyDefFn func(r *http.Request) *config.APIKeyDef

	// recordUsage reports completed token usage to the dashboard. Nil when no
	// dashboard is configured. It receives the client key fingerprint, never the
	// raw key.
	recordUsage func(clientKey, model string, input, output int)

	// mu guards keyCooldown.
	mu sync.Mutex
	// keyCooldown maps an upstream CommandCode key to the time until which
	// it is skipped (it failed recently). Entries expire after
	// keyCooldownDuration.
	keyCooldown map[string]time.Time
}

// NewProxy creates a new proxy instance.
func NewProxy(keyDefFn func(r *http.Request) *config.APIKeyDef) *Proxy {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   upstreamDialTimeout,
		ResponseHeaderTimeout: upstreamResponseHeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
	}
	return &Proxy{
		BaseURL:     defaultBaseURL,
		Client:      &http.Client{Transport: transport},
		keyDefFn:    keyDefFn,
		keyCooldown: make(map[string]time.Time),
	}
}

// validReasoningEfforts are the effort levels the upstream accepts.
var validReasoningEfforts = map[string]bool{
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"max":     true,
}

// NormalizeReasoningEffort validates a client-supplied effort level and returns
// "" when it should be omitted so the upstream default applies.
func NormalizeReasoningEffort(effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" || !validReasoningEfforts[e] {
		if e != "" {
			log.Printf("[WARN] Ignoring unsupported reasoning_effort %q (want minimal|low|medium|high|max)", effort)
		}
		return ""
	}
	return e
}

// BuildRequest builds the CommandCode request body
func (p *Proxy) BuildRequest(openAIReq api.OpenAIChatRequest) (api.CCRequestBody, error) {
	model := MapModel(openAIReq.Model)
	system, msgs := ExtractSystem(openAIReq.Messages)
	ccMessages := ConvertMessages(msgs)

	temperature := 0.3
	maxTokens := 64000
	reasoningEffort := ""
	if openAIReq.ReasoningEffort != nil {
		reasoningEffort = *openAIReq.ReasoningEffort
	}
	if openAIReq.Temperature != nil {
		temperature = *openAIReq.Temperature
	}
	if openAIReq.MaxTokens != nil {
		maxTokens = *openAIReq.MaxTokens
	}
	if openAIReq.MaxCompletionTokens != nil {
		maxTokens = *openAIReq.MaxCompletionTokens
	}

	tools := ConvertTools(openAIReq.Tools)

	ccBody := api.CCRequestBody{
		Config: api.CCConfig{
			WorkingDir:    ".",
			Date:          time.Now().Format("2006-01-02"),
			Environment:   "cli",
			Structure:     []string{},
			IsGitRepo:     false,
			CurrentBranch: "",
			MainBranch:    "main",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		Memory: "",
		Taste:  "",
		Skills: "",
		Params: api.CCChatParams{
			Model:           model,
			Messages:        ccMessages,
			Tools:           tools,
			System:          system,
			MaxTokens:       maxTokens,
			Temperature:     temperature,
			Stream:          true,
			ReasoningEffort: NormalizeReasoningEffort(reasoningEffort),
		},
		ThreadID: uuid.New().String(),
	}

	return ccBody, nil
}

// CreateUpstreamRequest creates a new HTTP request to the CommandCode API
func (p *Proxy) CreateUpstreamRequest(ctx context.Context, ccBody api.CCRequestBody, ccKey string, bodyJSON []byte) (*http.Request, error) {
	ccReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/alpha/generate", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}

	ccReq.Header.Set("Content-Type", "application/json")
	ccReq.Header.Set("Authorization", "Bearer "+ccKey)
	ccReq.Header.Set("x-command-code-version", version.GetCommandCodeVersion())
	ccReq.Header.Set("x-cli-environment", "production")
	ccReq.Header.Set("Accept", "text/event-stream")

	return ccReq, nil
}

// CallUpstream makes the request to CommandCode API
func (p *Proxy) CallUpstream(req *http.Request) (*http.Response, error) {
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream error: %w", err)
	}
	return resp, nil
}

// allowedModels checks whether the requested model is permitted for the given key.
// Returns the canonical model name and nil error on success.
func (p *Proxy) allowedModel(keyDef *config.APIKeyDef, requestedModel string) (string, error) {
	canonical := MapModel(requestedModel)

	if keyDef == nil || len(keyDef.Models) == 0 {
		// No restriction.
		return canonical, nil
	}

	for _, m := range keyDef.Models {
		if strings.EqualFold(m, canonical) || strings.EqualFold(m, requestedModel) {
			return canonical, nil
		}
	}

	return "", fmt.Errorf("model %q is not allowed for this API key", requestedModel)
}

// HandleChatCompletions handles the /v1/chat/completions endpoint
func (p *Proxy) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}

	keyDef := p.keyDefFn(r)

	// Read request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, "Failed to read body", "invalid_request_error")
		return
	}

	p.debugf("[DEBUG] Client request body: %s", truncateLog(string(body)))

	var openAIReq api.OpenAIChatRequest
	if err := json.Unmarshal(body, &openAIReq); err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %s", err.Error()), "invalid_request_error")
		return
	}

	if len(openAIReq.Messages) == 0 {
		p.writeOpenAIError(w, http.StatusBadRequest, "messages array is required", "invalid_request_error")
		return
	}

	// Model allowlist check
	canonicalModel, err := p.allowedModel(keyDef, openAIReq.Model)
	if err != nil {
		p.writeOpenAIError(w, http.StatusForbidden, err.Error(), "invalid_request_error")
		return
	}

	// Build CommandCode request (uses canonical model name)
	openAIReq.Model = canonicalModel
	ccBody, err := p.BuildRequest(openAIReq)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}

	reqJSON, err := json.Marshal(ccBody)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}
	p.debugf("[DEBUG] CommandCode request body: %s", truncateLog(string(reqJSON)))

	// Determine upstream CC keys to try
	ccKeys := p.ccKeys(keyDef)
	if len(ccKeys) == 0 {
		p.writeOpenAIError(w, http.StatusServiceUnavailable, "No upstream CommandCode keys configured for this API key.", "api_error")
		return
	}

	// Failover + retry loop.
	//
	// Pass 1: try each upstream CC key in turn (healthy keys first, see
	// orderKeys). ANY upstream failure — including 400s — immediately fails
	// over to the next key with no sleep, so one bad key neither fails the
	// request nor adds latency.
	//
	// Pass 2: if every key failed with a transient error (transport error,
	// 429, or 5xx), wait briefly and retry each key once more in LRU order
	// (least-recently-failed first). Deterministic failures (400/401/403/404)
	// are not retried — they would fail identically.
	//
	// Failed keys cool down for keyCooldownDuration (20 minutes) so later
	// requests skip them instead of paying for doomed attempts. If ALL keys
	// are cooling down, they are still tried oldest-failure-first rather
	// than failing fast.
	var ccResp *http.Response
	// pendingCooldown holds keys that failed with an ambiguous status (400,
	// 404, 422: could be the key or the request itself). They only cool down
	// if another key succeeds; if every key fails, the request is blamed and
	// no cooldown is applied.
	var pendingCooldown []string
	// lastStatus/lastMessage carry the most recent failure for the final
	// error response.
	lastStatus := http.StatusBadGateway
	lastMessage := "all upstream keys failed"
	transientSeen := false
	succeeded := false

	ordered := p.orderKeys(ccKeys)
	for round := 0; round < 2 && !succeeded; round++ {
		if round == 1 {
			if !transientSeen {
				break // deterministic failures — retrying is pointless
			}
			log.Printf("[RETRY] All %d CC keys failed with transient errors, retrying each key once", len(ordered))
			select {
			case <-r.Context().Done():
				p.writeOpenAIError(w, http.StatusRequestTimeout, "Client disconnected during retry", "api_error")
				return
			case <-time.After(retryRoundBackoff):
			}
			ordered = p.orderKeys(ccKeys)
		}

		for i, ccKey := range ordered {
			if r.Context().Err() != nil {
				p.writeOpenAIError(w, http.StatusRequestTimeout, "Client disconnected", "api_error")
				return
			}

			ccReq, err := p.CreateUpstreamRequest(r.Context(), ccBody, ccKey, reqJSON)
			if err != nil {
				p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to create upstream request", "server_error")
				return
			}

			ccResp, err = p.CallUpstream(ccReq)
			if err != nil {
				ccResp = nil
				if r.Context().Err() != nil {
					p.writeOpenAIError(w, http.StatusRequestTimeout, "Client disconnected", "api_error")
					return
				}
				p.markKeyFailed(ccKey)
				transientSeen = true
				lastStatus = http.StatusBadGateway
				lastMessage = fmt.Sprintf("Upstream error: %s", err.Error())
				log.Printf("[RETRY] Transport error with CC key %s (%d/%d), trying next key: %v",
					keyFingerprint(ccKey), i+1, len(ordered), err)
				continue
			}

			if ccResp.StatusCode == http.StatusOK {
				succeeded = true
				p.markKeyOK(ccKey)
				for _, pending := range pendingCooldown {
					if pending != ccKey {
						p.markKeyFailed(pending)
					}
				}
				pendingCooldown = nil
				break
			}

			status := ccResp.StatusCode
			errBody, _ := io.ReadAll(io.LimitReader(ccResp.Body, errBodyReadLimit))
			ccResp.Body.Close()
			ccResp = nil
			lastStatus = status
			lastMessage = fmt.Sprintf("Upstream error: %s", string(errBody))
			if isAmbiguousStatus(status) {
				// Might be this key, might be the request — decide on success.
				pendingCooldown = append(pendingCooldown, ccKey)
				log.Printf("[RETRY] Upstream returned %d with CC key %s (%d/%d), trying next key: %s",
					status, keyFingerprint(ccKey), i+1, len(ordered), string(errBody))
				continue
			}
			p.markKeyFailed(ccKey)
			if isTransientStatus(status) {
				transientSeen = true
			}
			log.Printf("[RETRY] Upstream returned %d with CC key %s (%d/%d), trying next key: %s",
				status, keyFingerprint(ccKey), i+1, len(ordered), string(errBody))
		}
	}

	if !succeeded {
		// Nobody succeeded: ambiguous 400s blame the request, not the keys.
		pendingCooldown = nil
		log.Printf("[ERROR] All %d CC keys failed, last error: %d %s", len(ordered), lastStatus, lastMessage)
		outStatus := http.StatusBadGateway
		if lastStatus >= http.StatusBadRequest && lastStatus < http.StatusInternalServerError {
			outStatus = lastStatus
		}
		p.writeOpenAIError(w, outStatus, lastMessage, "api_error")
		return
	}
	defer ccResp.Body.Close()

	requestID := "chatcmpl-" + uuid.New().String()[:29]
	created := time.Now().Unix()
	clientFingerprint := clientKeyFingerprint(keyDef)

	if openAIReq.Stream {
		p.StreamResponse(w, r, ccResp, requestID, ccBody.Params.Model, created, clientFingerprint)
	} else {
		p.NonStreamResponse(w, ccResp, requestID, ccBody.Params.Model, created, clientFingerprint)
	}
}

// clientKeyFingerprint returns a display-safe identifier for the authenticated
// client key, used as the account label in usage stats.
func clientKeyFingerprint(keyDef *config.APIKeyDef) string {
	if keyDef == nil {
		return "(none)"
	}
	return keyFingerprint(keyDef.Key)
}

// ccKeys returns the list of upstream CC keys to try, from the per-key definition
// or the legacy single-key field.
func (p *Proxy) ccKeys(keyDef *config.APIKeyDef) []string {
	if keyDef != nil && len(keyDef.CommandCodeKeys) > 0 {
		return keyDef.CommandCodeKeys
	}
	return nil
}

// orderKeys returns upstream keys with healthy keys first (in config order),
// followed by cooling-down keys least-recently-failed first (LRU). Expired
// cooldown entries are pruned. Even fully-cooled lists are returned in full
// so a request still tries something rather than failing fast.
func (p *Proxy) orderKeys(keys []string) []string {
	now := time.Now()
	type cooled struct {
		key   string
		until time.Time
	}
	var healthy []string
	var cooling []cooled
	p.mu.Lock()
	if p.keyCooldown == nil {
		p.keyCooldown = make(map[string]time.Time)
	}
	for _, k := range keys {
		if until, ok := p.keyCooldown[k]; ok && now.Before(until) {
			cooling = append(cooling, cooled{key: k, until: until})
			continue
		}
		healthy = append(healthy, k)
	}
	for k, until := range p.keyCooldown {
		if !now.Before(until) {
			delete(p.keyCooldown, k)
		}
	}
	p.mu.Unlock()
	sort.Slice(cooling, func(i, j int) bool { return cooling[i].until.Before(cooling[j].until) })
	out := make([]string, 0, len(keys))
	out = append(out, healthy...)
	for _, c := range cooling {
		out = append(out, c.key)
	}
	return out
}

// markKeyFailed puts an upstream key into cooldown for keyCooldownDuration.
func (p *Proxy) markKeyFailed(key string) {
	p.mu.Lock()
	if p.keyCooldown == nil {
		p.keyCooldown = make(map[string]time.Time)
	}
	p.keyCooldown[key] = time.Now().Add(keyCooldownDuration)
	p.mu.Unlock()
	log.Printf("[COOLDOWN] CC key %s cooling down for %v", keyFingerprint(key), keyCooldownDuration)
}

// markKeyOK clears any cooldown for an upstream key that just succeeded.
func (p *Proxy) markKeyOK(key string) {
	p.mu.Lock()
	if p.keyCooldown != nil {
		delete(p.keyCooldown, key)
	}
	p.mu.Unlock()
}

// SetUsageRecorder installs the token usage callback used by the dashboard.
func (p *Proxy) SetUsageRecorder(fn func(clientKey, model string, input, output int)) {
	p.recordUsage = fn
}

// KeyCooldownRemaining reports how long an upstream key stays in cooldown, or
// 0 when it is usable. Used by the dashboard for live status.
func (p *Proxy) KeyCooldownRemaining(key string) time.Duration {
	p.mu.Lock()
	until, ok := p.keyCooldown[key]
	p.mu.Unlock()
	if !ok {
		return 0
	}
	if d := time.Until(until); d > 0 {
		return d
	}
	return 0
}

// keyFingerprint renders a key safe for logs (last 4 chars only).
func keyFingerprint(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

// isAmbiguousStatus reports statuses that may blame either the key or the
// request itself (e.g. model not entitled on this key vs. malformed request).
func isAmbiguousStatus(status int) bool {
	return status == http.StatusBadRequest ||
		status == http.StatusNotFound ||
		status == http.StatusUnprocessableEntity
}

// isTransientStatus reports statuses worth a second round after backoff.
func isTransientStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// StreamResponse handles streaming response from CommandCode to OpenAI SSE
func (p *Proxy) StreamResponse(w http.ResponseWriter, r *http.Request, ccResp *http.Response, requestID, model string, created int64, clientKey string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	scanner := newLineScanner(ccResp.Body)
	sentRole := false
	seenFinish := false
	var streamErr string
	var inputTokens, outputTokens int
	toolCallIndex := 0
	lastToolUseIndex := -1
	toolCallIndexes := map[string]int{}

	for scanner.Scan() {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(line))

		var event api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Type {
		case "text-delta":
			delta := api.OpenAIDelta{Content: event.Text}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "reasoning-delta":
			if event.Text == "" {
				continue
			}
			delta := api.OpenAIDelta{ReasoningContent: event.Text}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "reasoning-start", "reasoning-end":
			// No user-visible content, skip silently

		case "tool-use":
			lastToolUseIndex = toolCallIndex
			toolCalls := []api.OpenAIDeltaToolCall{{
				Index:    toolCallIndex,
				ID:       event.ToolCallID,
				Type:     "function",
				Function: &api.OpenAIDeltaFunction{Name: event.ToolName},
			}}
			delta := api.OpenAIDelta{ToolCalls: toolCalls}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})
			toolCallIndex++

		case "tool-delta":
			deltaIdx := lastToolUseIndex
			if deltaIdx < 0 {
				// No preceding tool-use header: 0 beats the old
				// toolCallIndex-1 == -1, which violates the OpenAI
				// schema (index >= 0) and made clients drop the call.
				deltaIdx = 0
			}
			toolCalls := []api.OpenAIDeltaToolCall{{
				Index:    deltaIdx,
				Function: &api.OpenAIDeltaFunction{Arguments: event.Text},
			}}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &api.OpenAIDelta{ToolCalls: toolCalls}}},
			})

		case "tool-input-start":
			if _, ok := toolCallIndexes[event.ID]; !ok {
				toolCallIndexes[event.ID] = toolCallIndex
				toolCallIndex++
			}
			delta := api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
				Index: toolCallIndexes[event.ID],
				ID:    event.ID,
				Type:  "function",
				Function: &api.OpenAIDeltaFunction{
					Name: event.ToolName,
				},
			}}}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "tool-input-delta":
			idx, ok := toolCallIndexes[event.ID]
			if !ok {
				idx = toolCallIndex
				toolCallIndexes[event.ID] = idx
				toolCallIndex++
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
					Index:    idx,
					Function: &api.OpenAIDeltaFunction{Arguments: event.Delta},
				}}}}},
			})

		case "tool-call":
			if _, alreadyStreamed := toolCallIndexes[event.ToolCallID]; alreadyStreamed {
				continue
			}
			idx := toolCallIndex
			toolCallIndexes[event.ToolCallID] = idx
			toolCallIndex++
			lastToolUseIndex = idx
			args := ""
			if event.Input != nil {
				if data, err := json.Marshal(event.Input); err == nil {
					args = string(data)
				}
			}
			delta := api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
				Index: idx,
				ID:    event.ToolCallID,
				Type:  "function",
				Function: &api.OpenAIDeltaFunction{
					Name:      event.ToolName,
					Arguments: args,
				},
			}}}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "finish":
			seenFinish = true
			// Upstream reports usage only on the final event. Without this the
			// streaming path reported zero tokens, so every streaming request
			// (including all tool-call traffic) was invisible in stats.
			if event.TotalUsage != nil {
				inputTokens = event.TotalUsage.InputTokens
				outputTokens = event.TotalUsage.OutputTokens
			}
			reason := normalizeFinishReason(event.FinishReason)
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{
					Index:        0,
					Delta:        &api.OpenAIDelta{},
					FinishReason: &reason,
				}},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

		case "error":
			if event.Error != nil {
				streamErr = event.Error.Message
				if event.Error.StatusCode != nil {
					streamErr = fmt.Sprintf("%s (status %d)", streamErr, *event.Error.StatusCode)
				}
			} else {
				streamErr = "unknown upstream error"
			}
			log.Printf("[ERROR] Stream error: %s", streamErr)
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		log.Printf("[ERROR] Upstream stream read error: %v", err)
		if streamErr == "" {
			streamErr = err.Error()
		}
	}
	if !seenFinish {
		// Upstream died mid-generation (e.g. killed mid-tool-call): never
		// leave the client hanging on a truncated tool call. Surface the
		// error, then close the stream cleanly.
		log.Printf("[WARN] Upstream stream ended without finish (sentRole=%v): %s", sentRole, streamErr)
		if streamErr != "" {
			if data, merr := json.Marshal(map[string]any{"error": map[string]any{"message": streamErr, "type": "api_error"}}); merr == nil {
				fmt.Fprintf(w, "data: %s\n\n", data)
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
	p.record(clientKey, model, inputTokens, outputTokens)
}

// record forwards usage to the dashboard when one is attached.
func (p *Proxy) record(clientKey, model string, input, output int) {
	if p.recordUsage != nil {
		p.recordUsage(clientKey, model, input, output)
	}
}

// WriteSSE writes a Server-Sent Event
func (p *Proxy) WriteSSE(w io.Writer, flusher http.Flusher, resp api.OpenAIChatResponse) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// NonStreamResponse handles non-streaming response
func (p *Proxy) NonStreamResponse(w http.ResponseWriter, ccResp *http.Response, requestID, model string, created int64, clientKey string) {
	scanner := newLineScanner(ccResp.Body)
	seenFinish := false
	var streamErr string
	lastToolUseIdx := -1

	var content strings.Builder
	var reasoningContent strings.Builder
	var inputTokens, outputTokens int
	var hasToolCalls bool
	var toolCalls []api.ToolCall
	toolCallByID := map[string]int{}
	toolInputBuffers := map[string]*strings.Builder{}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(line))

		var event api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Type {
		case "text-delta":
			content.WriteString(event.Text)
		case "reasoning-delta":
			reasoningContent.WriteString(event.Text)
		case "reasoning-start", "reasoning-end":
			// No user-visible content, skip silently
		case "tool-use":
			hasToolCalls = true
			toolCallByID[event.ToolCallID] = len(toolCalls)
			lastToolUseIdx = len(toolCalls)
			toolCalls = append(toolCalls, api.ToolCall{
				ID:   event.ToolCallID,
				Type: "function",
				Function: api.FunctionCall{
					Name:      event.ToolName,
					Arguments: "",
				},
			})
		case "tool-delta":
			if idx := lastToolUseIdx; idx >= 0 && idx < len(toolCalls) {
				toolCalls[idx].Function.Arguments += event.Text
			} else if len(toolCalls) > 0 {
				toolCalls[len(toolCalls)-1].Function.Arguments += event.Text
			}
		case "tool-input-start":
			hasToolCalls = true
			toolCallByID[event.ID] = len(toolCalls)
			toolInputBuffers[event.ID] = &strings.Builder{}
			toolCalls = append(toolCalls, api.ToolCall{
				ID:   event.ID,
				Type: "function",
				Function: api.FunctionCall{
					Name:      event.ToolName,
					Arguments: "",
				},
			})
		case "tool-input-delta":
			if b := toolInputBuffers[event.ID]; b != nil {
				b.WriteString(event.Delta)
			}
			if idx, ok := toolCallByID[event.ID]; ok {
				toolCalls[idx].Function.Arguments += event.Delta
			}
		case "tool-call":
			hasToolCalls = true
			args := ""
			if event.Input != nil {
				if data, err := json.Marshal(event.Input); err == nil {
					args = string(data)
				}
			}
			if idx, ok := toolCallByID[event.ToolCallID]; ok {
				toolCalls[idx].Function.Name = event.ToolName
				if args != "" {
					toolCalls[idx].Function.Arguments = args
				}
			} else {
				toolCallByID[event.ToolCallID] = len(toolCalls)
				toolCalls = append(toolCalls, api.ToolCall{
					ID:   event.ToolCallID,
					Type: "function",
					Function: api.FunctionCall{
						Name:      event.ToolName,
						Arguments: args,
					},
				})
			}
		case "finish":
			seenFinish = true
			if event.TotalUsage != nil {
				inputTokens = event.TotalUsage.InputTokens
				outputTokens = event.TotalUsage.OutputTokens
			}
		case "error":
			if event.Error != nil {
				streamErr = event.Error.Message
				if event.Error.StatusCode != nil {
					streamErr = fmt.Sprintf("%s (status %d)", streamErr, *event.Error.StatusCode)
				}
			} else {
				streamErr = "unknown upstream error"
			}
			log.Printf("[ERROR] Stream error: %s", streamErr)
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		log.Printf("[ERROR] Upstream stream read error: %v", err)
		if streamErr == "" {
			streamErr = err.Error()
		}
	}
	if !seenFinish {
		log.Printf("[WARN] Upstream stream ended without finish (hasToolCalls=%v): %s", hasToolCalls, streamErr)
	}
	if streamErr != "" && content.Len() == 0 && !hasToolCalls {
		// Truncated before any usable output: return a real error so the
		// client retries instead of accepting an empty message.
		p.writeOpenAIError(w, http.StatusBadGateway, fmt.Sprintf("Upstream error: %s", streamErr), "api_error")
		return
	}
	p.record(clientKey, model, inputTokens, outputTokens)

	msg := &api.OpenAIMessage{
		Role:    "assistant",
		Content: content.String(),
	}
	if reasoningContent.Len() > 0 {
		rc := reasoningContent.String()
		msg.ReasoningContent = &rc
	}
	finishReason := "stop"
	if hasToolCalls {
		msg.Content = nil
		msg.ToolCalls = toolCalls
		finishReason = "tool_calls"
	}

	response := api.OpenAIChatResponse{
		ID:      requestID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []api.OpenAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Usage: &api.OpenAIUsage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (p *Proxy) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, "Failed to read body", "invalid_request_error")
		return
	}

	p.debugf("[DEBUG] Client responses request body: %s", truncateLog(string(body)))

	var responsesReq api.OpenAIResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %s", err.Error()), "invalid_request_error")
		return
	}

	chatReq := responsesToChatRequest(responsesReq)
	// Log the wire-level image count so oversized payloads are visible without
	// logging any base64.
	if n := countImageURLs(chatReq.Messages); n > 0 {
		log.Printf("[INFO] Request carries %d image(s)", n)
	}
	rewritten, err := json.Marshal(chatReq)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	p.HandleChatCompletions(w, r)
}

func responsesToChatRequest(req api.OpenAIResponsesRequest) api.OpenAIChatRequest {
	messages := responsesInputToMessages(req.Input)
	if req.Instructions != nil {
		messages = append([]api.OpenAIMessage{{Role: "system", Content: req.Instructions}}, messages...)
	}

	maxTokens := req.MaxCompletionTokens
	if maxTokens == nil {
		maxTokens = req.MaxOutputTokens
	}
	if maxTokens == nil {
		maxTokens = req.MaxTokens
	}

	return api.OpenAIChatRequest{
		Model:               req.Model,
		Messages:            messages,
		Temperature:         req.Temperature,
		MaxTokens:           req.MaxTokens,
		MaxCompletionTokens: maxTokens,
		Stream:              req.Stream,
		Tools:               req.Tools,
		ToolChoice:          req.ToolChoice,
		ParallelToolCalls:   req.ParallelToolCalls,
		ResponseFormat:      req.ResponseFormat,
		Stop:                req.Stop,
		TopP:                req.TopP,
		User:                req.User,
		ReasoningEffort:     reasoningEffort(req),
	}
}

// reasoningEffort pulls the effort level out of the Responses request.
func reasoningEffort(req api.OpenAIResponsesRequest) *string {
	if req.Reasoning == nil || req.Reasoning.Effort == "" {
		return nil
	}
	effort := NormalizeReasoningEffort(req.Reasoning.Effort)
	if effort == "" {
		return nil
	}
	return &effort
}

// responsesTextOf flattens Responses content (string, block array, or nested
// arrays) into plain text.
func responsesTextOf(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			b.WriteString(responsesTextOf(item))
		}
		return b.String()
	case map[string]any:
		for _, key := range []string{"text", "output_text", "input_text", "content", "refusal", "summary"} {
			if s, ok := v[key].(string); ok && s != "" {
				return s
			}
		}
		return responsesTextOf(v["content"])
	default:
		return ""
	}
}

// responsesReasoningOf collects a reasoning item's thinking text.
func responsesReasoningOf(item map[string]any) string {
	for _, key := range []string{"summary", "content"} {
		if arr, ok := item[key].([]any); ok {
			var b strings.Builder
			for _, p := range arr {
				if m, ok := p.(map[string]any); ok {
					if s, ok := m["text"].(string); ok {
						b.WriteString(s)
					}
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	if s, ok := item["text"].(string); ok {
		return s
	}
	return ""
}

// responsesInputToMessages converts a Responses input value into Chat messages.
// Responses splits reasoning / message / function_call into sibling items while
// Chat wants them on one assistant message, so pending assistant content is
// accumulated and flushed.
func responsesInputToMessages(input any) []api.OpenAIMessage {
	if input == nil {
		return nil
	}
	if s, ok := input.(string); ok {
		return []api.OpenAIMessage{{Role: "user", Content: s}}
	}
	items, ok := input.([]any)
	if !ok {
		return []api.OpenAIMessage{{Role: "user", Content: input}}
	}

	var messages []api.OpenAIMessage
	var pending *api.OpenAIMessage

	ensurePending := func() *api.OpenAIMessage {
		if pending == nil {
			pending = &api.OpenAIMessage{Role: "assistant"}
		}
		return pending
	}
	flush := func() {
		if pending == nil {
			return
		}
		m := *pending
		// Drop an assistant message that ended up with nothing to say.
		if m.Content == nil && len(m.ToolCalls) == 0 {
			pending = nil
			return
		}
		messages = append(messages, m)
		pending = nil
	}

	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			if s, ok := raw.(string); ok {
				flush()
				messages = append(messages, api.OpenAIMessage{Role: "user", Content: s})
			}
			continue
		}

		typ, _ := item["type"].(string)
		if typ == "" {
			// OpenAI's EasyInputMessage makes type optional: {role, content}.
			if _, hasRole := item["role"]; hasRole {
				typ = "message"
			}
		}

		switch typ {
		case "reasoning":
			if t := responsesReasoningOf(item); t != "" {
				ensurePending().ReasoningContent = strPtr(t)
			}

		case "message":
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			content := item["content"]
			if content == nil {
				content = item["text"]
			}
			if role == "assistant" {
				ensurePending().Content = content
			} else {
				flush()
				messages = append(messages, api.OpenAIMessage{Role: role, Content: content})
			}

		case "function_call":
			id, _ := item["call_id"].(string)
			if id == "" {
				id, _ = item["id"].(string)
			}
			if id == "" {
				id = "call_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
			}
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			if args == "" {
				args = "{}"
			}
			p := ensurePending()
			p.ToolCalls = append(p.ToolCalls, api.ToolCall{
				ID:       id,
				Type:     "function",
				Function: api.FunctionCall{Name: name, Arguments: args},
			})

		case "function_call_output":
			flush()
			id, _ := item["call_id"].(string)
			if id == "" {
				id, _ = item["id"].(string)
			}
			// Keep the raw output (text and/or images) on the tool message;
			// ConvertMessages splits it so base64 never becomes prose.
			messages = append(messages, api.OpenAIMessage{
				Role:       "tool",
				ToolCallID: id,
				Content:    item["output"],
			})

		case "":
			// Untyped item with no role: fall back to text.
			if text := responsesTextOf(item["content"]); text != "" {
				flush()
				messages = append(messages, api.OpenAIMessage{Role: "user", Content: text})
			}

		default:
			log.Printf("[WARN] Unknown Responses input item type %q", typ)
		}
	}
	flush()
	return messages
}

// countImageURLs counts data/image references in a message list without
// serializing them.
func countImageURLs(msgs []api.OpenAIMessage) int {
	n := 0
	for _, m := range msgs {
		switch v := m.Content.(type) {
		case []any:
			for _, part := range v {
				if pm, ok := part.(map[string]any); ok {
					if typ, _ := pm["type"].(string); typ == "image_url" || typ == "input_image" || typ == "image" {
						n++
					}
				}
			}
		}
	}
	return n
}

// HandleModels handles the /v1/models endpoint.
// Returns only the models permitted for the requesting API key.
func (p *Proxy) HandleModels(w http.ResponseWriter, r *http.Request) {
	keyDef := p.keyDefFn(r)

	allModels := []api.OpenAIModel{
		// MoonshotAI
		{ID: "moonshotai/Kimi-K2.6", Object: "model", Created: 0, OwnedBy: "moonshotai"},
		{ID: "moonshotai/Kimi-K2.5", Object: "model", Created: 0, OwnedBy: "moonshotai"},
		// ZhipuAI
		{ID: "zai-org/GLM-5.1", Object: "model", Created: 0, OwnedBy: "zhipuai"},
		{ID: "zai-org/GLM-5", Object: "model", Created: 0, OwnedBy: "zhipuai"},
		// MiniMaxAI
		{ID: "MiniMaxAI/MiniMax-M2.7", Object: "model", Created: 0, OwnedBy: "minimaxai"},
		{ID: "MiniMaxAI/MiniMax-M2.5", Object: "model", Created: 0, OwnedBy: "minimaxai"},
		{ID: "MiniMaxAI/MiniMax-M3", Object: "model", Created: 0, OwnedBy: "minimaxai"},
		// DeepSeek
		{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 0, OwnedBy: "deepseek"},
		{ID: "deepseek/deepseek-v4-flash", Object: "model", Created: 0, OwnedBy: "deepseek"},
		// Qwen
		{ID: "Qwen/Qwen3.6-Max-Preview", Object: "model", Created: 0, OwnedBy: "qwen"},
		{ID: "Qwen/Qwen3.6-Plus", Object: "model", Created: 0, OwnedBy: "qwen"},
		// StepFun
		{ID: "stepfun/Step-3.5-Flash", Object: "model", Created: 0, OwnedBy: "stepfun"},
		{ID: "stepfun/Step-3.7-Flash", Object: "model", Created: 0, OwnedBy: "stepfun"},
		// Qwen (3.7 line)
		{ID: "Qwen/Qwen3.7-Max-Free", Object: "model", Created: 0, OwnedBy: "qwen"},
		{ID: "Qwen/Qwen3.7-Max", Object: "model", Created: 0, OwnedBy: "qwen"},
		// Xiaomi MiMo
		{ID: "xiaomi/mimo-v2.5-pro", Object: "model", Created: 0, OwnedBy: "xiaomi"},
		{ID: "xiaomi/mimo-v2.5", Object: "model", Created: 0, OwnedBy: "xiaomi"},
		// Google
		{ID: "google/gemini-3.1-flash-lite", Object: "model", Created: 0, OwnedBy: "google"},
	}

	// Filter by allowed models if the key has a restriction
	if keyDef != nil && len(keyDef.Models) > 0 {
		allowed := make(map[string]bool, len(keyDef.Models))
		for _, m := range keyDef.Models {
			allowed[strings.ToLower(m)] = true
		}
		filtered := make([]api.OpenAIModel, 0, len(allowed))
		for _, m := range allModels {
			if allowed[strings.ToLower(m.ID)] {
				filtered = append(filtered, m)
			}
		}
		allModels = filtered
	}

	models := api.OpenAIModelList{
		Object: "list",
		Data:   allModels,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}
