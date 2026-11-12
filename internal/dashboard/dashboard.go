// Package dashboard serves docsync's web dashboard, admin API and metrics.
package dashboard

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/engine"
)

//go:embed static
var static embed.FS

// Handler builds the HTTP handler for an engine.
func Handler(e *engine.Engine, cfg *config.Config) (http.Handler, error) {
	mux := http.NewServeMux()
	sub, err := fs.Sub(static, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, e.Status())
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"document":  cfg.Document,
			"routing":   cfg.Routing,
			"schema":    e.Schema(),
			"sinks":     cfg.Sinks,
			"source":    map[string]any{"poll_interval": cfg.Source.PollInterval, "assemble_budget": cfg.Source.AssembleBudget, "low_priority_share": cfg.Source.LowPriorityShare},
			"hashes":    map[string]string{"config": cfg.ContentHash()},
			"state_dir": cfg.ResolvePath(cfg.State.Dir),
		})
	})
	mux.HandleFunc("GET /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		ins, err := e.Inspect(ctx, r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, ins)
	})
	mux.HandleFunc("POST /api/actions/{action}", func(w http.ResponseWriter, r *http.Request) {
		kind := map[string]string{
			"pause": engine.CmdPause, "resume": engine.CmdResume, "rebuild": engine.CmdRebuild,
			"resnapshot": engine.CmdResnapshot, "new-epoch": engine.CmdNewEpoch, "compact": engine.CmdCompact,
		}[r.PathValue("action")]
		if kind == "" {
			writeError(w, http.StatusNotFound, errors.New("unknown action"))
			return
		}
		var body struct {
			IDs []string `json:"ids"`
			All bool     `json:"all"`
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		if err := e.Do(ctx, kind, body.IDs, body.All); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"result": "ok"})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		v := e.Status()
		healthy := !v.Paused && time.Since(v.LastCycleAt) < 2*time.Minute
		code := http.StatusOK
		if !healthy {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]any{"healthy": healthy, "paused": v.Paused, "last_cycle_at": v.LastCycleAt})
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(engine.NewGaugeCollector(e))
	mux.Handle("GET /metrics", promhttp.HandlerFor(prometheus.Gatherers{engine.Registry, reg}, promhttp.HandlerOpts{}))

	var h http.Handler = mux
	if cfg.Dashboard.BasicAuthUser != "" {
		pass := os.Getenv(cfg.Dashboard.BasicAuthPasswordEnv)
		if pass == "" {
			return nil, errors.New("dashboard: basic auth is configured but " + cfg.Dashboard.BasicAuthPasswordEnv + " is empty")
		}
		h = basicAuth(h, cfg.Dashboard.BasicAuthUser, pass)
	}
	return securityHeaders(h), nil
}

func basicAuth(next http.Handler, user, pass string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="docsync"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN") // docsync studio embeds the dashboard
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'self'")
		// Actions change state; refuse cross-site form posts.
		if r.Method == http.MethodPost && !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "actions require Content-Type: application/json", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": strings.TrimSpace(err.Error())})
}
