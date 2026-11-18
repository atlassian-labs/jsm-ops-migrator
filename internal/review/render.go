package review

import (
	"fmt"
	"strings"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// The review files are Markdown, so they read well as plain text and render
// in any viewer the operator shares them with. Every text a step, an API or
// the operator wrote is cleaned (messages.Clean) and has the characters that
// make links, images, HTML and emphasis escaped, so a PagerDuty name cannot
// turn into a link that looks like part of the review. Keys and references
// are code spans; their checks keep backticks out of them.

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

// & would start an HTML entity such as &#x202e;, and ~~ strikes text out. _
// is left alone: GitHub's Markdown does not emphasize inside a word, and
// escaping it would garble the email addresses the files are full of. A bare
// URL still renders as a link in some viewers, but shows where it goes.
var markdownEscaper = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "[", `\[`, "]", `\]`, "<", `\<`, ">", `\>`, "&", `\&`, "~", `\~`)

func text(s string) string { return markdownEscaper.Replace(messages.Clean(s)) }

func code(s string) string { return "`" + messages.OneLine(s) + "`" }

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// RenderPlan returns plan.md: the reads the plan was built from, the counts
// per step, and every item in the order apply takes them.
func RenderPlan(p Plan) []byte {
	var b strings.Builder
	s := Summarize(p)
	fmt.Fprintf(&b, "# Plan v%d\n\n", p.Version)
	fmt.Fprintf(&b, "Created %s.\n\n", stamp(p.CreatedAt))
	if len(p.Reads) > 0 {
		b.WriteString("Built from these reads:\n\n")
		for _, r := range p.Reads {
			state := "complete"
			if !r.Complete {
				state = "incomplete"
			}
			fmt.Fprintf(&b, "- %s, read %s: %s\n", text(r.Side.Label()), stamp(r.At), state)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "`apply` makes %s. The plan raises %s: see `risk-queue.md`.\n\n",
		count(s.Calls, "call", "calls"), s.Messages)

	b.WriteString("## By step\n\n")
	if len(s.Steps) == 0 {
		b.WriteString("The plan has no items.\n\n")
	} else {
		b.WriteString("| Step |")
		for _, a := range Actions {
			fmt.Fprintf(&b, " %s |", a)
		}
		b.WriteString("\n|---|" + strings.Repeat("---:|", len(Actions)) + "\n")
		for _, sc := range s.Steps {
			fmt.Fprintf(&b, "| %s |", text(sc.Step))
			for _, a := range Actions {
				fmt.Fprintf(&b, " %d |", sc.Of(a))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Items in the order apply takes them\n\n")
	if len(p.Items) == 0 {
		b.WriteString("None.\n")
		return []byte(b.String())
	}
	b.WriteString("`create` and `update` make a call. `map` records the link to an object that is already in JSM, " +
		"and `skip` and `manual` are reported: none of these three makes a call.\n\n")
	for i, it := range p.Items {
		fmt.Fprintf(&b, "%d. %s %s", i+1, code(it.Key), text(string(it.Action)))
		if it.JSM != nil {
			fmt.Fprintf(&b, " (JSM %s)", code(it.JSM.String()))
		}
		fmt.Fprintf(&b, ": %s\n", text(it.Summary))
		if len(it.DependsOn) > 0 {
			deps := make([]string, len(it.DependsOn))
			for j, d := range it.DependsOn {
				deps[j] = code(d)
			}
			fmt.Fprintf(&b, "   After %s.\n", strings.Join(deps, ", "))
		}
	}
	return []byte(b.String())
}

var classIntro = map[messages.Class]string{
	messages.Blocker:     "These must be fixed before `apply` can run. Fix each one, then run `plan` again.",
	messages.Warning:     "`apply` runs only after each of these is accepted, with a reason: in its prompt, or in an acceptance file (`acceptance-template.yaml` lists every ID).",
	messages.ManualTask:  "Do these by hand.",
	messages.Information: "Nothing to do.",
}

var classHeading = map[messages.Class]string{
	messages.Blocker:     "Blockers",
	messages.Warning:     "Warnings",
	messages.ManualTask:  "Manual tasks",
	messages.Information: "Information",
}

// RenderRiskQueue returns risk-queue.md: the plan's messages by class, the
// most urgent first, each with its ID.
func RenderRiskQueue(p Plan) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Risk queue for plan v%d\n\n", p.Version)
	fmt.Fprintf(&b, "%s.\n", capitalize(messages.Count(p.Messages).String()))
	for _, class := range messages.Classes {
		fmt.Fprintf(&b, "\n## %s\n\n", classHeading[class])
		var listed bool
		for _, m := range p.Messages {
			if m.Class != class {
				continue
			}
			if !listed {
				b.WriteString(classIntro[class] + "\n\n")
				listed = true
			}
			fmt.Fprintf(&b, "- %s: %s\n", code(m.ID()), text(m.Text))
		}
		if !listed {
			b.WriteString("None.\n")
		}
	}
	return []byte(b.String())
}

// RenderDiff returns diff.md.
func RenderDiff(d Diff) []byte {
	var b strings.Builder
	if d.From == 0 {
		fmt.Fprintf(&b, "# Plan v%d\n\nThis is the first plan, so there is nothing to compare it with.\n", d.To)
		return []byte(b.String())
	}
	fmt.Fprintf(&b, "# Changes from plan v%d to v%d\n\n", d.From, d.To)
	if d.Empty() {
		b.WriteString("Nothing changed.\n")
		return []byte(b.String())
	}
	fmt.Fprintf(&b, "%s added, %d removed, %d changed. %s, %d no longer raised, %d changed.\n",
		count(len(d.Added), "item", "items"), len(d.Removed), len(d.Changed),
		count(len(d.NewMessages), "new message", "new messages"), len(d.GoneMessages), len(d.ChangedMessages))
	if d.ItemsReordered || d.MessagesReordered {
		b.WriteString("\n## Order\n\n")
		if d.ItemsReordered {
			b.WriteString("- The items are in another order: `plan.md` lists them in the order `apply` takes them.\n")
		}
		if d.MessagesReordered {
			b.WriteString("- The messages are in another order.\n")
		}
	}
	items := func(heading string, its []Item) {
		if len(its) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", heading)
		for _, it := range its {
			fmt.Fprintf(&b, "- %s %s: %s\n", code(it.Key), text(string(it.Action)), text(it.Summary))
		}
	}
	items("Added", d.Added)
	items("Removed", d.Removed)
	if len(d.Changed) > 0 {
		b.WriteString("\n## Changed\n\n")
		for _, c := range d.Changed {
			fields := make([]string, len(c.Fields))
			for i, f := range c.Fields {
				fields[i] = f
				if f == "action" {
					fields[i] = fmt.Sprintf("action (%s, now %s)", text(string(c.Before.Action)), text(string(c.After.Action)))
				}
			}
			fmt.Fprintf(&b, "- %s: %s. Now: %s\n", code(c.After.Key), strings.Join(fields, ", "), text(c.After.Summary))
		}
	}
	msgs := func(heading string, ms []messages.Message) {
		if len(ms) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", heading)
		for _, m := range ms {
			fmt.Fprintf(&b, "- %s %s: %s\n", m.Class.Label(), code(m.ID()), text(m.Text))
		}
	}
	msgs("New messages", d.NewMessages)
	msgs("No longer raised", d.GoneMessages)
	if len(d.ChangedMessages) > 0 {
		b.WriteString("\n## Changed messages\n\n")
		for _, c := range d.ChangedMessages {
			fmt.Fprintf(&b, "- %s %s: %s\n", c.After.Class.Label(), code(c.After.ID()), text(c.After.Text))
			if c.Before.Class != c.After.Class {
				fmt.Fprintf(&b, "  Class before: %s.\n", c.Before.Class.Label())
			}
			if c.Before.Text != c.After.Text {
				fmt.Fprintf(&b, "  Was: %s\n", text(c.Before.Text))
			}
		}
	}
	return []byte(b.String())
}

// RenderConfirmation returns confirmation.md, with the text of each accepted
// warning taken from p.
func RenderConfirmation(c Confirmation, p Plan) []byte {
	texts := map[string]string{}
	for _, m := range p.Messages {
		texts[m.ID()] = m.Text
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Confirmation of plan v%d\n\n", c.PlanVersion)
	fmt.Fprintf(&b, "- Confirmed: %s, by Atlassian account %s, through the %s\n", stamp(c.At), code(c.AccountID), text(c.Via))
	fmt.Fprintf(&b, "- plan.json: %s\n", code(c.PlanHash))
	b.WriteString("- No API shows whether the account is an org admin, so this record says who confirmed, not that they are one.\n")
	fmt.Fprintf(&b, "\n## Accepted warnings\n\n")
	if len(c.Accepted) == 0 {
		b.WriteString("The plan has no warnings.\n")
		return []byte(b.String())
	}
	for _, a := range c.Accepted {
		fmt.Fprintf(&b, "- %s: %s\n  Reason: %s\n", code(a.ID), text(texts[a.ID]), text(a.Reason))
	}
	return []byte(b.String())
}

// RenderRun returns run.md: the run's state and every item with its result.
func RenderRun(r Run) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Run %s of plan v%d\n\n", text(r.ID), r.PlanVersion)
	fmt.Fprintf(&b, "State: %s. Started %s", text(string(r.State)), stamp(r.StartedAt))
	if !r.EndedAt.IsZero() {
		fmt.Fprintf(&b, ", stopped %s", stamp(r.EndedAt))
	}
	b.WriteString(".\n\n")
	c := r.Counts()
	parts := make([]string, len(EntryStates))
	for i, s := range EntryStates {
		parts[i] = fmt.Sprintf("%d %s", c.Of(s), s)
	}
	fmt.Fprintf(&b, "%s: %s.\n\n", count(c.Total, "item", "items"), strings.Join(parts, ", "))
	b.WriteString("## Items in the order apply takes them\n\n")
	if len(r.Entries) == 0 {
		b.WriteString("None.\n")
		return []byte(b.String())
	}
	for i, e := range r.Entries {
		fmt.Fprintf(&b, "%d. %s %s: %s", i+1, code(e.Key), text(string(e.Action)), text(string(e.State)))
		var detail []string
		if e.Attempts > 1 {
			detail = append(detail, fmt.Sprintf("sent %d times", e.Attempts))
		}
		if e.Status != 0 {
			detail = append(detail, fmt.Sprintf("status %d", e.Status))
		}
		if e.RequestID != "" {
			// The ID comes from the API's answer, not from this tool.
			detail = append(detail, "request "+text(e.RequestID))
		}
		if len(detail) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(detail, ", "))
		}
		if e.PagerDuty != nil && e.JSM != nil {
			fmt.Fprintf(&b, ". PagerDuty %s is JSM %s", code(e.PagerDuty.String()), code(e.JSM.String()))
		}
		b.WriteString("\n")
		for _, m := range e.Messages {
			fmt.Fprintf(&b, "   - %s %s: %s\n", m.Class.Label(), code(m.ID()), text(m.Text))
		}
	}
	return []byte(b.String())
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
