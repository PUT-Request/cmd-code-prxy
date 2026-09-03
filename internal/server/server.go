package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/proxy"
)

const defaultPort = "55990"
const defaultHost = "127.0.0.1"

// Server represents the HTTP server
type Server struct {
	Port         string
	Host         string
	Proxy        *proxy.Proxy
	Handler      http.Handler
	ServerAPIKey string
}

// NewServer creates a new server instance
func NewServer(proxy *proxy.Proxy) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", logger(proxy.HandleChatCompletions))
	mux.HandleFunc("/chat/completions", logger(proxy.HandleChatCompletions))
	mux.HandleFunc("/v1/responses", logger(proxy.HandleResponses))
	mux.HandleFunc("/v1/models", logger(proxy.HandleModels))
	mux.HandleFunc("/health", logger(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))

	return &Server{
		Port:    defaultPort,
		Host:    defaultHost,
		Proxy:   proxy,
		Handler: mux,
	}
}

// SetServerAPIKey sets the API key required to access the exposed API.
// Endpoints (except /health) reject requests without this key.
func (s *Server) SetServerAPIKey(key string) {
	s.ServerAPIKey = key
	s.Handler = withAuth(s.ServerAPIKey, s.Handler)
}

// withAuth wraps the handler with API key authentication for all endpoints
// except /health, which is kept open for health checks.
func withAuth(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		auth = strings.TrimSpace(auth)
		if auth == "" || apiKey == "" || auth != apiKey {
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
			return
		}

		next.ServeHTTP(w, r)
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
