package live

import (
	"fmt"
	"os"
)

// DesktopGate, when set, is the desktop edition's local live switch. It is
// consulted only after TA_ALLOW_LIVE is absent, and only the desktop build
// wires it (to the allow_live flag in its credentials file). The server
// edition leaves it nil, so on a network-reachable deployment the environment
// variable stays the one and only switch.
var DesktopGate func() bool

// CheckExecutionAllowed enforces the process-level opt-in before constructing
// an order-placing runner. Paper sessions are unaffected. Only the exact value
// "1" enables execution; confirmation and exchange credentials remain required
// by the caller and broker respectively.
func CheckExecutionAllowed(execute bool) error {
	if !execute {
		return nil
	}
	if os.Getenv("TA_ALLOW_LIVE") == "1" {
		return nil
	}
	// An explicitly set value other than "1" is a deliberate refusal: the
	// operator who exported TA_ALLOW_LIVE=0 must not be overridden by the
	// desktop switch. Only an absent (or empty) variable lets the desktop
	// decide for itself.
	if value, ok := os.LookupEnv("TA_ALLOW_LIVE"); ok && value != "" {
		return fmt.Errorf("实盘启动被禁用：必须设置环境变量 TA_ALLOW_LIVE=1（默认关闭）")
	}
	if DesktopGate != nil && DesktopGate() {
		return nil
	}
	return fmt.Errorf("实盘启动被禁用：必须设置环境变量 TA_ALLOW_LIVE=1（默认关闭）")
}
