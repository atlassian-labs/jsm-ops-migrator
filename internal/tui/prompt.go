package tui

import (
	"context"
	"errors"
	"fmt"
	"io"

	"charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
)

// ErrCanceled is returned when the operator leaves a prompt with Esc or
// Ctrl+C.
var ErrCanceled = errors.New("canceled")

// Question is one field of a form.
type Question struct {
	Title       string
	Description string
	// Value is the starting answer, which the operator can edit. Never set
	// it for a masked question.
	Value       string
	Placeholder string
	// Masked hides what is typed, for secrets.
	Masked   bool
	Validate func(string) error
}

// Ask shows the questions together under a heading and returns the answers in
// order. In accessible mode it asks them one by one as plain prompts.
func Ask(ctx context.Context, in io.Reader, out io.Writer, color bool, heading string, qs []Question) ([]string, error) {
	answers := make([]string, len(qs))
	fields := make([]huh.Field, len(qs))
	for i, q := range qs {
		answers[i] = q.Value
		input := huh.NewInput().Title(q.Title).Placeholder(q.Placeholder).Value(&answers[i])
		if q.Description != "" {
			input = input.Description(q.Description)
		}
		if q.Masked {
			input = input.EchoMode(huh.EchoModePassword)
		}
		if q.Validate != nil {
			input = input.Validate(q.Validate)
		}
		fields[i] = input
	}
	group := huh.NewGroup(fields...)
	if !Accessible() {
		group = group.Title(heading)
	}
	if err := runForm(ctx, in, out, color, heading, group); err != nil {
		return nil, err
	}
	return answers, nil
}

// Option is one answer Choose offers.
type Option struct {
	Key   string // returned when the option is chosen
	Label string
}

// Choose asks for one of the options and returns its Key. The cursor starts
// on selected.
func Choose(ctx context.Context, in io.Reader, out io.Writer, color bool, title, description string, options []Option, selected string) (string, error) {
	choice := selected
	opts := make([]huh.Option[string], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o.Label, o.Key)
	}
	sel := huh.NewSelect[string]().Title(title).Options(opts...).Value(&choice)
	if description != "" {
		sel = sel.Description(description)
	}
	if err := runForm(ctx, in, out, color, "", huh.NewGroup(sel)); err != nil {
		return "", err
	}
	return choice, nil
}

// runForm runs one group as a form that Esc and Ctrl+C cancel. It draws on
// the terminal itself, not through a menu's capturing tee, so the result the
// menu shows afterwards holds what the command printed, not the form.
func runForm(ctx context.Context, in io.Reader, out io.Writer, color bool, heading string, group *huh.Group) error {
	out = screen(out)
	keys := huh.NewDefaultKeyMap()
	keys.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))
	form := huh.NewForm(group).WithInput(in).WithKeyMap(keys)
	if !color {
		form = form.WithTheme(huh.ThemeFunc(huh.ThemeBase))
	}
	if Accessible() {
		// Accessible mode reads out no group title, so it is printed first.
		if heading != "" {
			if _, err := fmt.Fprintln(out, heading); err != nil {
				return err
			}
		}
		form = form.WithOutput(out).WithAccessible(true)
	} else {
		form = form.WithOutput(filtered(out))
	}
	err := form.RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrCanceled
	}
	return err
}

// screen is the terminal behind a capturing tee, or out itself.
func screen(out io.Writer) io.Writer {
	if t, ok := out.(*teeTerminal); ok {
		return t.terminalFile
	}
	return out
}
