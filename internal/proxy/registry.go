package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

// defaultModels is the cold-start fallback for /v1/models when the upstream
// provider list has never been fetched successfully.
var defaultModels = []api.OpenAIModel{
	{ID: "moonshotai/Kimi-K2.6", Object: "model", OwnedBy: "moonshotai"},
	{ID: "moonshotai/Kimi-K2.5", Object: "model", OwnedBy: "moonshotai"},
	{ID: "zai-org/GLM-5.1", Object: "model", OwnedBy: "zhipuai"},
	{ID: "zai-org/GLM-5", Object: "model", OwnedBy: "zhipuai"},
	{ID: "MiniMaxAI/MiniMax-M2.7", Object: "model", OwnedBy: "minimaxai"},
	{ID: "MiniMaxAI/MiniMax-M2.5", Object: "model", OwnedBy: "minimaxai"},
	{ID: "MiniMaxAI/MiniMax-M3", Object: "model", OwnedBy: "minimaxai"},
	{ID: "deepseek/deepseek-v4-pro", Object: "model", OwnedBy: "deepseek"},
	{ID: "deepseek/deepseek-v4-flash", Object: "model", OwnedBy: "deepseek"},
	{ID: "Qwen/Qwen3.6-Max-Preview", Object: "model", OwnedBy: "qwen"},
	{ID: "Qwen/Qwen3.6-Plus", Object: "model", OwnedBy: "qwen"},
	{ID: "stepfun/Step-3.5-Flash", Object: "model", OwnedBy: "stepfun"},
	{ID: "stepfun/Step-3.7-Flash", Object: "model", OwnedBy: "stepfun"},
	{ID: "Qwen/Qwen3.7-Max-Free", Object: "model", OwnedBy: "qwen"},
	{ID: "Qwen/Qwen3.7-Max", Object: "model", OwnedBy: "qwen"},
	{ID: "xiaomi/mimo-v2.5-pro", Object: "model", OwnedBy: "xiaomi"},
	{ID: "xiaomi/mimo-v2.5", Object: "model", OwnedBy: "xiaomi"},
	{ID: "google/gemini-3.1-flash-lite", Object: "model", OwnedBy: "google"},
}

// defaultRefreshInterval is how often the registry re-fetches the upstream
// model list when not configured.
const defaultRefreshInterval = 5 * time.Minute

// upstreamModel is one entry of GET {base}/provider/v1/models.
type upstreamModel struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
}

// ModelRegistry keeps the live upstream model list. It refreshes lazily and
// in the background, falls back to the last good list, and finally to
// defaultModels, so /v1/models never breaks when upstream is down.
type ModelRegistry struct {
	baseURL string
	// keyProvider returns usable upstream CommandCode keys (may be empty).
	keyProvider func() []string
	client      *http.Client
	interval    time.Duration

	mu        sync.RWMutex
	models    []api.OpenAIModel
	lastFetch time.Time
	// aliases maps a derived short name (last path segment, lowercase) to the
	// canonical upstream ID. Only the first model with a given short name wins.
	aliases map[string]string
	// fetching is a single-flight guard so concurrent requests do not
	// stampede the upstream.
	fetching sync.Mutex
	fetched  bool
}

// NewRegistry creates a registry. interval <= 0 uses the default.
func NewRegistry(baseURL string, keyProvider func() []string, interval time.Duration) *ModelRegistry {
	if interval <= 0 {
		interval = defaultRefreshInterval
	}
	return &ModelRegistry{
		baseURL:     strings.TrimRight(baseURL, "/"),
		keyProvider: keyProvider,
		client:      &http.Client{Timeout: 15 * time.Second},
		interval:    interval,
	}
}

// Models returns the current model list, refreshing in the background when
// the cache is stale. Never returns nil: falls back to defaultModels.
func (r *ModelRegistry) Models() []api.OpenAIModel {
	r.mu.RLock()
	stale := time.Since(r.lastFetch) >= r.interval
	models := r.models
	r.mu.RUnlock()

	if stale {
		go r.Refresh() // non-blocking; next call sees the new list
	}
	if len(models) == 0 {
		return defaultModels
	}
	return models
}

// FreeModels returns the IDs of pool-eligible models: any registry model
// whose ID contains "free" (case-insensitive), e.g. ":free" or "-free".
func (r *ModelRegistry) FreeModels() []string {
	var out []string
	for _, m := range r.Models() {
		if strings.Contains(strings.ToLower(m.ID), "free") {
			out = append(out, m.ID)
		}
	}
	return out
}

// Resolve maps a client-supplied model name to a canonical upstream ID.
// Callers run their own override table first; this handles registry
// exact matches and derived short aliases.
func (r *ModelRegistry) Resolve(name string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.models) == 0 {
		return name, false
	}
	lower := strings.ToLower(name)
	for _, m := range r.models {
		if strings.EqualFold(m.ID, name) {
			return m.ID, true
		}
	}
	if id, ok := r.aliases[lower]; ok {
		return id, true
	}
	return name, false
}

// Refresh fetches the upstream model list now. Safe to call concurrently.
func (r *ModelRegistry) Refresh() {
	r.fetching.Lock()
	defer r.fetching.Unlock()
	r.doRefresh()
}

// doRefresh fetches the model list with a single upstream key. The model
// list does not depend on which key asks, so one attempt per refresh cycle
// is enough — iterating every configured key would multiply worst-case
// latency by the pool size for no benefit.
func (r *ModelRegistry) doRefresh() {
	if r.keyProvider == nil {
		return
	}
	keys := r.keyProvider()
	if len(keys) == 0 {
		return
	}

	models, err := r.fetch(keys[0])
	if err == nil && len(models) == 0 {
		err = fmt.Errorf("empty model list")
	}
	if err == nil {
		r.mu.Lock()
		r.models = models
		r.aliases = buildAliases(models)
		r.lastFetch = time.Now()
		r.fetched = true
		r.mu.Unlock()
		log.Printf("[MODELS] Refreshed model list from upstream: %d models", len(models))
		return
	}
	r.mu.RLock()
	had := r.fetched
	r.mu.RUnlock()
	if !had {
		log.Printf("[WARN] Model list fetch failed (%v); serving built-in fallback", err)
	} else {
		log.Printf("[WARN] Model list refresh failed (%v); keeping last good list", err)
	}
	// Advance lastFetch on failure too so a down upstream is not hammered.
	r.mu.Lock()
	r.lastFetch = time.Now()
	r.mu.Unlock()
}

// fetch pulls the model list with one upstream key.
func (r *ModelRegistry) fetch(key string) ([]api.OpenAIModel, error) {
	req, err := http.NewRequest(http.MethodGet, r.baseURL+"/provider/v1/models", nil)
	if err != nil {
		return nil, err
	}
	setCCHeaders(req, key)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyReadLimit))
		return nil, &upstreamHTTPError{status: resp.StatusCode, body: string(body)}
	}
	var payload struct {
		Data []upstreamModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]api.OpenAIModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		owner := m.ID
		if i := strings.LastIndex(m.ID, "/"); i >= 0 && i+1 < len(m.ID) {
			owner = m.ID[:i]
		}
		out = append(out, api.OpenAIModel{
			ID:            m.ID,
			Object:        "model",
			OwnedBy:       owner,
			ContextLength: m.ContextLength,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// buildAliases derives short names from canonical IDs: "moonshotai/Kimi-K3"
// also resolves as "kimi-k3". First model wins on collision.
func buildAliases(models []api.OpenAIModel) map[string]string {
	aliases := make(map[string]string, len(models))
	for _, m := range models {
		aliases[strings.ToLower(m.ID)] = m.ID
		if i := strings.LastIndex(m.ID, "/"); i >= 0 && i+1 < len(m.ID) {
			short := strings.ToLower(m.ID[i+1:])
			if _, exists := aliases[short]; !exists {
				aliases[short] = m.ID
			}
		}
	}
	return aliases
}

// upstreamHTTPError carries an upstream HTTP failure for logging.
type upstreamHTTPError struct {
	status int
	body   string
}

func (e *upstreamHTTPError) Error() string {
	return "upstream " + http.StatusText(e.status) + ": " + e.body
}
