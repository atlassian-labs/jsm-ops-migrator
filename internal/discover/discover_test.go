package discover_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

var now = func() time.Time { return time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC) }

var pdAccount = review.MigrationAccount{ID: "acme.pagerduty.com"}
var jsmAccount = review.MigrationAccount{ID: "cloud-1", OrgID: "org-1"}

// reader runs fn as its Read.
type reader struct {
	name string
	fn   func(ctx context.Context, w *discover.Writer) error
}

func (r reader) Name() string { return r.name }
func (r reader) Read(ctx context.Context, _ review.Side, w *discover.Writer) error {
	return r.fn(ctx, w)
}

func TestPutRefusesWhatCannotBeKept(t *testing.T) {
	cases := map[string]struct {
		kind, id string
		v        any
		want     string
	}{
		"a kind that is not snake_case": {"Team", "PT1", map[string]string{}, "snake_case"},
		"an empty ID":                   {"team", "", map[string]string{}, "printable"},
		"an ID with a space":            {"team", "PT 1", map[string]string{}, "printable"},
		"an ID with a control":          {"team", "PT\x1b1", map[string]string{}, "printable"},
		"a phone number as the ID":      {"contact", "+14155550123", map[string]string{}, "its ID holds sensitive data (phone_number)"},
		"a token in the payload":        {"team", "PT1", map[string]string{"routing_key": "R0123456789abcdef"}, "payload holds sensitive data"},
		"what JSON cannot hold":         {"team", "PT1", map[string]any{"f": func() {}}, "unsupported type"},
	}
	for name, c := range cases {
		st := testkit.NewStore(t)
		var got error
		r := reader{"teams", func(_ context.Context, w *discover.Writer) error {
			got = w.Put(c.kind, c.id, c.v)
			return nil // the step ignores the refusal
		}}
		s, err := discover.Read(context.Background(), st, review.PagerDuty, pdAccount, []discover.Reader{r}, now)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || !strings.Contains(got.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, got, c.want)
			continue
		}
		if strings.Contains(got.Error(), "4155550123") || strings.Contains(got.Error(), "R0123456789") {
			t.Errorf("%s: the refusal shows the value: %v", name, got)
		}
		// A step that goes on after a refusal still has an incomplete read.
		if s.Complete() || len(s.Problems()) != 1 {
			t.Errorf("%s: complete %v, problems %+v", name, s.Complete(), s.Problems())
		}
	}
}

func TestPutRefusesAnObjectTwice(t *testing.T) {
	st := testkit.NewStore(t)
	r := reader{"teams", func(_ context.Context, w *discover.Writer) error {
		if err := w.Put("team", "PT1", map[string]string{"name": "Platform"}); err != nil {
			return err
		}
		return w.Put("team", "PT1", map[string]string{"name": "Payments"})
	}}
	s, err := discover.Read(context.Background(), st, review.PagerDuty, pdAccount, []discover.Reader{r}, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Complete() || !strings.Contains(s.Problems()[0].Text, "written twice") {
		t.Errorf("problems %+v", s.Problems())
	}
}

// A read that is cut short never finishes, so no plan uses it: the latest
// read stays the one before.
func TestAReadCutShortIsNeverUsed(t *testing.T) {
	st := testkit.NewStore(t)
	ctx := context.Background()
	ok := reader{"teams", func(_ context.Context, w *discover.Writer) error { return w.Put("team", "PT1", map[string]string{}) }}
	first, err := discover.Read(ctx, st, review.JSM, jsmAccount, []discover.Reader{ok}, now)
	if err != nil {
		t.Fatal(err)
	}
	cut, cancel := context.WithCancel(ctx)
	stopped := reader{"teams", func(context.Context, *discover.Writer) error { cancel(); return cut.Err() }}
	if _, err := discover.Read(cut, st, review.JSM, jsmAccount, []discover.Reader{stopped}, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled read: %v", err)
	}
	latest, err := discover.Latest(ctx, st, review.JSM, []discover.Reader{ok})
	if err != nil || latest == nil || latest.ID() != first.ID() {
		t.Fatalf("Latest = %v, %v; want the read before", latest, err)
	}
	if _, err := discover.Open(ctx, st, first.ID()+1); !errors.Is(err, discover.ErrNoRead) {
		t.Errorf("opening the unfinished read: %v", err)
	}
	if none, err := discover.Latest(ctx, st, review.PagerDuty, []discover.Reader{ok}); none != nil || err != nil {
		t.Errorf("a side never read: %v, %v", none, err)
	}
}

func TestReadRefusesAStepNameThatIsNotSnakeCase(t *testing.T) {
	st := testkit.NewStore(t)
	r := reader{"Teams", func(context.Context, *discover.Writer) error { return nil }}
	if _, err := discover.Read(context.Background(), st, review.JSM, jsmAccount, []discover.Reader{r}, now); err == nil {
		t.Error("a step named Teams was accepted")
	}
}

// Second review: a step that went on after a refusal and then answered with
// a 402 got information, and a complete read without the refused object. A
// refusal is a blocker whatever the step returns after it.
func TestARefusalIsABlockerWhateverTheStepReturns(t *testing.T) {
	st := testkit.NewStore(t)
	r := reader{"users", func(_ context.Context, w *discover.Writer) error {
		_ = w.Put("user", "PU1", map[string]string{"name": "Avery"})
		_ = w.Put("user", "PU2", map[string]any{"contact": map[string]string{"address": "+14155550123"}})
		return fmt.Errorf("GET /extensions: 402: %w", httperr.ErrNotInPlan)
	}}
	s, err := discover.Read(context.Background(), st, review.PagerDuty, pdAccount, []discover.Reader{r}, now)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Problems()
	if s.Complete() || len(p) != 1 || p[0].Class != messages.Blocker ||
		!strings.Contains(p[0].Text, "holds sensitive data") || !strings.Contains(p[0].Text, "402") {
		t.Errorf("complete %v, problems %+v", s.Complete(), p)
	}
}

// A step's error can quote what an API was sent, so a read's problems are
// cleaned before they are sealed, as the review files show them.
func TestReadProblemsAreCleaned(t *testing.T) {
	st := testkit.NewStore(t)
	r := reader{"users", func(context.Context, *discover.Writer) error {
		return fmt.Errorf("GET /users?query=+14155550123: 400: %w", httperr.ErrInvalid)
	}}
	s, err := discover.Read(context.Background(), st, review.PagerDuty, pdAccount, []discover.Reader{r}, now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := discover.Open(context.Background(), st, s.ID())
	if err != nil || strings.Contains(again.Problems()[0].Text, "4155550123") {
		t.Errorf("the sealed problem keeps the phone number: %v %+v", err, again.Problems())
	}
}

// versioned is a reader whose part of a read is in format v.
type versioned struct {
	reader
	v int
}

func (r versioned) ReadVersion() int { return r.v }

// Third review: a read was reused by side alone, so a build with a new step
// took a read made without it as one with none of that step's objects. A
// read is reused only by the readers that made it, in the same versions, in
// any order; one made before reads recorded their readers never is.
func TestAReadIsReusedOnlyByItsReaders(t *testing.T) {
	st := testkit.NewStore(t)
	ctx := context.Background()
	nothing := func(context.Context, *discover.Writer) error { return nil }
	teams, users := reader{"teams", nothing}, reader{"users", nothing}
	first, err := discover.Read(ctx, st, review.JSM, jsmAccount, []discover.Reader{teams}, now)
	if err != nil {
		t.Fatal(err)
	}
	both, err := discover.Read(ctx, st, review.JSM, jsmAccount, []discover.Reader{users, teams}, now)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		readers []discover.Reader
		want    int64
	}{
		"the same readers in another order": {[]discover.Reader{teams, users}, both.ID()},
		"the readers of the first read":     {[]discover.Reader{teams}, first.ID()},
		"a reader that never read":          {[]discover.Reader{users}, 0},
		"a newer version of a reader":       {[]discover.Reader{versioned{teams, 1}}, 0},
	} {
		for _, latest := range []func(context.Context, *store.Store, review.Side, []discover.Reader) (*discover.Snapshot, error){
			discover.Latest, discover.LatestComplete,
		} {
			s, err := latest(ctx, st, review.JSM, c.readers)
			var got int64
			if s != nil {
				got = s.ID()
			}
			if err != nil || got != c.want {
				t.Errorf("%s: read %d, %v; want %d", name, got, err, c.want)
			}
		}
	}
	if got := discover.Coverage([]discover.Reader{users, versioned{teams, 2}}); got != "teams@2,users@0" {
		t.Errorf("Coverage = %q", got)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE snapshot_reads SET readers = NULL`); err != nil {
		t.Fatal(err)
	}
	if s, err := discover.Latest(ctx, st, review.JSM, []discover.Reader{teams}); s != nil || err != nil {
		t.Errorf("a read without its readers was reused: %v, %v", s, err)
	}
}
