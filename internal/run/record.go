package run

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
)

// An unfinished verification is safe to repeat. Only a write can have an
// unknown side effect while its journal row is in flight.
func entryState(action review.Action, state review.EntryState) review.EntryState {
	if state != sending {
		return state
	}
	if action == review.Verify {
		return review.Pending
	}
	return review.Unknown
}

// record rebuilds the run record from the journal: one entry per plan item,
// in the plan's order, with the state of its latest row. A row still sending
// is unknown in the record, because the call may have reached JSM; an item
// without a row is pending.
func (e *Engine) record(ctx context.Context, info RunInfo, p review.Plan) (review.Run, error) {
	var hash string
	if err := e.Store.DB().QueryRowContext(ctx, `SELECT hash FROM plan_versions WHERE version = ?`,
		info.PlanVersion).Scan(&hash); err != nil {
		return review.Run{}, err
	}
	j, err := e.readJournal(ctx, info.ID)
	if err != nil {
		return review.Run{}, err
	}
	r := review.Run{
		ID: info.ID, PlanVersion: info.PlanVersion, PlanHash: hash, State: info.State,
		StartedAt: info.StartedAt, EndedAt: info.EndedAt, Entries: make([]review.Entry, len(p.Items)),
	}
	for i, it := range p.Items {
		entry := review.Entry{Key: it.Key, Action: it.Action, State: review.Pending, PagerDuty: it.PagerDuty, JSM: it.JSM}
		if row, ok := j.latest[it.Key]; ok {
			entry.State, entry.Attempts = entryState(it.Action, row.state), j.attempts[it.Key]
			entry.Status, entry.RequestID, entry.Messages = row.status, row.requestID, row.msgs
			if row.jsm != nil {
				entry.JSM = row.jsm
			}
			entry.At = row.finished
			if entry.At.IsZero() {
				entry.At = row.started
			}
		}
		r.Entries[i] = entry
	}
	return r, nil
}

// readJournal reads a run's journal: each item's latest row, how many
// attempts each item's call had, and the highest sequence number.
func (e *Engine) readJournal(ctx context.Context, run string) (journal, error) {
	rows, err := e.Store.DB().QueryContext(ctx, `SELECT seq, item_key, attempt, state, started_at, finished_at,
		status, request_id, jsm_type, jsm_id, messages FROM journal WHERE run_id = ? ORDER BY seq`, run)
	if err != nil {
		return journal{}, err
	}
	defer func() { _ = rows.Close() }()
	j := journal{latest: map[string]journalRow{}, attempts: map[string]int{}}
	for rows.Next() {
		var row journalRow
		var state, started string
		var finished, requestID, jsmType, jsmID sql.NullString
		var status sql.NullInt64
		var sealed []byte
		if err := rows.Scan(&row.seq, &row.key, &row.attempt, &state, &started, &finished, &status, &requestID,
			&jsmType, &jsmID, &sealed); err != nil {
			return journal{}, err
		}
		row.state, row.status, row.requestID = review.EntryState(state), int(status.Int64), requestID.String
		if row.started, err = parseTime(started); err != nil {
			return journal{}, err
		}
		if finished.Valid {
			if row.finished, err = parseTime(finished.String); err != nil {
				return journal{}, err
			}
		}
		if jsmType.Valid && jsmID.Valid {
			row.jsm = &review.Ref{Type: jsmType.String, ID: jsmID.String}
		}
		if sealed != nil {
			data, err := e.Store.Cipher().Open(journalRef(run, row.seq), sealed)
			if err != nil {
				return journal{}, err
			}
			if err := json.Unmarshal(data, &row.msgs); err != nil {
				return journal{}, err
			}
		}
		if row.attempt > 0 && row.state != review.Declined {
			j.attempts[row.key]++
		}
		j.latest[row.key] = row
		j.seq = max(j.seq, row.seq)
	}
	return j, rows.Err()
}
