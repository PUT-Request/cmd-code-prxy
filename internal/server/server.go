package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/config"
	"github.com/dev2k6/command-code-proxy-server/internal/proxy"
)

const defaultPort = "55990"
const defaultHost = "127.0.0.1"

type contextKey string

const apiKeyDefContextKey contextKey = "api_key_def"

// GetAPIKeyDef retrieves the matched APIKeyDef from the request context.
// Returns nil if no key was matched (should not happen after auth middleware).
func GetAPIKeyDef(r *http.Request) *config.APIKeyDef {
	v, _ := r.Context().Value(apiKeyDefContextKey).(*config.APIKeyDef)
	return v
}

// Server represents the HTTP server
type Server struct {
	Port    string
	Host    string
	Proxy   *proxy.Proxy
	Handler http.Handler
}

// NewServer creates a new server instance with auth and routing wired up.
func NewServer(keyMap map[string]*config.APIKeyDef, p *proxy.Proxy) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", logger(p.HandleChatCompletions))
	mux.HandleFunc("/chat/completions", logger(p.HandleChatCompletions))
	mux.HandleFunc("/v1/responses", logger(p.HandleResponses))
	mux.HandleFunc("/v1/models", logger(p.HandleModels))
	mux.HandleFunc("/health", logger(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))

	s := &Server{
		Port:    defaultPort,
		Host:    defaultHost,
		Proxy:   p,
		Handler: withAuth(keyMap, mux),
	}
	return s
}

// withAuth wraps the handler with API key authentication.
// Health endpoint is exempt. On success the matched APIKeyDef is injected
// into the request context for downstream use.
func withAuth(keyMap map[string]*config.APIKeyDef, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		raw = strings.TrimSpace(raw)

		if raw == "" || keyMap == nil {
			rejectAuth(w)
			return
		}

		def, ok := keyMap[raw]
		if !ok {
			rejectAuth(w)
			return
		}

		ctx := context.WithValue(r.Context(), apiKeyDefContextKey, def)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func rejectAuth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "Invalid or missing API key. Provide a valid key via the Authorization header.",
			"type":    "authentication_error",
			"param":   nil,
			"code":    nil,
		},
	})
}

// SetPort sets the port for the server
func (s *Server) SetPort(port string) {
	if port != "" {
		s.Port = port
	}
}

// SetHost sets the host for the server
func (s *Server) SetHost(host string) {
	if host != "" {
		s.Host = host
	}
}

// GetPort returns the server port
func (s *Server) GetPort() string {
	return s.Port
}

// GetHost returns the server host
func (s *Server) GetHost() string {
	return s.Host
}

// logger is a middleware for logging requests
func logger(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("[%s] %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next(w, r)
		log.Printf("[%s] %s done in %v", r.Method, r.URL.Path, time.Since(start))
	}
}

// Start starts the HTTP server
func (s *Server) Start() {
	addr := s.Host + ":" + s.Port
	if err := http.ListenAndServe(addr, s.Handler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
