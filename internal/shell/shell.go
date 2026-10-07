// Package shell holds the small OS-level helpers the desktop and server
// entry points need: opening a browser, picking a free port, and surfacing a
// fatal error to a user who has no console to read.
//
// It stays on the standard library so the project keeps its single
// non-stdlib dependency.
package shell

import (
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// DefaultDesktopPort is the loopback port the desktop edition prefers. A
// stable port matters more than a guaranteed-free one: it keeps the console's
// URL identical across restarts, so a restored browser tab or a bookmark keeps
// working instead of failing to connect to last run's port.
const DefaultDesktopPort = 8765

// OpenBrowser launches the default browser at url. Failure is not fatal and
// is returned so the caller can log it: a headless machine simply has nothing
// to open, and the URL is printed anyway.
func OpenBrowser(url string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "windows":
		// rundll32 hands the URL to the shell's registered handler, which is
		// what the Start menu uses. It avoids the quoting rules of `cmd /c start`.
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		command, args = "open", []string{url}
	default:
		command, args = "xdg-open", []string{url}
	}
	return exec.Command(command, args...).Start()
}

// FreePort asks the OS for an unused TCP port on host and releases it. There
// is an unavoidable race between the release and the caller's own bind, but
// for a local single-user desktop app it is the standard approach and the
// caller retries on failure.
func FreePort(host string) (int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	return addr.Port, nil
}

// DesktopAddr returns the address the desktop edition should bind. It prefers
// the stable default port so the URL survives a restart, and falls back to an
// OS-assigned port only when that one is already in use by something else.
func DesktopAddr(host string, preferred int) (string, error) {
	if preferred > 0 {
		addr := net.JoinHostPort(host, strconv.Itoa(preferred))
		if listener, err := net.Listen("tcp", addr); err == nil {
			listener.Close()
			return addr, nil
		}
	}
	port, err := FreePort(host)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// DesktopTarget decides what a desktop launch should do. It returns the
// address to serve on, or alreadyRunning=true when this app is already
// answering on the preferred port, in which case the caller should just
// surface the running console instead of starting a second server.
//
// The order matters: the "am I already up?" probe has to run against the
// preferred port, before the fallback. Probing after the fallback would test
// the fresh port - which is by definition free - and miss the running
// instance entirely, letting a second server start and trade the same account.
func DesktopTarget(host string, preferred int) (addr string, alreadyRunning bool, err error) {
	if preferred > 0 {
		preferredAddr := net.JoinHostPort(host, strconv.Itoa(preferred))
		if IsServing(preferredAddr) {
			return preferredAddr, true, nil
		}
	}
	addr, err = DesktopAddr(host, preferred)
	return addr, false, err
}

// IsServing reports whether this app already answers on addr. Double-clicking
// the exe a second time should surface the running console rather than start a
// rival server on another port, which would leave two books trading the same
// account.
func IsServing(addr string) bool {
	client := &http.Client{Timeout: 900 * time.Millisecond}
	response, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// ErrorDialog shows a modal error box on Windows. A desktop build compiled
// with -H=windowsgui has no console, so without this a startup failure would
// be completely invisible. On other platforms it is a no-op: the caller has
// already written the same text to its log.
func ErrorDialog(title, message string) {
	if runtime.GOOS != "windows" {
		return
	}
	// PowerShell is present on every supported Windows version, so this needs
	// no cgo, no GUI toolkit and no extra module.
	script := fmt.Sprintf(
		`Add-Type -AssemblyName PresentationFramework;`+
			`[System.Windows.MessageBox]::Show(%s,%s,'OK','Error') | Out-Null`,
		psQuote(message), psQuote(title))
	_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}

// psQuote wraps a string in single quotes for PowerShell, doubling any
// embedded quote as the language requires.
func psQuote(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '\'')
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'')
		}
		out = append(out, r)
	}
	return string(append(out, '\''))
}
