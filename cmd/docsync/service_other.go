//go:build !windows

package main

import (
	"errors"

	"github.com/Jayx282/docsync/internal/config"
)

var errNotWindows = errors.New("docsync service manages Windows services; on Linux or macOS run `docsync run --config docsync.yaml` under systemd or launchd (docs/windows-service.md lists the settings to copy)")

func serviceInstall(serviceOptions, *config.Config) error { return errNotWindows }
func serviceStart(string) error                           { return errNotWindows }
func serviceStop(string) error                            { return errNotWindows }
func serviceStatus(string) error                          { return errNotWindows }
func serviceUninstall(string) error                       { return errNotWindows }
func runService(*globals, string) error                   { return errNotWindows }
