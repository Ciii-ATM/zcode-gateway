package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := envOr("ZCODE_GATEWAY_ADDR", "127.0.0.1:8787")
	dataDir := envOr("ZCODE_GATEWAY_DATA", "data")
	baseURL := envOr("ZCODE_GATEWAY_BASE_URL", "http://"+addr)
	if strings.HasPrefix(addr, ":") {
		baseURL = "http://127.0.0.1" + addr
	}

	store, err := NewStore(dataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()
	defer store.Save()
	upstream, err := NewUpstream()
	if err != nil {
		log.Fatalf("upstream: %v", err)
	}
	server := NewServer(store, upstream, baseURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go server.quotaLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", server.handleLogin)
	mux.HandleFunc("/api/auth/logout", server.handleLogout)
	mux.HandleFunc("/api/auth/session", server.handleSession)
	mux.HandleFunc("/api/overview", server.requireAdmin(server.handleOverview))
	mux.HandleFunc("/api/accounts", server.requireAdmin(server.handleAccounts))
	mux.HandleFunc("/api/accounts/import", server.requireAdmin(server.handleImport))
	mux.HandleFunc("/api/accounts/oauth/start", server.requireAdmin(server.handleAccounts))
	mux.HandleFunc("/api/accounts/oauth/status", server.requireAdmin(server.handleOAuthStatus))
	mux.HandleFunc("/api/accounts/oauth/callback", server.requireAdmin(server.handleOAuthCallback))
	mux.HandleFunc("/api/accounts/", server.requireAdmin(server.handleAccountByID))
	mux.HandleFunc("/api/logs", server.requireAdmin(server.handleLogs))
	mux.HandleFunc("/api/logs/", server.requireAdmin(server.handleLogs))
	mux.HandleFunc("/api/settings", server.requireAdmin(server.handleSettings))
	mux.HandleFunc("/v1/messages", server.handleMessages)
	mux.HandleFunc("/v1/models", server.handleModels)
	mux.HandleFunc("/health", server.handleHealth)
	mux.Handle("/", server.webHandler())

	handler := cors(logRequests(mux))
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if proxy := upstream.ProxyURL(); proxy != "" {
		log.Printf("upstream proxy: %s", proxyDisplay(proxy))
	}
	log.Printf("zcode-gateway %s listening on %s (data=%s)", appVersion, baseURL, dataDir)
	if server.adminGenerated {
		log.Printf("admin panel password (set ZCODE_ADMIN_PASSWORD to override): %s", server.adminPass)
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/api/") {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r)
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			if os.Getenv("ZCODE_GATEWAY_LOG") != "0" {
				log.Printf("%s %s %d %s", r.Method, r.URL.Path, status, time.Since(start).Round(time.Millisecond))
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}
