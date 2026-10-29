package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
	"github.com/dev2k6/command-code-proxy-server/internal/config"
)

// --- model LRU ---

func TestModelLRURotatesOnSuccess(t *testing.T) {
	l := newModelLRU(time.Minute)
	pool := []string{"a", "b", "c"}

	if got := strings.Join(l.Candidates(pool), ","); got != "a,b,c" {
		t.Fatalf("initial order = %q, want a,b,c", got)
	}
	l.MarkOK("a")
	if got := strings.Join(l.Candidates(pool), ","); got != "b,c,a" {
		t.Fatalf("after MarkOK(a) = %q, want b,c,a", got)
	}
}

func TestModelLRUCooldownSkipsAndLRUSorts(t *testing.T) {
	l := newModelLRU(time.Minute)
	pool := []string{"a", "b", "c"}

	l.MarkFailed("a")
	l.MarkFailed("b")
	if got := strings.Join(l.Candidates(pool), ","); got != "c,a,b" {
		t.Fatalf("cooled order = %q, want c,a,b (healthy first, cooled LRU)", got)
	}
	// Fresh store, zero cooldown window: failure only affects rotation.
	l2 := newModelLRU(0)
	l2.MarkFailed("x")
	if got := strings.Join(l2.Candidates([]string{"x", "c"}), ","); got != "x,c" {
		t.Fatalf("zero-cooldown order = %q, want x,c (rotation only, no skip)", got)
	}
}

func TestModelLRUDropsRemovedPoolModels(t *testing.T) {
	l := newModelLRU(time.Minute)
	l.Candidates([]string{"a", "b", "gone"})
	if got := strings.Join(l.Candidates([]string{"a", "b", "new"}), ","); got != "a,b,new" {
		t.Fatalf("pool swap order = %q, want a,b,new", got)
	}
}

// --- free-model pool ---

// registryWithModels builds a registry whose fetch endpoint returns ids.
func registryWithModels(t *testing.T, ids ...string) *ModelRegistry {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id})
		}
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	reg := NewRegistry(srv.URL, func() []string { return []string{"cc-key"} }, time.Hour)
	reg.Refresh()
	return reg
}

func TestFreeModelsOnlyIDsContainingFree(t *testing.T) {
	reg := registryWithModels(t,
		"stealth/glyph-cluster:free",
		"poolside/laguna-s-2.1-free",
		"reflection/beam:free",
		"tencent/hy3-paid",
		"deepseek/deepseek-v4-flash",
	)
	got := reg.FreeModels()
	want := map[string]bool{
		"stealth/glyph-cluster:free": true,
		"poolside/laguna-s-2.1-free": true,
		"reflection/beam:free":       true,
	}
	if len(got) != len(want) {
		t.Fatalf("FreeModels = %v, want exactly the 3 'free' models", got)
	}
	for _, m := range got {
		if !want[m] {
			t.Fatalf("FreeModels included non-free model %q", m)
		}
	}
}

func TestRegistryFallbackToDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	reg := NewRegistry(srv.URL, func() []string { return []string{"cc-key"} }, time.Hour)
	reg.Refresh() // fails synchronously
	models := reg.Models()
	if len(models) != len(defaultModels) {
		t.Fatalf("fallback list = %d models, want built-in %d", len(models), len(defaultModels))
	}
}

func TestRegistryResolvesExactAndAlias(t *testing.T) {
	reg := registryWithModels(t, "moonshotai/Kimi-K3", "meta/muse-spark-1.3")
	if id, ok := reg.Resolve("moonshotai/Kimi-K3"); !ok || id != "moonshotai/Kimi-K3" {
		t.Fatalf("exact resolve = %q %v", id, ok)
	}
	if id, ok := reg.Resolve("kimi-k3"); !ok || id != "moonshotai/Kimi-K3" {
		t.Fatalf("alias resolve = %q %v", id, ok)
	}
	if id, ok := reg.Resolve("muse-spark-1.3"); !ok || id != "meta/muse-spark-1.3" {
		t.Fatalf("alias resolve = %q %v", id, ok)
	}
	if _, ok := reg.Resolve("does-not-exist"); ok {
		t.Fatal("unknown model resolved")
	}
}

// --- limits ---

func TestEstimateTokensStripsBase64(t *testing.T) {
	msgs := []api.OpenAIMessage{{Role: "user", Content: "hi"}}
	text := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	base := EstimateTokens(text, msgs)
	if base <= 0 {
		t.Fatal("non-positive estimate")
	}
	// Same text with a giant base64 image must not scale with the base64 size.
	blob := strings.Repeat("QUFB", 100000) // 300k chars of base64
	withImage := []byte(fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`, blob))
	msgsImg := []api.OpenAIMessage{{Role: "user", Content: []api.ContentPart{{
		Type: "image_url", ImageURL: &api.ImageURL{URL: "data:image/png;base64," + blob},
	}}}}
	est := EstimateTokens(withImage, msgsImg)
	if est > base+5000 {
		t.Fatalf("base64 leaked into estimate: %d vs base %d", est, base)
	}
	if est < imageTokenEstimate {
		t.Fatalf("image flat cost missing: %d", est)
	}
}

func TestCheckLimitsRejectsOversized(t *testing.T) {
	p := NewProxy(func(r *http.Request) *config.APIKeyDef { return nil })
	p.SetLimits(0, 0) // defaults: 256k / 32k

	huge := api.OpenAIChatRequest{Messages: []api.OpenAIMessage{{
		Role: "user", Content: strings.Repeat("a", 1_200_000), // ~300k tokens
	}}}
	body := []byte(strings.Repeat("a", 1_200_000))
	if err := p.checkLimits(body, huge); err == nil {
		t.Fatal("oversized input not rejected")
	}

	tooMuchOut := 50000
	small := api.OpenAIChatRequest{MaxTokens: &tooMuchOut}
	if err := p.checkLimits([]byte("{}"), small); err == nil {
		t.Fatal("oversized max_tokens not rejected")
	}

	ok := 32000
	fine := api.OpenAIChatRequest{MaxTokens: &ok}
	if err := p.checkLimits([]byte("{}"), fine); err != nil {
		t.Fatalf("in-limit request rejected: %v", err)
	}
}

func TestEffectiveMaxTokensClampsDefault(t *testing.T) {
	p := NewProxy(func(r *http.Request) *config.APIKeyDef { return nil })
	p.SetLimits(0, 0)
	if got := p.effectiveMaxTokens(api.OpenAIChatRequest{}); got != DefaultOutputTokenLimit {
		t.Fatalf("default = %d, want %d", got, DefaultOutputTokenLimit)
	}
	v := 1000
	if got := p.effectiveMaxTokens(api.OpenAIChatRequest{MaxTokens: &v}); got != 1000 {
		t.Fatalf("client value = %d, want 1000", got)
	}
}

// --- router request path ---

// routerUpstream simulates CommandCode: model-a always 400s, model-b streams
// a normal reply. It records the models (and output budgets) it saw.
type routerUpstream struct {
	mu        sync.Mutex
	models    []string
	maxTokens []int
	srv       *httptest.Server
}

func newRouterUpstream(t *testing.T) *routerUpstream {
	t.Helper()
	u := &routerUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Params struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&payload)
		u.mu.Lock()
		u.models = append(u.models, payload.Params.Model)
		u.maxTokens = append(u.maxTokens, payload.Params.MaxTokens)
		u.mu.Unlock()

		if strings.Contains(payload.Params.Model, "model-a") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"success":false,"error":{"code":"BAD_REQUEST","message":"model broken"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "{\"type\":\"text-delta\",\"text\":\"pong\"}\n")
		fmt.Fprint(w, "{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":10,\"outputTokens\":5}}\n")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *routerUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

func newRouterTestProxy(t *testing.T, upstream *routerUpstream, pool []string) *Proxy {
	t.Helper()
	p := NewProxy(func(r *http.Request) *config.APIKeyDef {
		return &config.APIKeyDef{Key: "sk-test-1111", CommandCodeKeys: []string{"cc-key-1"}}
	})
	p.BaseURL = upstream.srv.URL
	p.SetLimits(0, 0)
	p.SetRouter(true, pool, 5, time.Minute)
	return p
}

func chatRequest(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	p.HandleChatCompletions(w, req)
	return w
}

func TestRouterFailsOverToSecondModel(t *testing.T) {
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, []string{"prov/model-a:free", "prov/model-b:free"})

	w := chatRequest(t, p, `{"model":"free:auto","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("missing streamed content: %s", w.Body.String())
	}
	if got := strings.Join(up.seen(), ","); got != "prov/model-a:free,prov/model-b:free" {
		t.Fatalf("upstream saw %q, want model-a then model-b", got)
	}
}

func TestRouterRotatesAfterFailure(t *testing.T) {
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, []string{"prov/model-a:free", "prov/model-b:free"})
	body := `{"model":"free:auto","messages":[{"role":"user","content":"hi"}],"stream":false}`

	if w := chatRequest(t, p, body); w.Code != http.StatusOK {
		t.Fatalf("first request status = %d", w.Code)
	}
	// model-a is cooling: the next request should go straight to model-b.
	if w := chatRequest(t, p, body); w.Code != http.StatusOK {
		t.Fatalf("second request status = %d", w.Code)
	}
	seen := up.seen()
	if len(seen) != 3 || seen[2] != "prov/model-b:free" {
		t.Fatalf("second request models = %v, want last = model-b (a cooled)", seen)
	}
}

func TestRouterUsageRecordsActualModel(t *testing.T) {
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, []string{"prov/model-a:free", "prov/model-b:free"})
	var gotModel string
	p.SetUsageRecorder(func(clientKey, model string, input, output int) { gotModel = model })

	body := `{"model":"free:auto","messages":[{"role":"user","content":"hi"}],"stream":false}`
	if w := chatRequest(t, p, body); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if gotModel != "prov/model-b:free" {
		t.Fatalf("usage model = %q, want the actual serving model", gotModel)
	}
}

func TestRouterDisabledRejects(t *testing.T) {
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, []string{"prov/model-b:free"})
	p.SetRouter(false, nil, 5, time.Minute)

	w := chatRequest(t, p, `{"model":"free:auto","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when router disabled", w.Code)
	}
	if len(up.seen()) != 0 {
		t.Fatal("request reached upstream despite disabled router")
	}
}

func TestLimitsRejectBeforeUpstream(t *testing.T) {
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, nil)

	tooBig := `{"model":"prov/model-b:free","messages":[{"role":"user","content":"` +
		strings.Repeat("a", 1_200_000) + `"}]}`
	if w := chatRequest(t, p, tooBig); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized status = %d, want 400", w.Code)
	}
	tooMuchOut := `{"model":"prov/model-b:free","messages":[{"role":"user","content":"hi"}],"max_tokens":99999}`
	if w := chatRequest(t, p, tooMuchOut); w.Code != http.StatusBadRequest {
		t.Fatalf("max_tokens status = %d, want 400", w.Code)
	}
	if len(up.seen()) != 0 {
		t.Fatal("rejected requests reached upstream")
	}
}

func TestDefaultMaxTokensSentUpstream(t *testing.T) {
	up := newRouterUpstream(t)
	p := newRouterTestProxy(t, up, nil)
	body := `{"model":"prov/model-b:free","messages":[{"role":"user","content":"hi"}]}`
	if w := chatRequest(t, p, body); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.maxTokens) != 1 || up.maxTokens[0] != DefaultOutputTokenLimit {
		t.Fatalf("upstream max_tokens = %v, want [%d] (clamped default)", up.maxTokens, DefaultOutputTokenLimit)
	}
}

// --- /v1/models endpoint ---

func TestModelsEndpointRouterAndContext(t *testing.T) {
	reg := registryWithModels(t, "prov/model-a:free", "prov/model-b:free", "prov/plain")
	p := NewProxy(func(r *http.Request) *config.APIKeyDef { return nil })
	p.SetRegistry(reg)
	p.SetLimits(0, 0)
	p.SetRouter(true, nil, 5, time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	p.HandleModels(w, req)

	var list api.OpenAIModelList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	ids := map[string]api.OpenAIModel{}
	for _, m := range list.Data {
		ids[m.ID] = m
	}
	if _, ok := ids[RouterModelID]; !ok {
		t.Fatalf("/v1/models missing %s: %v", RouterModelID, ids)
	}
	if ids["prov/plain"].ContextLength != DefaultInputTokenLimit {
		t.Fatalf("context_length = %d, want %d", ids["prov/plain"].ContextLength, DefaultInputTokenLimit)
	}
	if ids[RouterModelID].OwnedBy != "commandcode-proxy" {
		t.Fatalf("router owned_by = %q", ids[RouterModelID].OwnedBy)
	}
}

func TestModelsEndpointAllowlistFiltersRouter(t *testing.T) {
	reg := registryWithModels(t, "prov/model-a:free")
	p := NewProxy(func(r *http.Request) *config.APIKeyDef {
		return &config.APIKeyDef{Key: "sk-1", Models: []string{RouterModelID}}
	})
	p.SetRegistry(reg)
	p.SetLimits(0, 0)
	p.SetRouter(true, nil, 5, time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	p.HandleModels(w, req)

	var list api.OpenAIModelList
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != RouterModelID {
		t.Fatalf("allowlisted view = %+v, want only %s", list.Data, RouterModelID)
	}
}

// --- allowlist with the virtual model ---

func TestAllowedModelAcceptsRouterID(t *testing.T) {
	p := NewProxy(func(r *http.Request) *config.APIKeyDef { return nil })
	def := &config.APIKeyDef{Key: "sk-1", Models: []string{RouterModelID}}
	if _, err := p.allowedModel(def, "free:auto"); err != nil {
		t.Fatalf("free:auto rejected by allowlist: %v", err)
	}
	if _, err := p.allowedModel(def, "gpt-5.5"); err == nil {
		t.Fatal("non-allowlisted model accepted")
	}
}

// --- registry wiring on the proxy ---

func TestAllowedModelUsesRegistryAliases(t *testing.T) {
	reg := registryWithModels(t, "meta/muse-spark-1.3")
	p := NewProxy(func(r *http.Request) *config.APIKeyDef { return nil })
	p.SetRegistry(reg)
	got, err := p.allowedModel(nil, "muse-spark-1.3")
	if err != nil {
		t.Fatalf("registry alias rejected: %v", err)
	}
	if got != "meta/muse-spark-1.3" {
		t.Fatalf("resolved = %q, want meta/muse-spark-1.3", got)
	}
}

func TestIsRouterModelAliases(t *testing.T) {
	for _, name := range []string{"free:auto", "FREE:AUTO", "auto", "free"} {
		if !IsRouterModel(name) {
			t.Fatalf("%q should select the router", name)
		}
	}
	for _, name := range []string{"gpt-5.5", "freemodel", "auto-magic"} {
		if IsRouterModel(name) {
			t.Fatalf("%q should not select the router", name)
		}
	}
}
