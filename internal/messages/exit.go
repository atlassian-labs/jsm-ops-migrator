package messages

import "errors"

// Exit codes. They are distinct for each way a run can stop short, so scripts
// can tell them apart.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
	// ExitBlocked means the plan has a blocker: fix it and run plan again.
	ExitBlocked = 3
	// ExitAwaitingInput means a credential or a decision is missing: the
	// command did what it could without it, and runs fully once it is given.
	ExitAwaitingInput = 4
	// ExitValidationFailed means a file the command reads does not match
	// what it should: plan.json was changed, or an acceptance file is for
	// another plan version or does not accept every warning.
	ExitValidationFailed = 5
	// ExitPartial means apply stopped with items left that are not done:
	// some failed or were declined, an outcome is unknown, or it was paused.
	ExitPartial = 6
)

type exitError struct {
	err  error
	code int
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

func withCode(err error, code int) error {
	if err == nil {
		return nil
	}
	return exitError{err: err, code: code}
}

// ExitCodeOf returns the exit code one of this package's functions gave err.
// Only those count: an error from elsewhere can have an ExitCode method of
// its own, such as the *exec.ExitError of a keychain helper, and its code
// says nothing about this command.
func ExitCodeOf(err error) (int, bool) {
	var e exitError
	if errors.As(err, &e) {
		return e.code, true
	}
	return 0, false
}

// Blocked makes err exit with ExitBlocked. A nil err stays nil.
func Blocked(err error) error { return withCode(err, ExitBlocked) }

// AwaitingInput makes err exit with ExitAwaitingInput. A nil err stays nil.
func AwaitingInput(err error) error { return withCode(err, ExitAwaitingInput) }

// ValidationFailed makes err exit with ExitValidationFailed. A nil err stays
// nil.
func ValidationFailed(err error) error { return withCode(err, ExitValidationFailed) }

// PartiallyComplete makes err exit with ExitPartial. A nil err stays nil.
func PartiallyComplete(err error) error { return withCode(err, ExitPartial) }
