package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelWarn
	}
	return l
}

// newLogger builds the command's logger on w, which is stderr teed into the
// log file. Every value passes through the redactor. It never calls
// slog.SetDefault, so in-process tests do not share a logger.
func newLogger(w io.Writer, level slog.Level, json bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: credentials.RedactAttr}
	if json {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// logFile is artifacts/logs/<time>-<command>-<pid>.log. It is created on the
// first write, so a command that prints nothing to stderr leaves no empty
// file, and the name carries milliseconds and a counter, so two commands run
// from the menu in the same second do not collide.
type logFile struct {
	ws     *store.Workspace
	base   string
	stderr io.Writer

	mu     sync.Mutex
	w      io.WriteCloser
	failed bool
}

func newLogFile(ws *store.Workspace, command string, stderr io.Writer, now time.Time) *logFile {
	// No colons in the name, so it is valid on Windows.
	base := fmt.Sprintf("%s-%s-%d", now.UTC().Format("20060102T150405.000Z"), command, os.Getpid())
	return &logFile{ws: ws, base: base, stderr: stderr}
}

func (f *logFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.w == nil && !f.failed {
		w, err := f.create()
		if err != nil {
			// The text still reached stderr; say once that it is not saved.
			f.failed = true
			_, _ = fmt.Fprintf(f.stderr, "warning: cannot save the log file: %s\n", credentials.RedactText(err.Error()))
		} else {
			f.w = w
		}
	}
	if f.w != nil {
		if _, err := f.w.Write(p); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (f *logFile) create() (io.WriteCloser, error) {
	name := f.base + ".log"
	for i := 2; ; i++ {
		w, err := f.ws.CreateArtifact(filepath.Join("logs", name))
		if !errors.Is(err, fs.ErrExist) || i > 100 {
			return w, err
		}
		name = fmt.Sprintf("%s-%d.log", f.base, i)
	}
}

func (f *logFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.w == nil {
		return nil
	}
	return f.w.Close()
}
