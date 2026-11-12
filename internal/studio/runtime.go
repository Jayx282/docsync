package studio

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/dashboard"
	"github.com/Jayx282/docsync/internal/engine"
)

// RuntimeStatus describes the deployed pipeline.
type RuntimeStatus struct {
	State      string    `json:"state"` // stopped, starting, running, error
	Design     string    `json:"design,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	ConfigPath string    `json:"config_path,omitempty"`
}

// Runtime runs one deployed pipeline (Phase 1: one at a time) and exposes its dashboard.
type Runtime struct {
	Log *slog.Logger

	mu      sync.Mutex
	st      RuntimeStatus
	cancel  context.CancelFunc
	done    chan struct{}
	handler http.Handler
}

// Start stops any running pipeline and starts one for cfg.
func (rt *Runtime) Start(cfg *config.Config, design, cfgPath string) {
	rt.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	rt.mu.Lock()
	rt.st = RuntimeStatus{State: "starting", Design: design, StartedAt: time.Now().UTC(), ConfigPath: cfgPath}
	rt.cancel, rt.done = cancel, done
	rt.mu.Unlock()
	log := rt.Log
	if log == nil {
		log = slog.Default()
	}
	go func() {
		defer close(done)
		eng, err := engine.Open(ctx, engine.Options{Config: cfg, Logger: log.With("pipeline", design)})
		if err != nil {
			rt.fail(err)
			return
		}
		h, err := dashboard.Handler(eng, cfg)
		if err != nil {
			eng.Close()
			rt.fail(err)
			return
		}
		rt.mu.Lock()
		rt.handler, rt.st.State = h, "running"
		rt.mu.Unlock()
		runErr := eng.Run(ctx)
		eng.Close()
		rt.mu.Lock()
		rt.handler = nil
		if runErr != nil {
			rt.st.State, rt.st.Error = "error", runErr.Error()
		} else if rt.st.State == "running" {
			rt.st.State = "stopped"
		}
		rt.mu.Unlock()
	}()
}

func (rt *Runtime) fail(err error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.st.State, rt.st.Error = "error", err.Error()
}

// Stop stops the running pipeline, if any, and waits for it to close.
func (rt *Runtime) Stop() {
	rt.mu.Lock()
	cancel, done := rt.cancel, rt.done
	rt.cancel, rt.done = nil, nil
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	rt.mu.Lock()
	if rt.st.State == "running" || rt.st.State == "starting" {
		rt.st.State = "stopped"
	}
	rt.mu.Unlock()
}

// Status returns the pipeline's state.
func (rt *Runtime) Status() RuntimeStatus {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.st.State == "" {
		return RuntimeStatus{State: "stopped"}
	}
	return rt.st
}

// ServeHTTP serves the running pipeline's dashboard and admin API.
func (rt *Runtime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.mu.Lock()
	h := rt.handler
	rt.mu.Unlock()
	if h == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"no pipeline is running; deploy a design first"}`)
		return
	}
	h.ServeHTTP(w, r)
}

type activeFile struct {
	Design     string    `json:"design"`
	ConfigPath string    `json:"config_path"`
	DeployedAt time.Time `json:"deployed_at"`
	// Server and Database identify where the pipeline was deployed. The config only
	// names the environment variable holding the connection string, so a restart
	// must not follow that variable to a different database.
	Server   string `json:"server"`
	Database string `json:"database"`
}

// Deploy compiles a design, writes its config (keeping a history copy) and starts it.
func (s *Service) Deploy(ctx context.Context, d *Design, rt *Runtime) (RuntimeStatus, error) {
	comp, err := s.Compile(ctx, d)
	if err != nil {
		return RuntimeStatus{}, err
	}
	dir := s.PipelineDir(d.Name)
	if err := os.MkdirAll(filepath.Join(dir, "history"), 0o755); err != nil {
		return RuntimeStatus{}, err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	designJSON, _ := json.MarshalIndent(d, "", "  ")
	files := map[string][]byte{
		filepath.Join(dir, "docsync.yaml"):                   []byte(comp.YAML),
		filepath.Join(dir, "design.json"):                    designJSON,
		filepath.Join(dir, "history", stamp+".docsync.yaml"): []byte(comp.YAML),
		filepath.Join(dir, "history", stamp+".design.json"):  designJSON,
	}
	for p, b := range files {
		if err := writeAtomic(p, b); err != nil {
			return RuntimeStatus{}, err
		}
	}
	cfgPath := filepath.Join(dir, "docsync.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return RuntimeStatus{}, err
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return RuntimeStatus{}, err
	}
	act, _ := json.MarshalIndent(activeFile{Design: d.Name, ConfigPath: cfgPath, DeployedAt: time.Now().UTC(), Server: snap.Server, Database: snap.Database}, "", "  ")
	if err := writeAtomic(filepath.Join(s.BaseDir, "active.json"), act); err != nil {
		return RuntimeStatus{}, err
	}
	rt.Start(cfg, d.Name, cfgPath)
	return rt.Status(), nil
}

// StartActive restarts the last deployed pipeline (after a studio restart), unless
// this studio is connected to a different server or database than it was deployed to.
func (s *Service) StartActive(ctx context.Context, rt *Runtime) error {
	b, err := os.ReadFile(filepath.Join(s.BaseDir, "active.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var act activeFile
	if err := json.Unmarshal(b, &act); err != nil {
		return err
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if !strings.EqualFold(act.Server, snap.Server) || !strings.EqualFold(act.Database, snap.Database) {
		return fmt.Errorf("pipeline %s was deployed to %s on %s, but this studio is connected to %s on %s; not restarting it (deploy it again here, or start the studio with the original connection)",
			act.Design, act.Database, act.Server, snap.Database, snap.Server)
	}
	cfg, err := config.Load(act.ConfigPath)
	if err != nil {
		return err
	}
	rt.Start(cfg, act.Design, act.ConfigPath)
	return nil
}

// ClearActive forgets the deployed pipeline, so it does not restart with the studio.
func (s *Service) ClearActive() error {
	err := os.Remove(filepath.Join(s.BaseDir, "active.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
