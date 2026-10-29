package proxy

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// RouterModelID is the virtual model that routes across the free pool.
const RouterModelID = "free:auto"

// routerAliases are the accepted spellings of the router model.
var routerAliases = map[string]bool{
	"free:auto": true,
	"auto":      true,
	"free":      true,
}

// IsRouterModel reports whether a canonical model name selects the router.
func IsRouterModel(name string) bool {
	return routerAliases[strings.ToLower(name)]
}

// modelLRU tracks per-model health with LRU rotation, mirroring the
// upstream-key cooldown ordering: healthy models in rotation order,
// cooling models at the back, least-recently-failed first.
type modelLRU struct {
	mu        sync.Mutex
	order     []string
	cooldown  map[string]time.Time
	cooldownD time.Duration
}

func newModelLRU(cooldown time.Duration) *modelLRU {
	return &modelLRU{cooldown: make(map[string]time.Time), cooldownD: cooldown}
}

// Candidates returns pool models in try order: healthy models in LRU
// rotation order first, then cooling models least-recently-failed first.
// The full pool is always returned so a request still tries something
// rather than failing fast.
func (l *modelLRU) Candidates(pool []string) []string {
	now := time.Now()
	type cooled struct {
		model string
		until time.Time
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	inPool := make(map[string]bool, len(pool))
	for _, m := range pool {
		inPool[m] = true
	}
	// Keep only models still in the pool; remember visit order.
	kept := make([]string, 0, len(pool))
	seen := make(map[string]bool, len(pool))
	for _, m := range l.order {
		if inPool[m] && !seen[m] {
			kept = append(kept, m)
			seen[m] = true
		}
	}
	// Append pool models not yet tracked (registry growth).
	for _, m := range pool {
		if !seen[m] {
			kept = append(kept, m)
			seen[m] = true
		}
	}
	l.order = kept

	var healthy []string
	var cooling []cooled
	for _, m := range l.order {
		if until, ok := l.cooldown[m]; ok && now.Before(until) {
			cooling = append(cooling, cooled{model: m, until: until})
			continue
		}
		healthy = append(healthy, m)
		delete(l.cooldown, m) // expired
	}
	sort.Slice(cooling, func(i, j int) bool { return cooling[i].until.Before(cooling[j].until) })

	out := make([]string, 0, len(pool))
	out = append(out, healthy...)
	for _, c := range cooling {
		out = append(out, c.model)
	}
	return out
}

// setCooldown updates the per-model cooldown window.
func (l *modelLRU) setCooldown(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cooldownD = d
}

// MarkOK records a successful use: the model moves to the back of the
// rotation so the next request prefers a different model.
func (l *modelLRU) MarkOK(model string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotateBack(model)
	delete(l.cooldown, model)
}

// MarkFailed starts a cooldown for the model and moves it to the back.
func (l *modelLRU) MarkFailed(model string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cooldownD > 0 {
		l.cooldown[model] = time.Now().Add(l.cooldownD)
	}
	l.rotateBack(model)
}

// rotateBack moves model to the end of the rotation order.
func (l *modelLRU) rotateBack(model string) {
	for i, m := range l.order {
		if m == model {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	l.order = append(l.order, model)
}
