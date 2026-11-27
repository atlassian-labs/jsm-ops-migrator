package review

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// UserMapping is one proposed identity decision, before approval or a write.
type UserMapping struct {
	Email      string `json:"email"`
	Role       string `json:"pd_role"`
	AccountID  string `json:"account_id,omitempty"`
	SiteAccess string `json:"jsm_access,omitempty"`
	Decision   string `json:"decision"`
}

// MappingDecision records what the admin approved for this source user. An
// account ID is kept only for accepted mappings. The plan holds the evidence.
type MappingDecision struct {
	PagerDutyID string `json:"pd_user_id"`
	Decision    string `json:"review_decision"`
	AccountID   string `json:"account_id,omitempty"`
}

var mappingColumns = []string{"pd_user_id", "pd_email", "pd_role", "proposed_account_id", "target_jsm_access", "proposed_action", "reason", "plan_version", "plan_hash", "review_decision"}

func (m UserMapping) canAccept() bool {
	return m.AccountID != "" && (m.Decision == "map_existing" || m.Decision == "propose_access")
}

func (m UserMapping) decisionChoices() string {
	if m.canAccept() {
		return "accept / defer / skip"
	}
	return "defer / skip"
}

func mappingRow(it Item, version int, hash string) []string {
	m := it.UserMapping
	row := []string{it.PagerDuty.ID, m.Email, m.Role, m.AccountID, m.SiteAccess, m.Decision, messages.Clean(it.Summary), strconv.Itoa(version), hash}
	for i, value := range row {
		row[i] = csvCell(value)
	}
	return row
}

func proposedMappings(p Plan, hash string) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if err := w.Write(mappingColumns); err != nil {
		return nil, err
	}
	for _, it := range p.Items {
		if it.UserMapping == nil {
			continue
		}
		prompt := "Replace with " + it.UserMapping.decisionChoices()
		if err := w.Write(append(mappingRow(it, p.Version, hash), prompt)); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

// A supported header fits on one line. Parse it separately to select the
// spreadsheet's delimiter without guessing from any user-controlled data cell.
func mappingReader(r io.Reader) (*csv.Reader, error) {
	input := bufio.NewReader(r)
	line, err := input.ReadSlice('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading mapping CSV header: %w", err)
	}
	line = bytes.TrimPrefix(line, []byte("\uFEFF"))
	for _, sep := range []rune{',', ';'} {
		headerReader := csv.NewReader(bytes.NewReader(line))
		headerReader.Comma = sep
		header, err := headerReader.Read()
		if err != nil {
			continue
		}
		if slices.Equal(header, mappingColumns[:7]) {
			return nil, errors.New("older seven-column mapping CSV: run plan again and review the new export")
		}
		if slices.Equal(header, mappingColumns) {
			reader := csv.NewReader(input)
			reader.Comma, reader.FieldsPerRecord = sep, -1
			return reader, nil
		}
	}
	return nil, errors.New("invalid mapping CSV header: use the columns exported by this plan with comma or semicolon separators")
}

// ImportMappings checks immutable cells against the sealed plan, independent
// of CSV row order. Never echo cell contents in a validation error.
func ImportMappings(r io.Reader, p Plan, hash string) ([]MappingDecision, error) {
	reader, err := mappingReader(r)
	if err != nil {
		return nil, err
	}
	var decisions []MappingDecision
	type source struct {
		item  Item
		index int
	}
	rows := make(map[string]source)
	for _, it := range p.Items {
		if it.UserMapping == nil {
			continue
		}
		id := it.PagerDuty.ID
		key := csvCell(id)
		if _, exists := rows[key]; exists {
			return nil, errors.New("source IDs cannot be distinguished in the CSV")
		}
		rows[key] = source{it, len(decisions)}
		decisions = append(decisions, MappingDecision{PagerDutyID: id})
	}
	if len(rows) == 0 {
		return nil, errors.New("this plan has no user mappings")
	}
	for n := 2; ; n++ {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid mapping CSV at record %d", n)
		}
		if !slices.ContainsFunc(row, func(cell string) bool { return cell != "" }) {
			continue
		}
		if len(row) != len(mappingColumns) {
			return nil, fmt.Errorf("record %d needs %d columns", n, len(mappingColumns))
		}
		s, ok := rows[row[0]]
		if !ok {
			return nil, fmt.Errorf("unknown or duplicate PagerDuty user at record %d", n)
		}
		if !slices.Equal(row[:len(row)-1], mappingRow(s.item, p.Version, hash)) {
			return nil, fmt.Errorf("record %d differs from plan v%d; only review_decision may be edited", n, p.Version)
		}
		d := &decisions[s.index]
		d.Decision = strings.ToLower(strings.TrimSpace(row[len(row)-1]))
		switch d.Decision {
		case "accept":
			m := s.item.UserMapping
			if !m.canAccept() {
				return nil, fmt.Errorf("record %d has no eligible proposed mapping to accept; set review_decision to defer or skip", n)
			}
			d.AccountID = s.item.UserMapping.AccountID
		case "skip", "defer":
		default:
			return nil, fmt.Errorf("record %d: set review_decision to one of %s", n, s.item.UserMapping.decisionChoices())
		}
		delete(rows, row[0])
	}
	if len(rows) != 0 {
		return nil, fmt.Errorf("mapping CSV omits %d planned users", len(rows))
	}
	return decisions, nil
}
