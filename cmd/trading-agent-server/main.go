// Command trading-agent-server is the server edition: a headless, long-running
// dashboard meant to sit behind systemd (or in a container) on a Linux host.
//
// Differences from the desktop build, all of them deliberate:
//   - it binds 0.0.0.0 (configurable) instead of loopback, so it can be reached
//     from another machine;
//   - it therefore requires an access token, read from TA_TOKEN or a file, and
//     refuses to start without one unless --allow-anonymous is passed;
//   - it never opens a browser and never shows a dialog;
//   - it logs to stdout so the journal captures it, and shuts down cleanly on
//     SIGTERM so systemd restarts do not cut off in-flight requests;
//   - it exposes /healthz for a probe.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/webui"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		addr            = flag.String("addr", envOr("TA_ADDR", "0.0.0.0:8080"), "listen address")
		configPath      = flag.String("config", envOr("TA_CONFIG", defaultConfigPath()), "YAML config file")
		outputDir       = flag.String("output", envOr("TA_OUTPUT", ""), "report directory (default: config value)")
		token           = flag.String("token", envOr("TA_TOKEN", ""), "access token (default: the token file, then a generated one)")
		tokenFile       = flag.String("token-file", envOr("TA_TOKEN_FILE", ""), "read the access token from this file")
		allowAnonymous  = flag.Bool("allow-anonymous", envOr("TA_ALLOW_ANONYMOUS", "") == "1", "serve without a token (only safe behind a trusted proxy)")
		printTokenOnly  = flag.Bool("print-token", false, "generate a token, print it, and exit")
		shutdownTimeout = flag.Duration("shutdown-timeout", 10*time.Second, "grace period for in-flight requests")
	)
	flag.Parse()
	_ = shutdownTimeout // the webui server owns its own fixed grace period

	if *printTokenOnly {
		generated, err := generateToken()
		if err != nil {
			log.Printf("generate token: %v", err)
			return 1
		}
		fmt.Println(generated)
		return 0
	}

	cfg, err := config.Load(existingOrEmpty(*configPath))
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}
	if *outputDir != "" {
		cfg.Backtest.OutputDir = *outputDir
	}

	accessToken, err := resolveToken(*token, *tokenFile, *allowAnonymous)
	if err != nil {
		log.Printf("token: %v", err)
		return 1
	}

	server := webui.New(cfg)
	server.Token = accessToken
	server.Log = os.Stdout

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("trading-agent server starting")
	log.Printf("  listen      %s", *addr)
	log.Printf("  reports     %s", cfg.Backtest.OutputDir)
	log.Printf("  config      %s", existingOrEmpty(*configPath))
	log.Printf("  auth        %s", authDescription(accessToken))

	if err := server.Serve(ctx, *addr, func(actual string) {
		log.Printf("  ready       http://%s", actual)
		if accessToken != "" {
			log.Printf("  open        http://%s/?token=%s", displayHost(actual), accessToken)
		}
	}); err != nil {
		log.Printf("serve: %v", err)
		return 1
	}
	log.Printf("stopped")
	return 0
}

// resolveToken picks the access token, in order of precedence: the flag/env
// value, then a token file, then a freshly generated one that is written to
// the token file so a restart keeps the same URL. Serving anonymously is only
// possible with an explicit opt-in.
func resolveToken(token, tokenFile string, allowAnonymous bool) (string, error) {
	if token != "" {
		return token, nil
	}
	if tokenFile != "" {
		raw, err := os.ReadFile(tokenFile)
		if err == nil {
			if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
				return trimmed, nil
			}
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("read %s: %w", tokenFile, err)
		}
		generated, err := generateToken()
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(tokenFile, []byte(generated+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", tokenFile, err)
		}
		return generated, nil
	}
	if allowAnonymous {
		return "", nil
	}
	return "", fmt.Errorf(
		"需要访问令牌：设置 TA_TOKEN，或用 -token-file 指定文件，" +
			"或用 -print-token 生成一个。确实要无认证暴露请显式加 -allow-anonymous")
}

// generateToken returns 32 hex characters (128 bits) from crypto/rand.
func generateToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func authDescription(token string) string {
	if token == "" {
		return "disabled (anonymous)"
	}
	shown := token
	if len(shown) > 4 {
		shown = shown[:4]
	}
	return fmt.Sprintf("token %s…", shown)
}

// displayHost turns a listen address into something clickable: 0.0.0.0 and
// the empty host both mean "any interface", which a browser cannot use.
func displayHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// defaultConfigPath follows the filesystem convention for a service: a
// system-wide file if present, otherwise one beside the binary.
func defaultConfigPath() string {
	const systemPath = "/etc/trading-agent/config.yaml"
	if _, err := os.Stat(systemPath); err == nil {
		return systemPath
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "config.yaml")
	}
	return "config.yaml"
}

// existingOrEmpty returns the path only when it exists, so config.Load treats
// a missing default as "use the built-in defaults" instead of an error.
func existingOrEmpty(path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
