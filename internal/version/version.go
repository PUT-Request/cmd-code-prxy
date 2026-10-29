package version

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// VersionInfo holds the version information from npm registry
type VersionInfo struct {
	Version     string
	LastUpdated time.Time
}

var (
	versionInfo *VersionInfo
	versionMu   sync.RWMutex
)

const versionCacheDuration = 30 * time.Minute

// GetCommandCodeVersion fetches the latest version from npm registry
// It caches the result for 30 minutes to avoid excessive API calls.
//
// The fetch holds the lock so concurrent callers share one in-flight
// request (bounded by npmFetchTimeout). Failures are negative-cached for
// the same window: an unreachable npm registry costs one bounded wait per
// 30 minutes instead of stalling every upstream request.
func GetCommandCodeVersion() string {
	versionMu.Lock()
	defer versionMu.Unlock()

	// Return cached version if still valid
	if versionInfo != nil && time.Since(versionInfo.LastUpdated) < versionCacheDuration {
		return versionInfo.Version
	}

	newInfo, err := fetchVersionFromNPM()
	if err != nil {
		// Negative-cache: keep the last known version (or "unknown") and
		// retry only after the cache window.
		v := "unknown"
		if versionInfo != nil {
			v = versionInfo.Version
		}
		versionInfo = &VersionInfo{Version: v, LastUpdated: time.Now()}
		return v
	}

	versionInfo = newInfo
	return newInfo.Version
}

// npmFetchTimeout bounds the npm registry lookup so a stalled network can
// never hang request handling or startup.
const npmFetchTimeout = 5 * time.Second

// fetchVersionFromNPM fetches the latest version from npm registry
func fetchVersionFromNPM() (*VersionInfo, error) {
	client := &http.Client{Timeout: npmFetchTimeout}
	resp, err := client.Get("https://registry.npmjs.org/command-code/latest")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch version: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("npm registry returned %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse version: %w", err)
	}

	version, ok := result["version"].(string)
	if !ok {
		return nil, fmt.Errorf("version field not found or invalid")
	}

	return &VersionInfo{
		Version:     version,
		LastUpdated: time.Now(),
	}, nil
}

// init warms the version cache in the background so the first upstream
// request does not pay the fetch. Failures are left to
// GetCommandCodeVersion's bounded retry window.
func init() {
	go func() {
		if info, err := fetchVersionFromNPM(); err == nil {
			versionMu.Lock()
			versionInfo = info
			versionMu.Unlock()
		}
	}()
}
