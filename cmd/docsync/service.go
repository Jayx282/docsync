package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Jayx282/docsync/internal/config"
)

// serviceOptions are the settings of docsync service install.
type serviceOptions struct {
	name    string // service name
	config  string // absolute path of docsync.yaml
	dsn     string // connection string, if the service should carry one
	account string // account the service runs as
}

func serviceCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run a pipeline as a Windows service: install, start, stop, status, uninstall",
		Long: `Runs a pipeline as a Windows service, so it starts with the machine and restarts after failures.
Run these commands in an administrator terminal. See docs/windows-service.md.`,
	}
	var name, dsn, account string
	resolve := func() (string, *config.Config, error) {
		if name != "" {
			return name, nil, nil
		}
		cfg, err := config.Load(g.configPath)
		if err != nil {
			return "", nil, fmt.Errorf("give --name, or --config with the pipeline's docsync.yaml: %w", err)
		}
		return defaultServiceName(cfg), cfg, nil
	}

	install := &cobra.Command{
		Use:     "install",
		Short:   "Register a pipeline (its docsync.yaml) as a Windows service",
		Example: `  docsync service install --config C:\docsync\stone\docsync.yaml --dsn "sqlserver://localhost?database=Stones&encrypt=disable"`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := filepath.Abs(g.configPath)
			if err != nil {
				return err
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			n := name
			if n == "" {
				n = defaultServiceName(cfg)
			}
			return serviceInstall(serviceOptions{name: n, config: path, dsn: dsn, account: account}, cfg)
		},
	}
	install.Flags().StringVar(&name, "name", "", "service name (default docsync-<document name>)")
	install.Flags().StringVar(&dsn, "dsn", "", "connection string for the service; with Windows authentication it holds no password")
	install.Flags().StringVar(&account, "account", "", `account to run as (default: the service's own virtual account, NT SERVICE\<name>); for another user, set DOCSYNC_SERVICE_PASSWORD`)

	simple := func(use, short string, fn func(string) error) *cobra.Command {
		c := &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(cmd *cobra.Command, _ []string) error {
				n, _, err := resolve()
				if err != nil {
					return err
				}
				return fn(n)
			},
		}
		c.Flags().StringVar(&name, "name", "", "service name (or give --config)")
		return c
	}
	cmd.AddCommand(install,
		simple("start", "Start the service", serviceStart),
		simple("stop", "Stop the service", serviceStop),
		simple("status", "Show whether the service is running, and where it logs", serviceStatus),
		simple("uninstall", "Remove the service (its folder, state and output are kept)", serviceUninstall),
	)
	return cmd
}
