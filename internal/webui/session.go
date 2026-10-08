// Package webui's session handlers used to own the live trading loop directly.
// P2-1 moved the state machine into internal/live/session so this package only
// adapts HTTP routing to it. The view types below are re-exported as aliases so
// existing callers (and the external webui_test package) that referenced
// webui.SessionStatus and friends keep compiling without change.
package webui

import (
	livesession "github.com/rdone44/trading-agent-go/internal/live/session"
)

// Session is the dashboard's live trading loop (state machine now in the
// session package).
type Session = livesession.Session

// StartOptions describes a session start request.
type StartOptions = livesession.StartOptions

// SessionStatus is the JSON the console polls.
type SessionStatus = livesession.SessionStatus

// CycleRecord is one decision cycle as the console displays it.
type CycleRecord = livesession.CycleRecord

// PositionView describes the open position, if any.
type PositionView = livesession.PositionView

// RiskView is the risk manager's live state as the console displays it.
type RiskView = livesession.RiskView

// ExecutionView is one fill as the console's order blotter displays it.
type ExecutionView = livesession.ExecutionView
