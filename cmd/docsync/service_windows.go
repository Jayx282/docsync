//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/logfile"
)

// ---- install, start, stop, status, uninstall ----------------------------------------

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Windows service manager (run this in an administrator terminal): %w", err)
	}
	return m, nil
}

func openService(name string) (*mgr.Mgr, *mgr.Service, error) {
	m, err := connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(name)
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("there is no service named %s (see docsync service install): %w", name, err)
	}
	return m, s, nil
}

// builtinAccount reports accounts that need no password: virtual service accounts and
// the built-in service accounts.
func builtinAccount(a string) bool {
	l := strings.ToLower(a)
	return strings.HasPrefix(l, `nt service\`) || strings.HasPrefix(l, `nt authority\`) ||
		l == "localsystem" || l == "localservice" || l == "networkservice"
}

func scmAccount(a string) string {
	switch strings.ToLower(a) {
	case "localservice":
		return `NT AUTHORITY\LocalService`
	case "networkservice":
		return `NT AUTHORITY\NetworkService`
	}
	return a // LocalSystem, NT SERVICE\x, DOMAIN\user, .\user
}

func icacls(args ...string) error {
	out, err := exec.Command("icacls", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func serviceInstall(opt serviceOptions, cfg *config.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(opt.name); err == nil {
		s.Close()
		return fmt.Errorf("a service named %s already exists; remove it first with: docsync service uninstall --name %s", opt.name, opt.name)
	}

	account := opt.account
	if account == "" {
		account = `NT SERVICE\` + opt.name
	}
	password := ""
	if !builtinAccount(account) {
		password = os.Getenv("DOCSYNC_SERVICE_PASSWORD")
		if password == "" {
			return fmt.Errorf("set DOCSYNC_SERVICE_PASSWORD to the password of %s", account)
		}
	}
	c := mgr.Config{
		DisplayName:      "docsync (" + cfg.Document.Name + ")",
		Description:      "Keeps the " + cfg.Document.Name + " documents in sync with SQL Server. Config: " + opt.config,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true, // after SQL Server and the network are up
		ServiceStartName: scmAccount(account),
		Password:         password,
	}
	if strings.HasPrefix(strings.ToLower(account), `nt service\`) {
		c.SidType = windows.SERVICE_SID_TYPE_UNRESTRICTED // gives the virtual account its identity
	}
	s, err := m.CreateService(opt.name, exe, c, "run", "--config", opt.config, "--service", opt.name)
	if err != nil {
		return fmt.Errorf("create the service: %w", err)
	}
	defer s.Close()
	done := false
	defer func() {
		if !done {
			_ = s.Delete() // do not leave a half-installed service behind
		}
	}()

	// Restart after failures: 10 s, 30 s, then every minute; the count resets daily.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}, 24*60*60); err != nil {
		return fmt.Errorf("set restart on failure: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set restart on failure: %w", err)
	}

	dir := filepath.Dir(opt.config)
	local := !strings.EqualFold(account, "LocalSystem")
	if local { // the account writes state/, out/ and logs/ next to the config
		if err := icacls(dir, "/grant", scmAccount(account)+":(OI)(CI)M", "/T", "/Q"); err != nil {
			return fmt.Errorf("give %s access to %s: %w", account, dir, err)
		}
	}

	var env []string
	secret := ""
	if opt.dsn != "" {
		if dsnHasPassword(opt.dsn) {
			secret = filepath.Join(dir, "dsn.secret")
			if err := os.WriteFile(secret, []byte(opt.dsn+"\n"), 0o600); err != nil {
				return err
			}
			grants := []string{secret, "/inheritance:r", "/grant:r", "*S-1-5-18:F", "*S-1-5-32-544:F"} // SYSTEM, Administrators
			if local {
				grants = append(grants, scmAccount(account)+":R")
			}
			if err := icacls(append(grants, "/Q")...); err != nil {
				return fmt.Errorf("protect %s: %w", secret, err)
			}
			env = append(env, dsnFileEnv+"="+secret)
		} else {
			env = append(env, dsnEnvName(cfg)+"="+opt.dsn)
		}
	}
	if len(env) > 0 {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+opt.name, registry.SET_VALUE)
		if err != nil {
			return err
		}
		err = k.SetStringsValue("Environment", env)
		k.Close()
		if err != nil {
			return fmt.Errorf("store the connection setting: %w", err)
		}
	}
	if err := eventlog.InstallAsEventCreate(opt.name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil && !strings.Contains(err.Error(), "exists") {
		return fmt.Errorf("register the event log source: %w", err)
	}
	done = true

	fmt.Printf("\nInstalled the Windows service %s.\n", opt.name)
	fmt.Printf("  Runs:      %s run --config %s\n", exe, opt.config)
	fmt.Printf("  Account:   %s\n", account)
	fmt.Printf("  Starts:    automatically with Windows, and again after failures\n")
	fmt.Printf("  Logs:      %s\n", filepath.Join(dir, "logs", "docsync.log"))
	fmt.Printf("             and Event Viewer > Windows Logs > Application, source %s\n", opt.name)
	if !cfg.Dashboard.Disabled {
		fmt.Printf("  Dashboard: http://%s on this machine\n", displayAddr(cfg.Dashboard.Listen))
	}
	switch {
	case secret != "":
		fmt.Printf("\nThe connection string is in %s, readable only by the service account and administrators.\n", secret)
	case opt.dsn == "" && cfg.Source.DSN == "":
		fmt.Printf("\nNo --dsn given: the service reads %s from the system environment variables. Set it there, or reinstall with --dsn.\n", dsnEnvName(cfg))
	}
	if opt.dsn != "" && !dsnHasPassword(opt.dsn) && local {
		db := dsnDatabase(opt.dsn)
		fmt.Printf("\nThe service signs in to SQL Server as %s (Windows authentication). If SQL Server runs on this machine, allow it with:\n", account)
		fmt.Printf("  CREATE LOGIN [%s] FROM WINDOWS;\n  USE [%s];\n  CREATE USER [%s] FOR LOGIN [%s];\n", account, db, account, account)
		fmt.Printf("  -- then the GRANT statements from Studio's Checks, with [%s] in place of [docsync_login]\n", account)
		fmt.Printf("(For SQL Server on another machine, run the service as a domain account with --account.)\n")
	}
	fmt.Printf("\nStart it with: docsync service start --name %s\n", opt.name)
	return nil
}

// dsnDatabase returns the database a connection string names, or a placeholder.
func dsnDatabase(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		for k, v := range u.Query() {
			if strings.EqualFold(k, "database") && len(v) > 0 {
				return v[0]
			}
		}
		return "YourDatabase"
	}
	for _, part := range strings.Split(dsn, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			if k = strings.ToLower(strings.TrimSpace(k)); k == "database" || k == "initial catalog" {
				return strings.TrimSpace(v)
			}
		}
	}
	return "YourDatabase"
}

func waitFor(s *mgr.Service, want svc.State, timeout time.Duration) (svc.Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil || st.State == want || time.Now().After(deadline) {
			return st, err
		}
		time.Sleep(300 * time.Millisecond)
	}
}

var stateNames = map[svc.State]string{
	svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping", svc.Running: "running",
	svc.ContinuePending: "resuming", svc.PausePending: "pausing", svc.Paused: "paused",
}

func serviceStart(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err := s.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	st, err := waitFor(s, svc.Running, 30*time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("%s is %s.\n", name, stateNames[st.State])
	return serviceStatus(name)
}

func serviceStop(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State == svc.Stopped {
		fmt.Printf("%s is already stopped.\n", name)
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop %s: %w", name, err)
	}
	st, err := waitFor(s, svc.Stopped, 45*time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("%s is %s.\n", name, stateNames[st.State])
	return nil
}

func serviceStatus(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	c, err := s.Config()
	if err != nil {
		return err
	}
	fmt.Printf("Service:   %s (%s)\n", name, stateNames[st.State])
	fmt.Printf("Account:   %s\n", c.ServiceStartName)
	if args, err := windows.DecomposeCommandLine(c.BinaryPathName); err == nil {
		for i, a := range args {
			if a == "--config" && i+1 < len(args) {
				fmt.Printf("Config:    %s\n", args[i+1])
				fmt.Printf("Logs:      %s\n", filepath.Join(filepath.Dir(args[i+1]), "logs", "docsync.log"))
				if cfg, err := config.Load(args[i+1]); err == nil && !cfg.Dashboard.Disabled {
					fmt.Printf("Dashboard: http://%s\n", displayAddr(cfg.Dashboard.Listen))
				}
			}
		}
	}
	return nil
}

func serviceUninstall(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err == nil {
			_, _ = waitFor(s, svc.Stopped, 45*time.Second)
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	_ = eventlog.Remove(name)
	fmt.Printf("Removed the service %s. Its folder (config, state, output and logs) is kept.\n", name)
	return nil
}

// ---- running under the service manager -----------------------------------------------

// pipelineService connects the pipeline to the Windows service manager.
type pipelineService struct {
	run func(ctx context.Context)
}

func (p *pipelineService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.run(ctx)
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 30000}
				cancel()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
				}
				return false, 0
			}
		case <-done:
			return false, 0
		}
	}
}

func runService(g *globals, name string) error {
	inService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !inService {
		return errors.New("--service is set by docsync service install for the Windows service manager; to run docsync in this window, leave it out")
	}
	elog, _ := eventlog.Open(name) // without an event source, the log file still works
	event := func(kind, msg string) {
		if elog == nil {
			return
		}
		switch kind {
		case "info":
			_ = elog.Info(1, msg)
		case "warn":
			_ = elog.Warning(2, msg)
		default:
			_ = elog.Error(3, msg)
		}
	}
	logPath := g.logFile
	if logPath == "" {
		logPath = filepath.Join(filepath.Dir(g.configPath), "logs", "docsync.log")
	}
	lf, err := logfile.Open(logPath, 10<<20, 5)
	if err != nil {
		event("error", "docsync cannot open its log file "+logPath+": "+err.Error())
		return err
	}
	defer lf.Close()
	log := g.loggerTo(lf)
	slog.SetDefault(log)

	run := func(ctx context.Context) {
		event("info", "docsync started with "+g.configPath+". Logs: "+logPath)
		keepRunning(ctx, func(ctx context.Context) error {
			cfg, err := loadForService(g.configPath)
			if err != nil {
				return err
			}
			return runPipeline(ctx, cfg, g.configPath, log)
		}, log, func(err error, wait time.Duration) {
			event("warn", fmt.Sprintf("docsync stopped and starts again in %s: %v", wait, err))
		}, nil)
		event("info", "docsync stopped")
	}
	if err := svc.Run(name, &pipelineService{run: run}); err != nil {
		event("error", "docsync could not run as a service: "+err.Error())
		return err
	}
	return nil
}
