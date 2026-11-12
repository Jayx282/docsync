// Command docsync keeps a document index (one document per root row, assembled from
// many SQL Server tables) incrementally up to date and publishes it as Parquet.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/spf13/cobra"

	"github.com/Jayx282/docsync/examples/stones"
	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/dashboard"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/engine"
	"github.com/Jayx282/docsync/internal/logfile"
	"github.com/Jayx282/docsync/internal/source/sqlserver"
	"github.com/Jayx282/docsync/internal/tools"
)

// version is set at build time: make build and make release pass -X main.version=...
var version = "dev"

func init() {
	// go install github.com/Jayx282/docsync/cmd/docsync@v0.1.0 records the module version.
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type globals struct {
	configPath string
	logFormat  string
	logLevel   string
	logFile    string
}

func rootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "docsync",
		Short:         "Incrementally sync SQL Server tables into per-entity documents, published as Parquet",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	def := os.Getenv("DOCSYNC_CONFIG")
	if def == "" {
		def = "docsync.yaml"
	}
	root.PersistentFlags().StringVarP(&g.configPath, "config", "c", def, "config file (env DOCSYNC_CONFIG)")
	root.PersistentFlags().StringVar(&g.logFormat, "log-format", "text", "log format: text or json")
	root.PersistentFlags().StringVar(&g.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	root.PersistentFlags().StringVar(&g.logFile, "log-file", "", "also write logs to this file (rotated at 10 MB, 5 old files kept)")

	root.AddCommand(
		runCmd(g), studioCmd(g), serviceCmd(g), initCmd(), doctorCmd(g), introspectCmd(g), fanoutCmd(g), assembleCmd(g), dumpCmd(),
		statusCmd(g), rebuildCmd(g),
		actionCmd(g, "pause", "Stop capturing changes (sinks keep publishing what is committed)"),
		actionCmd(g, "resume", "Resume after a pause"),
		actionCmd(g, "resnapshot", "Re-check every document against the database (only changed ones are emitted)"),
		actionCmd(g, "new-epoch", "Rebuild every document with the current schema; consumers reload from a new base"),
		actionCmd(g, "compact", "Write a fresh base snapshot to every sink"),
		&cobra.Command{Use: "version", Short: "Print the version", Run: func(*cobra.Command, []string) {
			fmt.Printf("docsync %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		}},
	)
	return root
}

func (g *globals) logger() *slog.Logger {
	var w io.Writer = os.Stderr
	if g.logFile != "" {
		if lf, err := logfile.Open(g.logFile, 10<<20, 5); err != nil {
			fmt.Fprintf(os.Stderr, "cannot open the log file: %v\n", err)
		} else {
			w = io.MultiWriter(lf, os.Stderr) // file first: it must get every line
		}
	}
	return g.loggerTo(w)
}

func (g *globals) loggerTo(w io.Writer) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(g.logLevel))
	opts := &slog.HandlerOptions{Level: lvl}
	if g.logFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func (g *globals) load() (*config.Config, error) {
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func runCmd(g *globals) *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the sync engine and the dashboard",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if service != "" {
				return runService(g, service) // started by the Windows service manager
			}
			log := g.logger()
			slog.SetDefault(log)
			cfg, err := g.load()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runPipeline(ctx, cfg, g.configPath, log)
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "run as this Windows service (set by docsync service install)")
	_ = cmd.Flags().MarkHidden("service")
	return cmd
}

// runPipeline runs the engine and its dashboard until ctx ends or the engine fails.
func runPipeline(ctx context.Context, cfg *config.Config, configPath string, log *slog.Logger) error {
	log.Info("starting docsync", "version", version, "config", configPath, "document", cfg.Document.Name)
	e, err := engine.Open(ctx, engine.Options{Config: cfg, Logger: log})
	if err != nil {
		return err
	}
	defer e.Close()

	var srv *http.Server
	if !cfg.Dashboard.Disabled {
		h, err := dashboard.Handler(e, cfg)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", cfg.Dashboard.Listen)
		if err != nil {
			return fmt.Errorf("dashboard: %w", err)
		}
		srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("dashboard stopped", "error", err)
			}
		}()
		log.Info("dashboard listening", "url", "http://"+displayAddr(cfg.Dashboard.Listen))
	}
	err = e.Run(ctx)
	if srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}
	log.Info("docsync stopped")
	return err
}

func displayAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	return listen
}

func initCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write an example config (docsync.yaml) and schema (schema.sql) to start from",
		RunE: func(cmd *cobra.Command, _ []string) error {
			for name, data := range map[string][]byte{"docsync.yaml": stones.Config, "schema.sql": stones.Schema} {
				p := filepath.Join(dir, name)
				if _, err := os.Stat(p); err == nil {
					return fmt.Errorf("%s already exists", p)
				}
				if err := os.WriteFile(p, data, 0o644); err != nil {
					return err
				}
				fmt.Println("wrote", p)
			}
			fmt.Println("next: edit docsync.yaml, set DOCSYNC_DSN, then run `docsync doctor`")
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "directory to write into")
	return cmd
}

func doctorCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the database and config for everything docsync needs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			env, err := tools.Connect(ctx, cfg)
			if err != nil {
				return err
			}
			defer env.Close()
			findings := env.Doctor(ctx)
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			failed := 0
			for _, f := range findings {
				fmt.Fprintf(w, "%s\t%s\t%s\n", f.Severity, f.Check, f.Detail)
				if f.Severity == tools.Fail {
					failed++
				}
			}
			w.Flush()
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
}

func introspectCmd(g *globals) *cobra.Command {
	var opt tools.IntrospectOptions
	var root, dsnEnv string
	cmd := &cobra.Command{
		Use:   "introspect",
		Short: "Draft a config from foreign keys, column names and (optionally) existing views",
		Example: `  DOCSYNC_DSN=... docsync introspect --root dbo.stone
  DOCSYNC_DSN=... docsync introspect --root dbo.stone --key stoneid --views 'sync.%'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn := os.Getenv(dsnEnv)
			if dsn == "" {
				return fmt.Errorf("set %s to the database connection string", dsnEnv)
			}
			t, err := config.ParseTableName(root)
			if err != nil {
				return err
			}
			opt.Root = t
			db, err := sqlserver.Open(cmd.Context(), dsn)
			if err != nil {
				return err
			}
			defer db.Close()
			out, err := tools.Introspect(cmd.Context(), db, opt)
			if err != nil {
				return err
			}
			fmt.Print(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "root table (one document per row), e.g. dbo.stone")
	cmd.Flags().StringVar(&opt.RootKey, "key", "", "root key column (defaults to the primary key)")
	cmd.Flags().StringVar(&opt.Views, "views", "", "only include tables read by views matching this LIKE pattern, e.g. 'sync.%'")
	cmd.Flags().IntVar(&opt.MaxDepth, "depth", 3, "maximum hops from the root")
	cmd.Flags().StringVar(&dsnEnv, "dsn-env", "DOCSYNC_DSN", "environment variable holding the connection string")
	_ = cmd.MarkFlagRequired("root")
	return cmd
}

func fanoutCmd(g *globals) *cobra.Command {
	var only string
	cmd := &cobra.Command{
		Use:   "fanout",
		Short: "Measure how many documents one source row reaches (scans tables; run off-peak)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			env, err := tools.Connect(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer env.Close()
			stats, err := env.Fanout(cmd.Context(), only)
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "TABLE\tKIND\tROWS\tMAX\tAVG\tP99\tWIDEST")
			for _, s := range stats {
				var top []string
				for _, k := range s.Top {
					top = append(top, fmt.Sprintf("%s→%d", k.Key, k.Roots))
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%.1f\t%.0f\t%s\n", s.Table, s.Kind, s.Rows, s.Max, s.Avg, s.P99, strings.Join(top, " "))
			}
			w.Flush()
			return err
		},
	}
	cmd.Flags().StringVar(&only, "table", "", "measure only this routing table")
	return cmd
}

func assembleCmd(g *globals) *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:   "assemble",
		Short: "Print the document for one root id, assembled from the database right now",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			env, err := tools.Connect(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer env.Close()
			d, err := env.AssembleOne(cmd.Context(), id)
			if err != nil {
				return err
			}
			return printJSON(doc.MustJSON(d))
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "root id")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func printJSON(raw string) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		fmt.Println(raw)
		return nil
	}
	fmt.Println(buf.String())
	return nil
}

func dumpCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "dump FILE.parquet",
		Short: "Print the rows of a segment or base file as JSON lines",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rdr, err := file.OpenParquetFile(args[0], false)
			if err != nil {
				return err
			}
			defer rdr.Close()
			fr, err := pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{BatchSize: 1024}, memory.DefaultAllocator)
			if err != nil {
				return err
			}
			tbl, err := fr.ReadTable(cmd.Context())
			if err != nil {
				return err
			}
			defer tbl.Release()
			fmt.Fprintf(os.Stderr, "%d rows; schema:\n%s\n", tbl.NumRows(), tbl.Schema())
			tr := array.NewTableReader(tbl, 1024)
			defer tr.Release()
			printed := 0
			for tr.Next() {
				rec := tr.RecordBatch()
				if limit > 0 && printed >= limit {
					break
				}
				if limit > 0 && printed+int(rec.NumRows()) > limit {
					rec = rec.NewSlice(0, int64(limit-printed))
					defer rec.Release()
				}
				if err := array.RecordToJSON(rec, os.Stdout); err != nil {
					return err
				}
				printed += int(rec.NumRows())
			}
			return tr.Err()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "rows to print (0 = all)")
	return cmd
}

// ---- admin client for a running instance -----------------------------------

type client struct {
	base       string
	user, pass string
}

func (g *globals) client() (*client, error) {
	cfg, err := g.load()
	if err != nil {
		return nil, err
	}
	c := &client{base: "http://" + displayAddr(cfg.Dashboard.Listen)}
	if cfg.Dashboard.BasicAuthUser != "" {
		c.user, c.pass = cfg.Dashboard.BasicAuthUser, os.Getenv(cfg.Dashboard.BasicAuthPasswordEnv)
	}
	return c, nil
}

func (c *client) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("is `docsync run` running? %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return out, nil
}

func statusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the status of a running instance",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			out, err := c.do(http.MethodGet, "/api/status", nil)
			if err != nil {
				return err
			}
			fmt.Println(string(out))
			return nil
		},
	}
}

func actionCmd(g *globals, name, short string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			if _, err := c.do(http.MethodPost, "/api/actions/"+name, struct{}{}); err != nil {
				return err
			}
			fmt.Println(name + ": done")
			return nil
		},
	}
}

func rebuildCmd(g *globals) *cobra.Command {
	var ids []string
	var all bool
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Queue documents for rebuild (only changed ones are emitted)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(ids) == 0 && !all {
				return errors.New("pass --id (repeatable) or --all")
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			if _, err := c.do(http.MethodPost, "/api/actions/rebuild", map[string]any{"ids": ids, "all": all}); err != nil {
				return err
			}
			fmt.Println("rebuild queued")
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&ids, "id", nil, "root id to rebuild (repeatable, or comma separated)")
	cmd.Flags().BoolVar(&all, "all", false, "rebuild every document")
	return cmd
}
