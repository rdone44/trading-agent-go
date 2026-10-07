package shell_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/shell"
)

// The desktop edition pins a port so its URL survives a restart; a browser tab
// left open from the previous run keeps working instead of failing to connect.
func TestDesktopAddrPrefersTheStablePort(t *testing.T) {
	// Ask for port 0 as the "preferred" value is not meaningful, so pick a free
	// one first and then request it explicitly.
	free, err := shell.FreePort("127.0.0.1")
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}

	addr, err := shell.DesktopAddr("127.0.0.1", free)
	if err != nil {
		t.Fatalf("DesktopAddr: %v", err)
	}
	if addr != "127.0.0.1:"+strconv.Itoa(free) {
		t.Fatalf("addr = %q, want the preferred port %d", addr, free)
	}
}

// When the preferred port is taken, the desktop must still start rather than
// fail: it falls back to whatever the OS hands out.
func TestDesktopAddrFallsBackWhenPreferredIsTaken(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	taken := listener.Addr().(*net.TCPAddr).Port

	addr, err := shell.DesktopAddr("127.0.0.1", taken)
	if err != nil {
		t.Fatalf("DesktopAddr: %v", err)
	}
	if addr == "127.0.0.1:"+strconv.Itoa(taken) {
		t.Fatalf("addr = %q, but that port is already bound", addr)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("addr = %q, want a loopback address", addr)
	}
}

// IsServing is what stops a second double-click from starting a rival server
// that would trade the same account behind the first one's back.
func TestIsServingDetectsARunningConsole(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	addr := strings.TrimPrefix(server.URL, "http://")
	if !shell.IsServing(addr) {
		t.Fatalf("IsServing(%q) = false, want true", addr)
	}
}

// A closed port must report not-serving so the desktop starts normally.
func TestIsServingIsFalseWhenNothingListens(t *testing.T) {
	port, err := shell.FreePort("127.0.0.1")
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}
	if shell.IsServing("127.0.0.1:" + strconv.Itoa(port)) {
		t.Fatal("IsServing must be false on a port nobody is listening on")
	}
}

// A server that answers but is not our console (no /healthz) must not be
// mistaken for a running instance.
func TestIsServingRejectsAnUnrelatedServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer server.Close()

	addr := strings.TrimPrefix(server.URL, "http://")
	if shell.IsServing(addr) {
		t.Fatal("a 404 on /healthz must not count as a running console")
	}
}

// The regression that let two servers run at once: the "already running?"
// probe has to happen on the *preferred* port. If it runs after the
// free-port fallback, it tests a freshly allocated port that is free by
// definition, concludes nothing is running, and starts a rival server that
// trades the same account.
func TestDesktopTargetDetectsRunningInstanceOnPreferredPort(t *testing.T) {
	// A stand-in for our own console, answering /healthz on the preferred port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	addr, alreadyRunning, err := shell.DesktopTarget("127.0.0.1", port)
	if err != nil {
		t.Fatalf("DesktopTarget: %v", err)
	}
	if !alreadyRunning {
		t.Fatal("a live console on the preferred port must be reported as already running")
	}
	if addr != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("addr = %q, want the preferred port %d", addr, port)
	}
}

// A port held by something that is not our console must not block startup: the
// desktop should fall back to a free port and serve normally.
func TestDesktopTargetFallsBackWhenPortIsHeldByAnotherProgram(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	taken := listener.Addr().(*net.TCPAddr).Port

	addr, alreadyRunning, err := shell.DesktopTarget("127.0.0.1", taken)
	if err != nil {
		t.Fatalf("DesktopTarget: %v", err)
	}
	if alreadyRunning {
		t.Fatal("a non-console listener must not be mistaken for a running instance")
	}
	if addr == "127.0.0.1:"+strconv.Itoa(taken) {
		t.Fatalf("addr = %q, but that port is already bound", addr)
	}
}
