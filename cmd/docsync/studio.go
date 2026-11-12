package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Jayx282/docsync/internal/source/sqlserver"
	"github.com/Jayx282/docsync/internal/studio"
)

func studioCmd(g *globals) *cobra.Command {
	var dir, listen, dsnEnv, token string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "studio",
		Short: "Open the visual designer: browse tables, design documents, preview, validate and deploy",
		Example: `  export DOCSYNC_DSN='sqlserver://user:pass@host:1433?database=mydb'
  docsync studio --dir ./studio`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := g.logger()
			slog.SetDefault(log)
			dsn := os.Getenv(dsnEnv)
			if dsn == "" {
				return fmt.Errorf("set %s to the database connection string (it is never written to disk)", dsnEnv)
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			db, err := sqlserver.Open(ctx, dsn)
			if err != nil {
				return err
			}
			defer db.Close()
			store, err := studio.NewStore(filepath.Join(abs, "designs"))
			if err != nil {
				return err
			}
			svc := &studio.Service{DB: db, DSNEnv: dsnEnv, BaseDir: abs}
			rt := &studio.Runtime{Log: log}
			if err := svc.StartActive(ctx, rt); err != nil {
				log.Warn("could not restart the deployed pipeline", "error", err)
			}
			defer rt.Stop()

			tokenFile := ""
			if token == "" {
				t, _, warn, err := studio.LoadOrCreateToken(abs)
				if err != nil {
					return fmt.Errorf("studio token: %w", err)
				}
				token, tokenFile = t, filepath.Join(abs, studio.TokenFile)
				if warn != "" {
					log.Warn(warn)
				}
			}
			host, _, err := net.SplitHostPort(listen)
			if err != nil {
				return fmt.Errorf("--listen: %w", err)
			}
			loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
			if !loopback {
				log.Warn("studio is listening on a non-loopback address; anyone with the token and network access can use it", "listen", listen)
			}
			srv := &studio.Server{Svc: svc, Store: store, Runtime: rt, Token: token, Log: log, Loopback: loopback, TokenFile: tokenFile}
			h, err := srv.Handler()
			if err != nil {
				return err
			}
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			httpSrv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
			go func() {
				if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("studio server stopped", "error", err)
				}
			}()
			base := fmt.Sprintf("http://%s/", displayAddr(ln.Addr().String()))
			url := base + "?token=" + token
			fmt.Fprintf(os.Stderr, "\n  docsync studio is running.\n  Sign-in link: %s\n", url)
			if tokenFile != "" {
				fmt.Fprintf(os.Stderr, "  The token is saved in %s, so this link keeps working after restarts.\n"+
					"  In another browser, open %s and paste the token. Delete the file to issue a new one.\n", tokenFile, base)
			}
			fmt.Fprintf(os.Stderr, "  Designs are saved in %s\n\n", filepath.Join(abs, "designs"))
			if !noBrowser {
				openBrowser(url)
			}
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutCtx)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "./studio", "studio directory (designs, deployed pipelines, output)")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8090", "address to listen on")
	cmd.Flags().StringVar(&dsnEnv, "dsn-env", "DOCSYNC_DSN", "environment variable holding the connection string")
	cmd.Flags().StringVar(&token, "token", os.Getenv("DOCSYNC_STUDIO_TOKEN"), "access token (default: the one saved in <dir>/.token, created on first start; env DOCSYNC_STUDIO_TOKEN)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not open a browser")
	return cmd
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}
