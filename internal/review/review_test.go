package review_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// golden compares got with testdata/<name>, or rewrites the file with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	// Git may check the files out with CRLF line endings on Windows.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run go test -update to accept):\n%s", name, got)
	}
}

var (
	created = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	readPD  = time.Date(2026, 11, 12, 9, 58, 0, 0, time.UTC)
	readJSM = time.Date(2026, 11, 12, 9, 59, 30, 0, time.UTC)
)

// samplePlan has every action, a dependency across steps, and a message of
// every class. The names are pseudonyms.
func samplePlan() review.Plan {
	return review.Plan{
		Version:   2,
		CreatedAt: created,
		Reads: []review.Read{
			{Side: review.PagerDuty, At: readPD, Complete: true},
			{Side: review.JSM, At: readJSM, Complete: false},
		},
		Items: []review.Item{
			{Key: "users/PU1", Action: review.Create, Summary: "Invite Avery Example (avery@example.com)",
				PagerDuty: &review.Ref{Type: "user", ID: "PU1"}, Payload: json.RawMessage(`{"email": "avery@example.com"}`)},
			{Key: "users/PU2", Action: review.Map, Summary: "Blake Example is already in the organization",
				PagerDuty: &review.Ref{Type: "user", ID: "PU2"}, JSM: &review.Ref{Type: "account", ID: "712020:0000aaaa"}},
			{Key: "users/PU3", Action: review.Skip, Summary: "Casey Example is a stakeholder",
				PagerDuty: &review.Ref{Type: "user", ID: "PU3"}},
			{Key: "teams/PT1", Action: review.Create, Summary: "Create team Platform",
				DependsOn: []string{"users/PU1", "users/PU2"}, PagerDuty: &review.Ref{Type: "team", ID: "PT1"},
				Payload: json.RawMessage(`{"displayName":"Platform","teamType":"MEMBER_INVITE"}`)},
			{Key: "teams/PT1-ops", Action: review.Manual, Summary: "Turn on Operations for Platform by hand",
				DependsOn: []string{"teams/PT1"}},
		},
		Messages: []messages.Message{
			{Class: messages.Blocker, Code: "jsm_incomplete", Text: "The JSM snapshot is incomplete: the teams list answered 403."},
			{Class: messages.Warning, Code: "member_dropped", Item: "teams/PT1", Text: "Casey Example is not migrated, so they are not added to Platform."},
			{Class: messages.Warning, Code: "stakeholder", Item: "users/PU3", Text: "Casey Example is a PagerDuty stakeholder and is left out."},
			{Class: messages.ManualTask, Code: "enable_ops", Item: "teams/PT1-ops", Text: "Open the team in JSM and turn on Operations."},
			{Class: messages.Information, Code: "no_services", Text: "The account has no services."},
		},
	}
}

// readyPlan is samplePlan with nothing that stops apply: no blocker, and
// every read complete.
func readyPlan() review.Plan {
	p := samplePlan()
	p.Reads[1].Complete = true
	p.Messages = p.Messages[1:]
	return p
}

// previousPlan is samplePlan's version before: PU3 was still created, PT1
// had no dependency on PU2, and PU4 was planned.
func previousPlan() review.Plan {
	p := samplePlan()
	p.Version = 1
	p.Items = []review.Item{
		p.Items[0], p.Items[1],
		{Key: "users/PU3", Action: review.Create, Summary: "Invite Casey Example",
			PagerDuty: &review.Ref{Type: "user", ID: "PU3"}},
		{Key: "users/PU4", Action: review.Create, Summary: "Invite Drew Example",
			PagerDuty: &review.Ref{Type: "user", ID: "PU4"}},
		{Key: "teams/PT1", Action: review.Create, Summary: "Create team Platform",
			DependsOn: []string{"users/PU1"}, PagerDuty: &review.Ref{Type: "team", ID: "PT1"},
			Payload: json.RawMessage(`{ "displayName": "Platform", "teamType": "MEMBER_INVITE" }`)},
	}
	p.Messages = []messages.Message{
		p.Messages[0],
		{Class: messages.Warning, Code: "member_dropped", Item: "teams/PT1", Text: "An older text with the same ID."},
		{Class: messages.Information, Code: "no_escalations", Text: "The account has no escalation policies."},
	}
	return p
}

func sampleRun() review.Run {
	at := func(m int) time.Time { return created.Add(time.Duration(m) * time.Minute) }
	return review.Run{
		ID: "20261112T101500Z-a1b2", PlanVersion: 2, PlanHash: "sha256:00", State: review.Paused,
		StartedAt: at(15), EndedAt: at(20),
		Entries: []review.Entry{
			{Key: "users/PU1", Action: review.Create, State: review.Done, Attempts: 1, Status: 200, RequestID: "req-1",
				PagerDuty: &review.Ref{Type: "user", ID: "PU1"}, JSM: &review.Ref{Type: "account", ID: "712020:0000bbbb"}, At: at(16)},
			{Key: "users/PU2", Action: review.Map, State: review.Done,
				PagerDuty: &review.Ref{Type: "user", ID: "PU2"}, JSM: &review.Ref{Type: "account", ID: "712020:0000aaaa"}, At: at(16)},
			{Key: "users/PU3", Action: review.Skip, State: review.Done, PagerDuty: &review.Ref{Type: "user", ID: "PU3"}, At: at(16)},
			{Key: "teams/PT1", Action: review.Create, State: review.Unknown, Attempts: 1,
				PagerDuty: &review.Ref{Type: "team", ID: "PT1"}, At: at(19),
				Messages: []messages.Message{{Class: messages.ManualTask, Code: "unknown_outcome", Item: "teams/PT1",
					Text: "The create timed out. Check whether team Platform exists before resuming."}}},
			{Key: "teams/PT1-ops", Action: review.Manual, State: review.Pending},
		},
	}
}

func TestGoldenFiles(t *testing.T) {
	p, prev := samplePlan(), previousPlan()
	data, err := review.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "plan.json.golden", data)
	golden(t, "plan.md.golden", review.RenderPlan(p))
	golden(t, "risk-queue.md.golden", review.RenderRiskQueue(p))
	golden(t, "diff.md.golden", review.RenderDiff(review.Compare(&prev, p)))
	golden(t, "diff-first.md.golden", review.RenderDiff(review.Compare(nil, prev)))
	golden(t, "acceptance-template.yaml.golden", review.AcceptanceTemplate(p, review.Hash(data)))
	c := review.Confirmation{PlanVersion: 2, PlanHash: review.Hash(data), AccountID: "712020:0000cccc",
		At: created.Add(10 * time.Minute), Via: review.ViaFile, Accepted: []review.Accepted{
			{ID: "teams/PT1/member_dropped", Reason: "Casey left the company."},
			{ID: "users/PU3/stakeholder", Reason: "Stakeholders stay in PagerDuty until the cut-over."},
		}}
	golden(t, "confirmation.md.golden", review.RenderConfirmation(c, p))
	r := sampleRun()
	runJSON, err := review.EncodeRun(r)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "run.json.golden", runJSON)
	golden(t, "run.md.golden", review.RenderRun(r))
	mapping, err := review.MappingCSV(r)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "mapping.csv.golden", mapping)
}

func TestEncodeIsStableAndDecodesBack(t *testing.T) {
	p := samplePlan()
	a, err := review.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	// A plan whose times are in another zone, and whose payload is
	// formatted differently, is the same plan.
	q := samplePlan()
	q.CreatedAt = q.CreatedAt.In(time.FixedZone("UTC+3", 3*3600))
	q.Items[0].Payload = json.RawMessage("{\n  \"email\":\"avery@example.com\"\n}")
	b, err := review.Encode(q)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) || review.Hash(a) != review.Hash(b) {
		t.Errorf("the same plan encoded differently:\n%s\n%s", a, b)
	}
	back, err := review.Decode(a)
	if err != nil {
		t.Fatal(err)
	}
	again, err := review.Encode(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, again) {
		t.Errorf("plan.json does not survive a round trip:\n%s\n%s", a, again)
	}
}

func TestDecodeIsStrict(t *testing.T) {
	data, err := review.Encode(samplePlan())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		data []byte
		want string
	}{
		"unknown field": {bytes.Replace(data, []byte(`"version": 2,`), []byte(`"version": 2, "extra": true,`), 1), "unknown field"},
		"second value":  {append(append([]byte{}, data...), []byte("{}")...), "more than one JSON value"},
		"bad plan":      {bytes.Replace(data, []byte(`"depends_on": [`), []byte(`"depends_on": ["teams/nope", `), 1), "not earlier in the plan"},
	}
	for name, c := range cases {
		if _, err := review.Decode(c.data); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Decode = %v, want %q", name, err, c.want)
		}
	}
}

func TestHashes(t *testing.T) {
	p := samplePlan()
	a, _ := review.Encode(p)
	q := samplePlan()
	q.Version, q.CreatedAt = 3, created.Add(time.Hour)
	q.Reads[1].Complete = true
	b, _ := review.Encode(q)
	if review.Hash(a) == review.Hash(b) {
		t.Errorf("plan.json hash ignores the version and time")
	}
	ca, err := review.ContentHash(p)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := review.ContentHash(q)
	if ca != cb {
		t.Errorf("content hash changed with only the version, time and reads")
	}
	q.Items[3].Summary = "Create team Platform Ops"
	if cc, _ := review.ContentHash(q); cc == ca {
		t.Errorf("content hash did not change with an item")
	}
}

func TestEncodeRedactsTextsAndRefusesSensitivePayloads(t *testing.T) {
	p := samplePlan()
	p.Items[0].Summary = "Invite Avery, phone +1 555 010 0199"
	p.Messages[4].Text = "Calendar https://acme.pagerduty.com/private/feed123 is not migrated."
	data, err := review.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"555", "feed123"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("plan.json holds %q", leak)
		}
	}
	for name, payload := range map[string]string{
		"phone number": `{"note":"+1 555 010 0199"}`,
		"contact to":   `{"method":"sms","to":"90-5551112233"}`,
		"routing key":  `{"routing_key":"abc"}`,
		"calendar URL": `{"feed":"https://acme.pagerduty.com/private/feed123"}`,
	} {
		p := samplePlan()
		p.Items[0].Payload = json.RawMessage(payload)
		_, err := review.Encode(p)
		if err == nil || !strings.Contains(err.Error(), "sensitive data") {
			t.Errorf("%s: Encode = %v, want a refusal", name, err)
			continue
		}
		if strings.Contains(err.Error(), "555") || strings.Contains(err.Error(), "feed123") || strings.Contains(err.Error(), "abc") {
			t.Errorf("%s: the refusal quotes the value: %v", name, err)
		}
	}
}

// Each case breaks one thing, mostly on an extra item nothing refers to, so
// the error it expects can only come from the check under test.
func TestCheckFindsEveryProblem(t *testing.T) {
	extra := func(edit func(*review.Item)) func(*review.Plan) {
		return func(p *review.Plan) {
			it := review.Item{Key: "extra/X1", Action: review.Skip, PagerDuty: &review.Ref{Type: "x", ID: "X1"}}
			edit(&it)
			p.Items = append(p.Items, it)
		}
	}
	format := string(rune(0x200b))
	cases := map[string]struct {
		breakIt func(*review.Plan)
		want    string
	}{
		"version 0":               {func(p *review.Plan) { p.Version = 0 }, "versions start at 1"},
		"bad key":                 {extra(func(it *review.Item) { it.Key = "Extra X1" }), "key must be"},
		"key without a step":      {extra(func(it *review.Item) { it.Key = "X1" }), "key must be"},
		"control in key":          {extra(func(it *review.Item) { it.Key = "extra/X\x1b1" }), "key must be"},
		"invalid UTF-8 key":       {extra(func(it *review.Item) { it.Key = "extra/X\xff1" }), "key must be"},
		"format character in key": {extra(func(it *review.Item) { it.Key = "extra/X" + format + "1" }), "key must be"},
		"control in reference":    {extra(func(it *review.Item) { it.PagerDuty.ID = "X\x001" }), "reference is not a plain type and ID"},
		"duplicate key":           {func(p *review.Plan) { extra(func(*review.Item) {})(p); extra(func(*review.Item) {})(p) }, "planned twice"},
		"unknown action":          {extra(func(it *review.Item) { it.Action = "delete" }), "unknown action"},
		"map without JSM":         {extra(func(it *review.Item) { it.Action = review.Map }), "map needs the JSM object"},
		"create with JSM":         {extra(func(it *review.Item) { it.Action, it.JSM = review.Create, &review.Ref{Type: "x", ID: "Y"} }), "create cannot name"},
		"map without PagerDuty": {extra(func(it *review.Item) {
			it.Action, it.JSM, it.PagerDuty = review.Map, &review.Ref{Type: "x", ID: "Y"}, nil
		}), "map needs the PagerDuty object"},
		"dependency later":        {func(p *review.Plan) { p.Items[0].DependsOn = []string{"teams/PT1"} }, "not earlier in the plan"},
		"dependency on itself":    {extra(func(it *review.Item) { it.DependsOn = []string{"extra/X1"} }), "not earlier in the plan"},
		"missing dependency":      {extra(func(it *review.Item) { it.DependsOn = []string{"users/PU9"} }), "not earlier in the plan"},
		"dependency twice":        {extra(func(it *review.Item) { it.DependsOn = []string{"users/PU1", "users/PU1"} }), "twice"},
		"payload not JSON":        {extra(func(it *review.Item) { it.Payload = json.RawMessage(`{`) }), "not one JSON value"},
		"payload with more after": {extra(func(it *review.Item) { it.Payload = json.RawMessage(`{"a":1} trailing`) }), "not one JSON value"},
		"incomplete, no blocker":  {func(p *review.Plan) { p.Messages = p.Messages[1:] }, "no blocker"},
		"message bad code":        {func(p *review.Plan) { p.Messages[0].Code = "Bad Code" }, "snake_case"},
		"message bad class":       {func(p *review.Plan) { p.Messages[0].Class = "error" }, "unknown class"},
		"message about no item":   {func(p *review.Plan) { p.Messages[1].Item = "teams/PT9" }, "no such item"},
		"message twice":           {func(p *review.Plan) { p.Messages[2] = p.Messages[1] }, "raised twice"},
		"read of an unknown side": {func(p *review.Plan) { p.Reads[0].Side = "opsgenie" }, "unknown side"},
	}
	for name, c := range cases {
		p := samplePlan()
		c.breakIt(&p)
		err := p.Check()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Check = %v, want %q", name, err, c.want)
		}
	}
	if err := samplePlan().Check(); err != nil {
		t.Errorf("the sample plan: %v", err)
	}
	p := samplePlan()
	p.Items[0].Action, p.Items[1].JSM = "delete", nil
	if err := p.Check(); err == nil || !strings.Contains(err.Error(), "delete") || !strings.Contains(err.Error(), "map needs") {
		t.Errorf("Check must report every problem at once: %v", err)
	}
}

func TestSummarize(t *testing.T) {
	s := review.Summarize(samplePlan())
	want := []review.StepCounts{{Step: "users", Create: 1, Map: 1, Skip: 1}, {Step: "teams", Create: 1, Manual: 1}}
	if len(s.Steps) != len(want) || s.Steps[0] != want[0] || s.Steps[1] != want[1] {
		t.Errorf("Steps = %+v, want %+v", s.Steps, want)
	}
	if s.Calls != 2 || s.Messages.Blockers != 1 || s.Messages.Warnings != 2 {
		t.Errorf("Summary = %+v", s)
	}
}

func TestCompare(t *testing.T) {
	prev, next := previousPlan(), samplePlan()
	d := review.Compare(&prev, next)
	keys := func(its []review.Item) []string {
		var out []string
		for _, it := range its {
			out = append(out, it.Key)
		}
		return out
	}
	if got := keys(d.Added); strings.Join(got, ",") != "teams/PT1-ops" {
		t.Errorf("Added = %v", got)
	}
	if got := keys(d.Removed); strings.Join(got, ",") != "users/PU4" {
		t.Errorf("Removed = %v", got)
	}
	if len(d.Changed) != 2 || d.Changed[0].After.Key != "users/PU3" || d.Changed[1].After.Key != "teams/PT1" {
		t.Fatalf("Changed = %+v", d.Changed)
	}
	// The payload of PT1 differs only in its spaces, as one read back from
	// the indented plan.json does.
	if got := strings.Join(d.Changed[1].Fields, ","); got != "depends_on" {
		t.Errorf("PT1 changed fields = %s", got)
	}
	if len(d.NewMessages) != 3 || len(d.GoneMessages) != 1 || d.GoneMessages[0].Code != "no_escalations" {
		t.Errorf("messages: new %v, gone %v", d.NewMessages, d.GoneMessages)
	}
	if same := review.Compare(&next, next); len(same.Added)+len(same.Removed)+len(same.Changed) != 0 {
		t.Errorf("a plan differs from itself: %+v", same)
	}
}

func filledTemplate(t *testing.T, p review.Plan, hash string) []byte {
	t.Helper()
	tmpl := review.AcceptanceTemplate(p, hash)
	return bytes.ReplaceAll(tmpl, []byte(`reason: ""`), []byte(`reason: "Agreed with the team."`))
}

func TestAcceptanceTemplateRoundTrips(t *testing.T) {
	p := samplePlan()
	data, _ := review.Encode(p)
	hash := review.Hash(data)
	a, err := review.ParseAcceptance(filledTemplate(t, p, hash))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Accept(p, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "teams/PT1/member_dropped" || got[1].ID != "users/PU3/stakeholder" {
		t.Errorf("Accept = %+v", got)
	}
	// The unfilled template accepts nothing.
	empty, err := review.ParseAcceptance(review.AcceptanceTemplate(p, hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Accept(p, hash); !errors.Is(err, review.ErrAcceptance) || !strings.Contains(err.Error(), "without a reason") {
		t.Errorf("unfilled template: %v", err)
	}
}

func TestAcceptRefuses(t *testing.T) {
	p := samplePlan()
	data, _ := review.Encode(p)
	hash := review.Hash(data)
	ok := review.Acceptance{PlanVersion: 2, PlanHash: hash, Warnings: []review.Accepted{
		{ID: "teams/PT1/member_dropped", Reason: "a"}, {ID: "users/PU3/stakeholder", Reason: "b"},
	}}
	cases := map[string]struct {
		edit func(*review.Acceptance)
		want string
	}{
		"other version": {func(a *review.Acceptance) { a.PlanVersion = 1 }, "for plan v1, not v2"},
		"other hash":    {func(a *review.Acceptance) { a.PlanHash = "sha256:00" }, "plan_hash"},
		"missing":       {func(a *review.Acceptance) { a.Warnings = a.Warnings[:1] }, `not accepted: "users/PU3/stakeholder"`},
		"blocker": {func(a *review.Acceptance) {
			a.Warnings = append(a.Warnings, review.Accepted{ID: "jsm_incomplete", Reason: "x"})
		}, `not a warning of this plan: "jsm_incomplete"`},
		"twice":     {func(a *review.Acceptance) { a.Warnings = append(a.Warnings, a.Warnings[0]) }, `accepted twice: "teams/PT1/member_dropped"`},
		"no reason": {func(a *review.Acceptance) { a.Warnings[1].Reason = "  " }, `accepted without a reason: "users/PU3/stakeholder"`},
	}
	for name, c := range cases {
		a := ok
		a.Warnings = append([]review.Accepted{}, ok.Warnings...)
		c.edit(&a)
		_, err := a.Accept(p, hash)
		if !errors.Is(err, review.ErrAcceptance) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if _, err := ok.Accept(p, hash); err != nil {
		t.Errorf("a complete acceptance: %v", err)
	}
}

func TestParseAcceptanceRejectsUnknownKeys(t *testing.T) {
	_, err := review.ParseAcceptance([]byte("plan_version: 2\nplan_hash: x\nwarnings:\n  - id: a\n    reasn: typo\n"))
	if !errors.Is(err, review.ErrAcceptance) {
		t.Errorf("a typo in a key: %v", err)
	}
	if _, err := review.ParseAcceptance(nil); !errors.Is(err, review.ErrAcceptance) {
		t.Errorf("an empty file: %v", err)
	}
}

func TestRunCheck(t *testing.T) {
	for name, edit := range map[string]func(*review.Run){
		"path in ID":                 func(r *review.Run) { r.ID = "../x" },
		"bad entry key":              func(r *review.Run) { r.Entries[0].Key = "users/P U1" },
		"unknown state":              func(r *review.Run) { r.State = "stopped" },
		"entry state":                func(r *review.Run) { r.Entries[0].State = "skipped" },
		"unknown action":             func(r *review.Run) { r.Entries[0].Action = "create`\n\n# Injected" },
		"message about another item": func(r *review.Run) { r.Entries[3].Messages[0].Item = "users/PU1" },
		"sensitive key":              func(r *review.Run) { r.Entries[0].Key = "contacts/+14155550123" },
		"bad message":                func(r *review.Run) { r.Entries[3].Messages[0].Class = "error" },
	} {
		r := sampleRun()
		edit(&r)
		if err := r.Check(); err == nil {
			t.Errorf("%s: Check = nil", name)
		}
	}
	if c := sampleRun().Counts(); c != (review.RunCounts{Total: 5, Done: 3, Unknown: 1, Pending: 1}) {
		t.Errorf("Counts = %+v", c)
	}
}

func newWorkspace(t *testing.T) *store.Workspace {
	t.Helper()
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestWritePlanVersion(t *testing.T) {
	ws := newWorkspace(t)
	prev, p := previousPlan(), samplePlan()
	if _, err := review.WritePlanVersion(ws, prev, nil); err != nil {
		t.Fatal(err)
	}
	hash, err := review.WritePlanVersion(ws, p, &prev)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ws.ArtifactsDir(), review.PlanDir(2))
	for _, name := range []string{review.PlanFile, review.PlanMarkdownFile, review.DiffFile, review.RiskQueueFile, review.AcceptanceTemplateFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", name, info.Mode().Perm())
		}
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, review.PlanFile))
	if err != nil {
		t.Fatal(err)
	}
	if review.Hash(onDisk) != hash {
		t.Errorf("the returned hash is not the hash of plan.json on disk")
	}
	diff, _ := os.ReadFile(filepath.Join(dir, review.DiffFile))
	if !bytes.HasPrefix(diff, []byte("# Changes from plan v1 to v2")) {
		t.Errorf("diff.md does not compare with v1:\n%s", diff)
	}

	// A version is never replaced, and a failed write leaves nothing behind.
	if _, err := review.WritePlanVersion(ws, p, &prev); err == nil {
		t.Errorf("writing v2 again succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(ws.ArtifactsDir(), "plans"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "v1,v2" {
		t.Errorf("plans/ holds %v, want only v1 and v2", names)
	}
	again, _ := os.ReadFile(filepath.Join(dir, review.PlanFile))
	if !bytes.Equal(onDisk, again) {
		t.Errorf("plan.json of v2 changed")
	}

	bad := samplePlan()
	bad.Version = 3
	bad.Items[0].Payload = json.RawMessage(`{"to":"x@example.com"}`)
	if _, err := review.WritePlanVersion(ws, bad, &p); err == nil {
		t.Errorf("a plan with contact data was written")
	}
	if _, err := os.Stat(filepath.Join(ws.ArtifactsDir(), review.PlanDir(3))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused plan left a folder: %v", err)
	}
}

func TestWriteConfirmationAndRun(t *testing.T) {
	ws := newWorkspace(t)
	p := readyPlan()
	hash, err := review.WritePlanVersion(ws, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := review.Confirmation{PlanVersion: 2, PlanHash: hash, AccountID: "712020:0000cccc", At: created, Via: review.ViaPrompt,
		Accepted: []review.Accepted{{ID: "teams/PT1/member_dropped", Reason: "a"}, {ID: "users/PU3/stakeholder", Reason: "b"}}}
	if err := review.WriteConfirmation(ws, c, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.ArtifactsDir(), review.PlanDir(2), review.ConfirmationFile)); err != nil {
		t.Error(err)
	}
	r := sampleRun()
	record, err := review.WriteRun(ws, r)
	if err != nil {
		t.Fatal(err)
	}
	if record != filepath.Join("runs", r.ID, review.RunMarkdownFile) {
		t.Errorf("record = %q", record)
	}
	// The record is rebuilt whole each time.
	r.State, r.Entries[4].State = review.Completed, review.Done
	if _, err := review.WriteRun(ws, r); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(ws.ArtifactsDir(), record))
	if !bytes.Contains(got, []byte("State: completed.")) {
		t.Errorf("run.md was not rebuilt:\n%s", got)
	}
	r.ID = "../escape"
	if _, err := review.WriteRun(ws, r); err == nil {
		t.Errorf("a run ID with a path was written")
	}
}

func TestWriteConfirmationRefuses(t *testing.T) {
	ws := newWorkspace(t)
	p := readyPlan()
	hash, err := review.WritePlanVersion(ws, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	good := review.Confirmation{PlanVersion: 2, PlanHash: hash, AccountID: "712020:0000cccc", At: created, Via: review.ViaFile,
		Accepted: []review.Accepted{{ID: "teams/PT1/member_dropped", Reason: "a"}, {ID: "users/PU3/stakeholder", Reason: "b"}}}
	other := readyPlan()
	other.Items[0].Summary = "Invite someone else"
	cases := map[string]struct {
		edit func(*review.Confirmation)
		plan review.Plan
	}{
		"another hash":         {func(c *review.Confirmation) { c.PlanHash = "sha256:00" }, p},
		"unknown way":          {func(c *review.Confirmation) { c.Via = "carrier pigeon" }, p},
		"account on two lines": {func(c *review.Confirmation) { c.AccountID = "712020:x\n\n# Injected" }, p},
		"no account":           {func(c *review.Confirmation) { c.AccountID = "" }, p},
		"no time":              {func(c *review.Confirmation) { c.At = time.Time{} }, p},
		"a warning left out":   {func(c *review.Confirmation) { c.Accepted = c.Accepted[:1] }, p},
		"not the written plan": {func(*review.Confirmation) {}, other},
	}
	for name, tc := range cases {
		c := good
		c.Accepted = append([]review.Accepted{}, good.Accepted...)
		tc.edit(&c)
		if err := review.WriteConfirmation(ws, c, tc.plan); err == nil {
			t.Errorf("%s: written", name)
		}
	}
	if _, err := os.Stat(filepath.Join(ws.ArtifactsDir(), review.PlanDir(2), review.ConfirmationFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused confirmation was written: %v", err)
	}
	// The good one is written, so each refusal above had its own cause.
	if err := review.WriteConfirmation(ws, good, p); err != nil {
		t.Fatalf("the good confirmation: %v", err)
	}
	// Finding: a second confirmation replaced the first, which names who
	// confirmed the run that started under it.
	second := good
	second.AccountID, second.At = "712020:0000dddd", created.Add(48*time.Hour)
	if err := review.WriteConfirmation(ws, second, p); !errors.Is(err, review.ErrAlreadyConfirmed) {
		t.Errorf("a second confirmation: %v", err)
	}
	record, _ := os.ReadFile(filepath.Join(ws.ArtifactsDir(), review.PlanDir(2), review.ConfirmationFile))
	if !bytes.Contains(record, []byte("0000cccc")) {
		t.Errorf("the first record was replaced:\n%s", record)
	}

	// Finding: a plan with a blocker, or built from an incomplete read, could
	// be confirmed.
	for _, tc := range []struct {
		version int
		edit    func(*review.Plan)
		want    string
	}{
		{3, func(p *review.Plan) { p.Messages = samplePlan().Messages }, "the plan has 1 blocker"},
		// Check wants a blocker with an incomplete read, so this plan has one too.
		{4, func(p *review.Plan) { p.Reads[1].Complete = false; p.Messages = samplePlan().Messages }, "the JSM read is incomplete"},
	} {
		blocked := readyPlan()
		blocked.Version = tc.version
		tc.edit(&blocked)
		h, err := review.WritePlanVersion(ws, blocked, nil)
		if err != nil {
			t.Fatal(err)
		}
		c := good
		c.PlanVersion, c.PlanHash = blocked.Version, h
		if err := review.WriteConfirmation(ws, c, blocked); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("v%d: WriteConfirmation = %v, want %q", tc.version, err, tc.want)
		}
	}

	// A version that was never written gets no folder from its confirmation,
	// so the version can still be written later.
	v7 := readyPlan()
	v7.Version = 7
	c := good
	c.PlanVersion = 7
	if err := review.WriteConfirmation(ws, c, v7); !errors.Is(err, review.ErrNoPlanVersion) {
		t.Errorf("confirming an unwritten version: %v", err)
	}
	if _, err := review.WritePlanVersion(ws, v7, nil); err != nil {
		t.Errorf("writing v7 after the refused confirmation: %v", err)
	}
}

// Finding: the payload was screened as raw text, so a nested key and a URL
// after an escaped line break got through, while a UUID or a name refused
// the whole plan.
func TestPayloadScreening(t *testing.T) {
	refused := map[string]string{
		"nested contact to":      `{"contact":{"method":"sms","to":"4155550123"}}`,
		"nested password":        `{"auth":{"password":"hunter2hunter2"}}`,
		"nested routing key":     `{"integration":{"routing_key":"R0123456789ABCDEF"}}`,
		"in a list":              `{"rules":[{"device":"iPhone"}]}`,
		"calendar after a break": `{"description":"Team calendar:\nhttps://acme.pagerduty.com/private/abc123/feed"}`,
		"phone after a break":    `{"description":"NOC contact:\n1-4155550123"}`,
		"phone with a plus":      `{"note":"+1 415 555 0123"}`,
		"sensitive key as a key": `{"+14155550123":"x"}`,
	}
	for name, payload := range refused {
		p := samplePlan()
		p.Items[0].Payload = json.RawMessage(payload)
		_, err := review.Encode(p)
		if err == nil || !strings.Contains(err.Error(), "sensitive data") {
			t.Errorf("%s: Encode = %v, want a refusal", name, err)
		}
	}
	accepted := map[string]func(*review.Plan){
		"UUID in the payload": func(p *review.Plan) {
			p.Items[0].Payload = json.RawMessage(`{"teamId":"3f2a8c1e-7d44-4b5e-8001-000012345678"}`)
		},
		"UUID JSM reference": func(p *review.Plan) { p.Items[1].JSM.ID = "712020:1b9d6bcd-bbfd-4b2d-9123-123456789012" },
		"a name with Basic": func(p *review.Plan) {
			p.Items[0].Payload = json.RawMessage(`{"name":"Basic Schedule","timezone":"UTC"}`)
		},
		"an empty sensitive key": func(p *review.Plan) { p.Items[0].Payload = json.RawMessage(`{"to":"","address":null}`) },
	}
	for name, edit := range accepted {
		p := samplePlan()
		edit(&p)
		if _, err := review.Encode(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// One random UUID in about 2,200 used to be refused.
	for i := range 20000 {
		p := samplePlan()
		id := fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012d", i*7919, i%65536, i%4096, (i*31)%4096, i*1000003)
		p.Items[1].JSM.ID = id
		if _, err := review.Encode(p); err != nil {
			t.Fatalf("UUID %s: %v", id, err)
		}
	}
}

// Finding: a refusal quoted the key or reference that held the value.
func TestRefusalsDoNotQuoteTheValue(t *testing.T) {
	p := samplePlan()
	p.Items = append(p.Items, review.Item{Key: "contacts/+14155550123", Action: review.Skip,
		PagerDuty: &review.Ref{Type: "contact_method", ID: "R0123?token=abcdef123456"}})
	err := p.Check()
	if err == nil {
		t.Fatal("Check = nil")
	}
	for _, leak := range []string{"4155550123", "abcdef123456"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error holds %q: %v", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "item 6") {
		t.Errorf("the item is not named by its position: %v", err)
	}
}

// Finding: diff.md said "Nothing changed." when a message's text or class
// changed.
func TestDiffShowsChangedMessages(t *testing.T) {
	a, b := samplePlan(), samplePlan()
	b.Version = 3
	b.Messages[1].Text = "40 members are not migrated."
	b.Messages[4].Class = messages.Warning
	d := review.Compare(&a, b)
	if d.Empty() || len(d.ChangedMessages) != 2 {
		t.Fatalf("ChangedMessages = %+v", d.ChangedMessages)
	}
	out := string(review.RenderDiff(d))
	for _, want := range []string{"## Changed messages", "40 members are not migrated.", "Was: Casey Example is not migrated", "Class before: information."} {
		if !strings.Contains(out, want) {
			t.Errorf("diff.md lacks %q:\n%s", want, out)
		}
	}
}

// Finding: the version before, read back from its plan.json, had its
// summaries redacted, so unchanged items looked changed.
func TestCompareWithAVersionReadBack(t *testing.T) {
	v1 := samplePlan()
	v1.Version = 1
	v1.Items[0].Summary = "Invite Avery, desk phone +1 555 010 0199"
	data, err := review.Encode(v1)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := review.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	v2 := v1
	v2.Version = 2
	v2.Items = append([]review.Item{}, v1.Items...)
	v2.Items[1].Summary = "Blake Example is in the organization"
	d := review.Compare(&prev, v2)
	if len(d.Changed) != 1 || d.Changed[0].After.Key != "users/PU2" {
		t.Errorf("Changed = %+v", d.Changed)
	}
}

// Finding: texts were redacted before they were put on one line, and once.
func TestTextsAreCleanedBeforeTheyAreWritten(t *testing.T) {
	p := samplePlan()
	p.Items[0].Summary = "Call the NOC on +1 415 555\n0123"
	p.Messages[4].Text = "On-call line +1 415 555 0123https://acme.pagerduty.com/private/feed123/x.ics"
	data, err := review.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range [][]byte{data, review.RenderPlan(p), review.RenderRiskQueue(p)} {
		for _, leak := range []string{"0123", "feed123"} {
			if bytes.Contains(out, []byte(leak)) {
				t.Errorf("%q reached the output:\n%s", leak, out)
			}
		}
	}
	back, err := review.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := review.Encode(back)
	h1, _ := review.ContentHash(p)
	h2, _ := review.ContentHash(back)
	if !bytes.Equal(data, again) || h1 != h2 {
		t.Errorf("a plan read back from its plan.json differs from it")
	}
}

// Finding: Decode took a file the JSON decoder reads loosely.
func TestDecodeRefusesAFileNotAsWritten(t *testing.T) {
	data, err := review.Encode(samplePlan())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"a key given twice":   bytes.Replace(data, []byte(`"version": 2,`), []byte(`"version": 1, "version": 2,`), 1),
		"a key in capitals":   bytes.Replace(data, []byte(`"version": 2,`), []byte(`"Version": 2,`), 1),
		"other indentation":   bytes.ReplaceAll(data, []byte("\n  "), []byte("\n    ")),
		"no final line break": bytes.TrimSuffix(data, []byte("\n")),
	}
	for name, bad := range cases {
		if _, err := review.Decode(bad); !errors.Is(err, review.ErrNotAsWritten) {
			t.Errorf("%s: Decode = %v, want ErrNotAsWritten", name, err)
		}
	}
}

// Finding: links, images and HTML in a PagerDuty name reached plan.md as
// Markdown.
func TestMarkdownInTextsIsEscaped(t *testing.T) {
	p := samplePlan()
	p.Items[1].Summary = "Team [Approved by security](https://evil.example/x) <img src=x> *bold* `code`"
	p.Items[1].JSM.ID = "[x](https://evil.example)"
	out := string(review.RenderPlan(p))
	want := "2. `users/PU2` map (JSM `account [x](https://evil.example)`): " +
		"Team \\[Approved by security\\](https://evil.example/x) \\<img src=x\\> \\*bold\\* \\`code\\`\n"
	if !strings.Contains(out, want) {
		t.Errorf("plan.md:\n%s\nwant the line\n%s", out, want)
	}
}

func TestAcceptanceEdgeCases(t *testing.T) {
	p := samplePlan()
	data, _ := review.Encode(p)
	hash := review.Hash(data)
	invisible := review.Acceptance{PlanVersion: 2, PlanHash: hash, Warnings: []review.Accepted{
		{ID: "teams/PT1/member_dropped", Reason: "\u200b\x00\t"}, {ID: "users/PU3/stakeholder", Reason: "b"},
	}}
	if _, err := invisible.Accept(p, hash); err == nil || !strings.Contains(err.Error(), "without a reason") {
		t.Errorf("a reason of invisible characters: %v", err)
	}
	if _, err := review.ParseAcceptance([]byte("plan_version: 2\nplan_hash: x\nwarnings: []\n---\nplan_version: 9\n")); !errors.Is(err, review.ErrAcceptance) {
		t.Errorf("a second document: %v", err)
	}
	odd := review.Acceptance{PlanVersion: 2, PlanHash: hash, Warnings: append(invisible.Warnings, review.Accepted{ID: "x\x1b]0;owned\a\nError: fake", Reason: "c"})}
	_, err := odd.Accept(p, hash)
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\a\n") {
		t.Errorf("an unknown ID reached the error raw: %q", err)
	}
}

func TestWritePlanVersionRemovesUncommittedFolders(t *testing.T) {
	ws := newWorkspace(t)
	stale := filepath.Join("plans", ".v1.tmp-0123abcd")
	if err := ws.WriteArtifact(filepath.Join(stale, review.PlanFile), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := review.WritePlanVersion(ws, samplePlan(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.ArtifactsDir(), stale)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale folder is still there: %v", err)
	}
}

func TestRunRecordCleansAndEscapes(t *testing.T) {
	r := sampleRun()
	r.Entries[0].RequestID = "Bearer abcdefgh12345678"
	r.Entries[1].JSM.ID = "-Nq3xYz_base64url"
	data, err := review.EncodeRun(r)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("abcdefgh12345678")) {
		t.Errorf("run.json holds the request ID's token:\n%s", data)
	}
	csv, err := review.MappingCSV(r)
	if err != nil {
		t.Fatalf("an ID that starts like a formula stopped the record: %v", err)
	}
	if !bytes.Contains(csv, []byte(",'-Nq3xYz_base64url\n")) {
		t.Errorf("mapping.csv does not escape the formula cell:\n%s", csv)
	}
}

// Second review, F2: in quoted text a line break is the two characters \n,
// and the rules for a calendar URL and a JSM number missed what came after.
func TestEscapedLineBreaksDoNotHideSensitiveData(t *testing.T) {
	p := samplePlan()
	p.Items[0].Summary = `Create schedule "Primary on-call\nhttps://acme.pagerduty.com/private/abc123feed/feed"`
	p.Messages[1].Text = `The description "NOC contact:\n1-4155550123" is kept as it is.`
	data, err := review.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string][]byte{
		"plan.json": data, "plan.md": review.RenderPlan(p), "risk-queue.md": review.RenderRiskQueue(p),
		"acceptance-template.yaml": review.AcceptanceTemplate(p, review.Hash(data)),
	} {
		for _, leak := range []string{"abc123feed", "4155550123"} {
			if bytes.Contains(out, []byte(leak)) {
				t.Errorf("%s holds %q", name, leak)
			}
		}
	}
	var b bytes.Buffer
	_ = messages.WriteText(&b, p.Messages)
	if strings.Contains(b.String(), "4155550123") {
		t.Errorf("WriteText holds the number: %s", b.String())
	}
	q := samplePlan()
	q.Items[0].Payload = json.RawMessage(`{"description":"Team calendar:\\nhttps://acme.pagerduty.com/private/abc123feed/feed and NOC:\\n1-4155550123"}`)
	if _, err := review.Encode(q); err == nil {
		t.Errorf("a payload with an escaped line break before a calendar URL was accepted")
	}
}

// Second review, F3 and F4.
func TestPayloadScreeningClosesTheGaps(t *testing.T) {
	refused := map[string]string{
		"camelCase token":            `{"accessToken":"tok123456789"}`,
		"camelCase secret":           `{"clientSecret":"s3cr3t"}`,
		"camelCase routing key":      `{"routingKey":"R0123456789"}`,
		"camelCase phone":            `{"phoneNumber":"4155550123"}`,
		"camelCase calendar":         `{"webCalUrl":"https://x.example/feed"}`,
		"Bearer under a plain key":   `{"headers":[{"name":"Authorization","value":"Bearer eyJhbGciOiJIUzI1NiJ9.abcdef"}]}`,
		"Basic under a plain key":    `{"webhook":{"auth":"Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ=="}}`,
		"a key given twice":          `{"to":"+14155550123","to":""}`,
		"a number under a phone key": `{"phone":14155550123}`,
	}
	for name, payload := range refused {
		p := samplePlan()
		p.Items[0].Payload = json.RawMessage(payload)
		data, err := review.Encode(p)
		if err == nil {
			t.Errorf("%s: Encode accepted it:\n%s", name, data)
		}
	}
	// A key given twice is refused even when neither value is sensitive: the
	// step's decoder would keep the last value, plan.md readers see both.
	p := samplePlan()
	p.Items[0].Payload = json.RawMessage(`{"name":"Platform","name":"Payments"}`)
	if _, err := review.Encode(p); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a key given twice: %v", err)
	}
	// Finding F4: a second value after the payload was dropped without a word.
	p = samplePlan()
	p.Items[0].Payload = json.RawMessage(`{"email":"avery@example.com"} trailing`)
	if _, err := review.Encode(p); err == nil || !strings.Contains(err.Error(), "not one JSON value") {
		t.Errorf("a payload with more after it: %v", err)
	}
}

// Second review, F5: prose and ordinary values refused whole plans.
func TestPayloadScreeningAcceptsOrdinaryData(t *testing.T) {
	for _, payload := range []string{
		`{"name":"Platform","description":"Escalate to: Tier 2 after 30 minutes"}`,
		`{"description":"Reports to: VP Engineering"}`,
		`{"description":"Address: Building 4, floor 2"}`,
		`{"description":"Authorization: handled by the SRE lead"}`,
		`{"description":"Handles 100-200000 requests a day"}`,
		`{"description":"Ticket 2024-123456 tracks this"}`,
		`{"timeRestriction":{"from":"09:00","to":"17:00"}}`,
		`{"name":"Basic Schedule","description":"Bearer responsibilities rotate"}`,
		`{"number":14155550123}`,
	} {
		p := samplePlan()
		p.Items[0].Payload = json.RawMessage(payload)
		if _, err := review.Encode(p); err != nil {
			t.Errorf("%s: %v", payload, err)
		}
	}
	// A refusal names the field that holds the data.
	p := samplePlan()
	p.Items[0].Payload = json.RawMessage(`{"contact":{"method":"sms","to":"90-5551112233"}}`)
	_, err := review.Encode(p)
	if err == nil || !strings.Contains(err.Error(), `"payload.contact.to"`) || strings.Contains(err.Error(), "5551112233") {
		t.Errorf("the refusal does not name the field, or shows the value: %v", err)
	}
}

// Second review, F6: an order-only change gave an empty diff, but a new
// content hash.
func TestDiffShowsAChangeOfOrder(t *testing.T) {
	a, b := samplePlan(), samplePlan()
	b.Version = 3
	b.Items[0], b.Items[2] = b.Items[2], b.Items[0]
	b.Messages[1], b.Messages[2] = b.Messages[2], b.Messages[1]
	ha, _ := review.ContentHash(a)
	hb, _ := review.ContentHash(b)
	d := review.Compare(&a, b)
	if ha == hb || d.Empty() || !d.ItemsReordered || !d.MessagesReordered {
		t.Fatalf("hashes differ %v, diff %+v", ha != hb, d)
	}
	out := string(review.RenderDiff(d))
	if !strings.Contains(out, "## Order") || strings.Contains(out, "Nothing changed") {
		t.Errorf("diff.md:\n%s", out)
	}
	if same := review.Compare(&a, a); !same.Empty() {
		t.Errorf("a plan differs from itself: %+v", same)
	}
}

// Second review, F8: a later version of the CLI may clean texts differently;
// it must still read the plan.json an earlier one wrote.
func TestDecodeReadsTextsAsWritten(t *testing.T) {
	data, err := review.Encode(samplePlan())
	if err != nil {
		t.Fatal(err)
	}
	// What an older version left alone that this one would clean.
	older := bytes.Replace(data, []byte(`"summary": "Create team Platform"`), []byte(`"summary": "webhook config: to=the on-call channel"`), 1)
	p, err := review.Decode(older)
	if err != nil {
		t.Fatalf("Decode refused a plan.json an older version could write: %v", err)
	}
	if p.Items[3].Summary != "webhook config: to=the on-call channel" {
		t.Errorf("the text was changed on reading: %q", p.Items[3].Summary)
	}
	if out := review.RenderPlan(p); bytes.Contains(out, []byte("to=the on-call")) {
		t.Errorf("the review files clean the text all the same:\n%s", out)
	}
}

// Second review, F9: Clean cut the tail off about 1 UUID in 2,000.
func TestUUIDsSurviveTheReviewFiles(t *testing.T) {
	uuid := "43a29c5c-3dbf-4fa4-8123-123456789012"
	p := samplePlan()
	p.Items[0].Summary = "Map team Platform to JSM team " + uuid
	data, _ := review.Encode(p)
	if !bytes.Contains(data, []byte(uuid)) {
		t.Errorf("plan.json lost the UUID")
	}
	r := sampleRun()
	r.Entries[0].RequestID = uuid
	run, _ := review.EncodeRun(r)
	if !bytes.Contains(run, []byte(uuid)) || !bytes.Contains(review.RenderRun(r), []byte(uuid)) {
		t.Errorf("the run record lost the request ID")
	}
}

// Second review, F10 and F11: errors that carry what the input holds.
func TestErrorsStayPrintable(t *testing.T) {
	_, err := review.ParseAcceptance([]byte("plan_version: 2\nplan_hash: x\n\"\\e]0;owned\\a\\e[2J\": 1\n"))
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\a\n") {
		t.Errorf("a YAML error reached the caller with control characters: %q", err)
	}
	p := samplePlan()
	p.Items = append(p.Items, review.Item{Key: "users/100%d", Action: "delete"})
	err = p.Check()
	if err == nil || strings.Contains(err.Error(), "%!") || !strings.Contains(err.Error(), `item "users/100%d": unknown action "delete"`) {
		t.Errorf("a %% in a key garbled the error: %v", err)
	}
}

func TestReasonsMustBeVisible(t *testing.T) {
	p := samplePlan()
	data, _ := review.Encode(p)
	hash := review.Hash(data)
	for _, blank := range []rune{0x3164, 0x2800, 0x115F, 0xFE0F, 0x200B} {
		a := review.Acceptance{PlanVersion: 2, PlanHash: hash, Warnings: []review.Accepted{
			{ID: "teams/PT1/member_dropped", Reason: string(blank)}, {ID: "users/PU3/stakeholder", Reason: "b"},
		}}
		if _, err := a.Accept(p, hash); err == nil || !strings.Contains(err.Error(), "without a reason") {
			t.Errorf("a reason of U+%04X: %v", blank, err)
		}
	}
}

func TestMarkdownEntitiesAndStrikethroughAreEscaped(t *testing.T) {
	p := samplePlan()
	p.Items[0].Summary = "Team &#x202e; ~~gone~~"
	out := string(review.RenderPlan(p))
	if !strings.Contains(out, `Team \&#x202e; \~\~gone\~\~`) {
		t.Errorf("plan.md:\n%s", out)
	}
}

func TestWriteRunRemovesTempFiles(t *testing.T) {
	ws := newWorkspace(t)
	r := sampleRun()
	stale := filepath.Join(review.RunDir(r.ID), ".run.md.tmp-0123abcd")
	if err := ws.WriteArtifact(stale, []byte("half")); err != nil {
		t.Fatal(err)
	}
	if _, err := review.WriteRun(ws, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.ArtifactsDir(), stale)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary file of a crashed write is still there: %v", err)
	}
}
