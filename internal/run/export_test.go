package run

// CrashPoint and the hooks below let the crash tests stop a run as a crash
// would.
type CrashPoint = crashPoint

// CrashPoints are every point in an item's turn where a crash leaves a
// different state behind.
var CrashPoints = []CrashPoint{beforeSending, afterSending, afterCall, afterDone}

// ErrCrashed is what a run stopped by CrashAt returns.
var ErrCrashed = errCrashed

// ResendAfter and MaxAttempts bound when and how often resume sends a call
// again.
const (
	ResendAfter = resendAfter
	MaxAttempts = maxAttempts
)

// CrashAt makes the engine's runs stop when crash returns true.
func (e *Engine) CrashAt(crash func(p CrashPoint, key string) bool) { e.crash = crash }

func (p crashPoint) String() string {
	return [...]string{"before the sending row", "after the sending row", "after the call", "after the answer"}[p]
}
