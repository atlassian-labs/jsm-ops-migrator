package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// The root files a run keeps while it runs.
const (
	// ActiveRunFile names the run an apply or resume is making, so pause can
	// find it without the data key.
	ActiveRunFile = "active-run.json"
	// PauseRequestFile asks the run named in it to stop before its next
	// item.
	PauseRequestFile = "pause-request.json"
)

// ActiveRun is what active-run.json holds.
type ActiveRun struct {
	RunID       string    `json:"run_id"`
	PlanVersion int       `json:"plan_version"`
	StartedAt   time.Time `json:"started_at"`
}

// PauseRequest is what pause-request.json holds.
type PauseRequest struct {
	RunID       string    `json:"run_id"`
	RequestedAt time.Time `json:"requested_at"`
}

// FindActive returns the run an apply or resume is making in the workspace,
// or nil when none is. It needs neither the data key nor the lock, and holds
// the lock only for as long as it takes to try it.
//
// A run is being made only while the lock is held and active-run.json
// exists: plan and login take the same lock, and a run that crashed leaves
// the file behind.
func FindActive(ws *store.Workspace) (*ActiveRun, error) {
	switch err := ws.Lock(); {
	case err == nil:
		return nil, ws.Unlock()
	case !errors.Is(err, store.ErrWorkspaceLocked):
		return nil, err
	}
	data, err := ws.ReadFile(ActiveRunFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a ActiveRun
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("reading %s: %w", ActiveRunFile, err)
	}
	if a.RunID == "" {
		return nil, fmt.Errorf("reading %s: it names no run", ActiveRunFile)
	}
	return &a, nil
}

// Announce tells pause that the command holding the lock is about to resume
// the run a names, so a pause asked from now on, while resume still runs its
// pre-flight checks, stops the run before its first item. That holds after a
// clean pause, which removed active-run.json, as after a crash, which left
// it. A pause request already there was asked before this command, so it is
// not for it: a crash left it unanswered, or it came after the run stopped.
// Announce removes it, so the run does not stop the moment it is resumed.
// The command calls Withdraw when it ends.
func Announce(ws *store.Workspace, a ActiveRun) error {
	if err := ws.RemoveFile(PauseRequestFile); err != nil {
		return err
	}
	return writeActive(ws, a)
}

// Withdraw removes active-run.json and the pause request, when the command
// that announced the run ends, however it ends: no run is being made any
// more, and a request asked before the run started is answered by the run
// not starting.
func Withdraw(ws *store.Workspace) error {
	if err := ws.RemoveFile(ActiveRunFile); err != nil {
		return err
	}
	return ws.RemoveFile(PauseRequestFile)
}

func writeActive(ws *store.Workspace, a ActiveRun) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return ws.WriteFileAtomic(ActiveRunFile, data)
}

// RequestPause asks run to stop before its next item. The run removes the
// request when it stops; apply, and resume when it announces the run, remove
// one left by a run that stopped before the request was written.
func RequestPause(ws *store.Workspace, run string, now time.Time) error {
	data, err := json.Marshal(PauseRequest{RunID: run, RequestedAt: now.UTC()})
	if err != nil {
		return err
	}
	return ws.WriteFileAtomic(PauseRequestFile, data)
}

// readPauseRequest returns the pause request, or nil when there is none.
func readPauseRequest(ws *store.Workspace) (*PauseRequest, error) {
	data, err := ws.ReadFile(PauseRequestFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r PauseRequest
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
