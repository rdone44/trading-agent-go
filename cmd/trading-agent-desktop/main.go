// Command trading-agent-desktop is the desktop edition: it starts the same
// dashboard the server build serves, but bound to loopback on a free port and
// opened in the default browser, with no console window and no flags to pass.
//
// It is built with -H=windowsgui on Windows so double-clicking the .exe never
// flashes a terminal. Everything it would have printed goes to a log file
// under the user's config directory instead.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/shell"
	"github.com/rdone44/trading-agent-go/internal/webui"
)

const appName = "trading-agent"

func main() {
	if err := run(); err != nil {
		// A GUI process has no console, so the message also goes to a dialog
		// box; otherwise a double-click failure would be silent.
		logPath := logFilePath()
		shell.ErrorDialog(appName, fmt.Sprintf("%v\n\n日志：%s", err, logPath))
		os.Exit(1)
	}
}

func run() error {
	logFile, err := openLog()
	if err != nil {
		return err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n=== %s desktop %s ===\n", appName, time.Now().Format(time.RFC3339))

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Reports land next to the executable so the folder is self-contained and
	// easy to find; fall back to the working directory if it is not writable
	// (e.g. the exe sits in Program Files).
	cfg.Backtest.OutputDir = reportDir(logFile)
	// Pin the session state next to the executable for the same reason as the
	// reports directory: a double-clicked exe should keep its book in one
	// predictable place, not wherever the shell happened to set the CWD.
	cfg.Live.StateFile = filepath.Join(filepath.Dir(cfg.Backtest.OutputDir), "trade-state.json")

	addr, alreadyRunning, err := shell.DesktopTarget("127.0.0.1", shell.DefaultDesktopPort)
	if err != nil {
		return fmt.Errorf("找不到可用端口: %w", err)
	}
	url := "http://" + addr

	// Double-clicking the exe while it is already running should surface the
	// live console, not start a second server on another port that would trade
	// the same account behind the first one's back.
	if alreadyRunning {
		fmt.Fprintf(logFile, "already serving on %s, opening it\n", addr)
		if err := shell.OpenBrowser(url); err != nil {
			fmt.Fprintf(logFile, "open browser: %v\n", err)
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := webui.New(cfg)
	server.Desktop = true
	server.Log = logFile
	// The 退出 button in the UI cancels this context. Only the desktop build
	// wires it, so the endpoint does not exist on the server edition.
	server.Cancel = stop

	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		// Serve binds the port itself and reports the address it actually got,
		// which is what we open in the browser.
		done <- server.Serve(ctx, addr, func(actual string) {
			ready <- actual
		})
	}()

	select {
	case actual := <-ready:
		url = "http://" + actual
		fmt.Fprintf(logFile, "listening on %s\n", actual)
	case err := <-done:
		if err != nil {
			return err
		}
		return nil
	}

	if err := shell.OpenBrowser(url); err != nil {
		// Not fatal: the user can still open the URL by hand.
		fmt.Fprintf(logFile, "open browser: %v\n", err)
	}

	if err := <-done; err != nil {
		return err
	}
	fmt.Fprintf(logFile, "stopped %s\n", time.Now().Format(time.RFC3339))
	return nil
}

// loadConfig reads config.yaml next to the executable first, then the working
// directory, and finally falls back to the built-in defaults. The exe-relative
// lookup is what makes a copied folder behave the same on any machine.
func loadConfig() (config.Config, error) {
	for _, path := range configCandidates() {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		return config.Load(path)
	}
	return config.Default(), nil
}

func configCandidates() []string {
	paths := make([]string, 0, 2)
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "config.yaml"))
	}
	paths = append(paths, "config.yaml")
	return paths
}

// reportDir returns a writable reports directory, preferring the one next to
// the executable and falling back to the user's config directory.
func reportDir(log io.Writer) string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "reports")
		if err := os.MkdirAll(candidate, 0o755); err == nil {
			if probeWritable(candidate) {
				return candidate
			}
		}
	}
	fallback := filepath.Join(configDir(), "reports")
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		fmt.Fprintf(log, "report dir fallback: %v\n", err)
		return "reports"
	}
	return fallback
}

// probeWritable confirms a directory really accepts a file, which a MkdirAll
// on an existing but read-only directory would not catch.
func probeWritable(dir string) bool {
	probe := filepath.Join(dir, ".write-test")
	file, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false
	}
	file.Close()
	_ = os.Remove(probe)
	return true
}

// openLog appends to a log file in the user's config directory. The desktop
// build has no console, so this file is the only record of what happened.
func openLog() (*os.File, error) {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("无法创建配置目录 %s: %w", dir, err)
	}
	return os.OpenFile(filepath.Join(dir, "desktop.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func logFilePath() string {
	return filepath.Join(configDir(), "desktop.log")
}

// configDir is %APPDATA%\trading-agent on Windows and ~/.config/trading-agent
// elsewhere, matching the platform convention for per-user application data.
func configDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, appName)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "."+appName)
	}
	return "."
}
