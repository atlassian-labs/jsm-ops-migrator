package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	engine "github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

// stopNotice is shown at the first Ctrl+C of a run. A call can take minutes:
// turning on Operations waits for JSM's site lock, then for the request to
// finish.
const stopNotice = "Stopping after the current call, which can take a few minutes if it waits on JSM. " +
	"Press Ctrl+C again to stop at once: the call is then looked up when you resume."

// runView shows a run as it goes. In a terminal it draws a line per step
// with a bar, and prints the messages of each item that did not end done
// above it as they happen. Elsewhere it logs them: each message as a
// warning, and each step at info once its last item is taken.
type runView struct {
	env     *Env
	bar     *tui.Progress // nil when not drawing in a terminal
	done    chan struct{}
	watched sync.WaitGroup
	once    sync.Once
}

// startRunView starts showing the run e makes, and tells the engine to stop
// before its next item at the first Ctrl+C.
func startRunView(env *Env, e *engine.Engine) *runView {
	v := &runView{env: env, done: make(chan struct{})}
	if env.Interactive && env.Output == OutputText {
		v.bar = tui.NewProgress(env.Stdout)
	}
	e.Interrupted, e.Progress = env.Interrupted, v.progress
	v.watched.Go(v.watch)
	return v
}

// watch shows the stop notice at the first Ctrl+C. A Ctrl+C that came just
// before the run ended still gets it, before the run's result.
func (v *runView) watch() {
	select {
	case <-v.env.Interrupted:
	case <-v.done:
		if !interrupted(v.env) {
			return
		}
	}
	if v.bar != nil {
		v.bar.Println(stopNotice)
		return
	}
	_, _ = fmt.Fprintln(v.env.Stderr, stopNotice)
}

func (v *runView) progress(p engine.Progress) {
	if p.Entry.State != review.Done && len(p.Entry.Messages) > 0 {
		if v.bar != nil {
			var b strings.Builder
			_ = messages.WriteText(&b, p.Entry.Messages)
			for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
				v.bar.Println(line)
			}
		} else {
			for _, m := range p.Entry.Messages {
				v.env.Logger.Warn(messages.Clean(m.Text), slog.String("message", messages.Clean(m.ID())),
					slog.String("class", m.Class.Label()))
			}
		}
	}
	if v.bar != nil {
		v.bar.Update(p.Step, p.Taken, p.Total)
	}
	if p.Taken == p.Total {
		v.env.Logger.Info("step taken", slog.String("step", p.Step), slog.Int("items", p.Total))
	}
}

// close stops the view, so what the command prints next starts on a line of
// its own.
func (v *runView) close() {
	v.once.Do(func() {
		close(v.done)
		v.watched.Wait()
		if v.bar != nil {
			v.bar.Close()
		}
	})
}

// interrupted reports whether the first Ctrl+C has come.
func interrupted(env *Env) bool {
	select {
	case <-env.Interrupted:
		return true
	default:
		return false
	}
}

// finishRun reports a run that apply or resume made: in text, its counts
// and what is not done; in JSON, as part of the command's one document.
func finishRun(env *Env, command string, out applyOutput, res engine.RunResult, err error) error {
	if err != nil && res.Info.ID == "" {
		return out.emit(env, fmt.Errorf("%s: %w", command, err))
	}
	out.Run = runResult(env.Workspace, res)
	if env.Output == OutputText {
		if werr := writeRun(env.Stdout, out.Run); werr != nil {
			return errors.Join(err, werr)
		}
	}
	env.summarize(runSummary(out.Run))
	if err != nil {
		return out.emit(env, fmt.Errorf("%s: %w", command, err))
	}
	return out.emit(env, runError(command, out.Run))
}
