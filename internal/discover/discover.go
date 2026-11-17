package discover

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// Reader is a step's part of a read.
type Reader interface {
	// Name is the step's name. The objects the step writes are kept under it.
	Name() string
	// Read reads the step's part of side into w. It returns an error when it
	// could not read all of it, such as a list PagerDuty answered only in
	// part (pd.Coverage), so an incomplete list never looks complete.
	Read(ctx context.Context, side review.Side, w *Writer) error
}

// nameRe is a step's name and an object's kind: they become part of message
// codes and item keys, which are snake_case.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidName reports whether s can be a step's name or an object's kind.
func ValidName(s string) bool { return nameRe.MatchString(s) }

type objectKey struct{ kind, id string }

// Writer collects what one step reads from one side. Nothing is saved until
// the step's Read returns.
type Writer struct {
	step    string
	objects []object
	seen    map[objectKey]bool
	refused error
}

type object struct {
	kind, id string
	payload  []byte
}

func newWriter(step string) *Writer { return &Writer{step: step, seen: map[objectKey]bool{}} }

// Put adds one object to the snapshot: kind says what it is, such as "user"
// or "team", and id is its ID on the side being read. v is kept as JSON.
//
// An object whose ID or JSON holds a secret, contact data or a calendar URL
// is refused, never kept: sealed or not, the workspace never stores them. A
// refusal names the rules and the field, never the value, and makes the
// step's read incomplete even if the step goes on.
func (w *Writer) Put(kind, id string, v any) error {
	err := w.put(kind, id, v)
	if err != nil && w.refused == nil {
		w.refused = err
	}
	return err
}

func (w *Writer) put(kind, id string, v any) error {
	if !ValidName(kind) {
		return fmt.Errorf("snapshot object of kind %s: the kind must be snake_case", review.Quote(kind))
	}
	name := kind + " " + review.Quote(id)
	switch {
	case id == "" || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }):
		return fmt.Errorf("snapshot object %s: the ID must be printable, without spaces", name)
	case len(review.ScreenID(id)) > 0:
		return fmt.Errorf("snapshot object %s: its ID holds sensitive data (%s)", name, strings.Join(review.ScreenID(id), ", "))
	case w.seen[objectKey{kind, id}]:
		return fmt.Errorf("snapshot object %s: written twice", name)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("snapshot object %s: %w", name, err)
	}
	if err := review.ScreenPayload(data); err != nil {
		return fmt.Errorf("snapshot object %s: %w", name, err)
	}
	w.seen[objectKey{kind, id}] = true
	w.objects = append(w.objects, object{kind: kind, id: id, payload: data})
	return nil
}

// Snapshot is one finished read of one side.
type Snapshot struct {
	account  review.MigrationAccount
	st       *store.Store
	id       int64
	side     review.Side
	at       time.Time
	complete bool
	problems []messages.Message
}

// ID is the read's row in state.db.
func (s *Snapshot) ID() int64 { return s.id }

// Side is the side that was read.
func (s *Snapshot) Side() review.Side { return s.side }

// At is when the read started.
func (s *Snapshot) At() time.Time { return s.at }

// Complete reports whether every step read all of its part.
func (s *Snapshot) Complete() bool { return s.complete }

// Problems are the messages of the steps whose read failed: a blocker for a
// step whose part is incomplete, and information for one whose part the
// account's plan does not include.
func (s *Snapshot) Problems() []messages.Message {
	return append([]messages.Message(nil), s.problems...)
}

// Read is the read as a plan records it.
func (s *Snapshot) Read() review.Read {
	return review.Read{MigrationAccount: s.account, Side: s.side, At: s.at, Complete: s.complete}
}

// Object is one object a step wrote.
type Object struct {
	ID      string
	Payload json.RawMessage
}

// Get decodes the object step wrote with kind and id into v. It reports
// false when there is none.
func (s *Snapshot) Get(ctx context.Context, step, kind, id string, v any) (bool, error) {
	var sealed []byte
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT payload FROM snapshot_objects WHERE read_id = ? AND step = ? AND kind = ? AND object_id = ?`,
		s.id, step, kind, id).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, err := s.st.Cipher().Open(objectRef(s.id, step, kind, id), sealed)
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

// List returns every object step wrote with kind, by ID.
func (s *Snapshot) List(ctx context.Context, step, kind string) ([]Object, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT object_id, payload FROM snapshot_objects WHERE read_id = ? AND step = ? AND kind = ? ORDER BY object_id`,
		s.id, step, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Object
	for rows.Next() {
		var id string
		var sealed []byte
		if err := rows.Scan(&id, &sealed); err != nil {
			return nil, err
		}
		data, err := s.st.Cipher().Open(objectRef(s.id, step, kind, id), sealed)
		if err != nil {
			return nil, err
		}
		out = append(out, Object{ID: id, Payload: data})
	}
	return out, rows.Err()
}

// objectRef is where an object's payload is sealed. The step and the kind
// are snake_case, so the ID, which can hold '/', comes last and the parts
// never run together.
func objectRef(read int64, step, kind, id string) store.FieldRef {
	return store.FieldRef{Table: "snapshot_objects", Item: fmt.Sprintf("%d/%s/%s/%s", read, step, kind, id), Column: "payload"}
}

func problemsRef(read int64) store.FieldRef {
	return store.FieldRef{Table: "snapshot_reads", Item: strconv.FormatInt(read, 10), Column: "problems"}
}

// Read reads side with every reader, in order, and saves what they wrote as
// one read. A reader that fails leaves the read incomplete, with a message
// for it, and the readers after it still read, so the plan shows every
// problem at once. What a failed reader wrote before it failed is kept: the
// plan shows it, and the blocker stops apply.
//
// Read returns an error only when ctx ends, when a reader's name is not
// snake_case, or when the read cannot be saved. A read cut short is never
// finished, so no plan uses it.
func Read(ctx context.Context, st *store.Store, side review.Side, account review.MigrationAccount, readers []Reader, now func() time.Time) (*Snapshot, error) {
	if side != review.PagerDuty && side != review.JSM {
		return nil, fmt.Errorf("discover: unknown side %s", review.Quote(string(side)))
	}
	for _, r := range readers {
		if !ValidName(r.Name()) {
			return nil, fmt.Errorf("discover: step name %s must be snake_case", review.Quote(r.Name()))
		}
	}
	if !account.Valid(side) {
		return nil, fmt.Errorf("cannot identify the %s migration account", side.Label())
	}
	if err := CheckAccount(ctx, st, side, account); err != nil {
		return nil, err
	}
	s := &Snapshot{st: st, side: side, account: account, at: now().UTC(), complete: true}
	res, err := st.DB().ExecContext(ctx, `INSERT INTO snapshot_reads(side, started_at) VALUES (?, ?)`,
		string(side), formatTime(s.at))
	if err != nil {
		return nil, err
	}
	if s.id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	identity, err := json.Marshal(account)
	if err != nil {
		return nil, err
	}
	sealedAccount, err := st.Cipher().Seal(accountRef(s.id), identity)
	if err != nil {
		return nil, err
	}
	if _, err = st.DB().ExecContext(ctx, `UPDATE snapshot_reads SET account = ? WHERE id = ?`, sealedAccount, s.id); err != nil {
		return nil, err
	}
	for _, r := range readers {
		w := newWriter(r.Name())
		readErr := r.Read(ctx, side, w)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.save(ctx, w); err != nil {
			return nil, err
		}
		if readErr != nil || w.refused != nil {
			m := readProblem(r.Name(), side, readErr, w.refused)
			s.problems = append(s.problems, m)
			s.complete = s.complete && m.Class != messages.Blocker
		}
	}
	data, err := json.Marshal(s.problems)
	if err != nil {
		return nil, err
	}
	sealed, err := st.Cipher().Seal(problemsRef(s.id), data)
	if err != nil {
		return nil, err
	}
	_, err = st.DB().ExecContext(ctx, `UPDATE snapshot_reads SET finished_at = ?, complete = ?, problems = ? WHERE id = ?`,
		formatTime(now()), s.complete, sealed, s.id)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// readProblem is the message for a step whose read failed or that wrote an
// object Put refused. It is a blocker, because a list that was not read in
// full must never look complete, except when the account's plan does not
// include what the step reads: then there is nothing to read, and the read
// is complete. A refusal is always a blocker, whatever the step returned
// after it: the object it refused is missing from the snapshot.
func readProblem(step string, side review.Side, readErr, refused error) messages.Message {
	m := messages.Message{Class: messages.Blocker, Code: step + "_" + string(side) + "_read"}
	err := readErr
	switch {
	case refused != nil && (readErr == nil || errors.Is(readErr, refused)):
		err = refused
	case refused != nil:
		err = fmt.Errorf("%w; the step then failed: %w", refused, readErr)
	default:
		m.Class = messages.Classify(readErr, messages.Read)
	}
	if m.Class == messages.Information {
		m.Text = fmt.Sprintf("The %s account's plan does not include what %s reads, so there was nothing to read: %v", side.Label(), step, err)
	} else {
		m.Text = fmt.Sprintf("The %s read of %s is incomplete: %v. Fix the cause, then run plan again.", side.Label(), step, err)
	}
	// Kept as the review files show it: an API's error can quote what it
	// was sent.
	return m.Redacted()
}

func (s *Snapshot) save(ctx context.Context, w *Writer) error {
	if len(w.objects) == 0 {
		return nil
	}
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO snapshot_objects(read_id, step, kind, object_id, payload) VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, o := range w.objects {
			sealed, err := s.st.Cipher().Seal(objectRef(s.id, w.step, o.kind, o.id), o.payload)
			if err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, s.id, w.step, o.kind, o.id, sealed); err != nil {
				return err
			}
		}
		return nil
	})
}

// Latest returns the latest finished read of side, or nil when there is
// none.
func Latest(ctx context.Context, st *store.Store, side review.Side) (*Snapshot, error) {
	return latestWhere(ctx, st, side, "")
}

func latestWhere(ctx context.Context, st *store.Store, side review.Side, and string) (*Snapshot, error) {
	var id int64
	err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM snapshot_reads WHERE side = ? AND account IS NOT NULL AND finished_at IS NOT NULL `+and+` ORDER BY id DESC LIMIT 1`,
		string(side)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Open(ctx, st, id)
}

// LatestComplete returns the latest finished read of side in which every
// step read all of its part, or nil when there is none.
func LatestComplete(ctx context.Context, st *store.Store, side review.Side) (*Snapshot, error) {
	return latestWhere(ctx, st, side, `AND complete = 1`)
}

// ErrNoRead means no finished read has the ID asked for.
var ErrNoRead = errors.New("discover: no such finished read")

// Open returns the finished read with the given ID.
func Open(ctx context.Context, st *store.Store, id int64) (*Snapshot, error) {
	s := &Snapshot{st: st, id: id}
	var side, at string
	var sealed, identity []byte
	err := st.DB().QueryRowContext(ctx,
		`SELECT side, started_at, complete, problems, account FROM snapshot_reads WHERE id = ? AND finished_at IS NOT NULL`,
		id).Scan(&side, &at, &s.complete, &sealed, &identity)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", ErrNoRead, id)
	}
	if err != nil {
		return nil, err
	}
	if identity != nil {
		if s.account, err = openAccount(st, id, identity); err != nil {
			return nil, err
		}
	}
	s.side = review.Side(side)
	if s.at, err = parseTime(at); err != nil {
		return nil, err
	}
	data, err := st.Cipher().Open(problemsRef(id), sealed)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.problems); err != nil {
		return nil, err
	}
	return s, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func accountRef(id int64) store.FieldRef {
	return store.FieldRef{Table: "snapshot_reads", Item: strconv.FormatInt(id, 10), Column: "account"}
}

func openAccount(st *store.Store, id int64, sealed []byte) (review.MigrationAccount, error) {
	var a review.MigrationAccount
	data, err := st.Cipher().Open(accountRef(id), sealed)
	if err != nil {
		return a, err
	}
	err = json.Unmarshal(data, &a)
	return a, err
}

// CheckAccount checks the first bound read, including one that stopped midway.
// A legacy mapping has no trustworthy source or destination to bind it to.
func CheckAccount(ctx context.Context, st *store.Store, side review.Side, account review.MigrationAccount) error {
	var id int64
	var sealed []byte
	err := st.DB().QueryRowContext(ctx, `SELECT id, account FROM snapshot_reads WHERE side = ? AND account IS NOT NULL ORDER BY id LIMIT 1`, side).Scan(&id, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		var mapped bool
		if err := st.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mappings)`).Scan(&mapped); err != nil {
			return err
		}
		if mapped {
			return fmt.Errorf("existing mappings predate account binding; use a separate --workspace")
		}
		return nil
	}
	if err != nil {
		return err
	}
	bound, err := openAccount(st, id, sealed)
	if err != nil {
		return err
	}
	if (account.ID != "" && account.ID != bound.ID) || (account.OrgID != "" && account.OrgID != bound.OrgID) {
		return messages.ValidationFailed(fmt.Errorf("this workspace is bound to a different %s account or organization; use a separate --workspace", side.Label()))
	}
	return nil
}
