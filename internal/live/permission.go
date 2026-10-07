package live

import (
	"fmt"
	"os"
)

// CheckExecutionAllowed enforces the process-level opt-in before constructing
// an order-placing runner. Paper sessions are unaffected. Only the exact value
// "1" enables execution; confirmation and exchange credentials remain required
// by the caller and broker respectively.
func CheckExecutionAllowed(execute bool) error {
	if execute && os.Getenv("TA_ALLOW_LIVE") != "1" {
		return fmt.Errorf("实盘启动被禁用：必须设置环境变量 TA_ALLOW_LIVE=1（默认关闭）")
	}
	return nil
}
