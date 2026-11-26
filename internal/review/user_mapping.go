package review

import (
	"bytes"
	"encoding/csv"

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

func proposedMappings(p Plan) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if err := w.Write([]string{"pd_user_id", "pd_email", "pd_role", "proposed_account_id", "target_jsm_access", "proposed_action", "reason"}); err != nil {
		return nil, err
	}
	for _, it := range p.Items {
		m := it.UserMapping
		if m == nil {
			continue
		}
		row := []string{it.PagerDuty.ID, m.Email, m.Role, m.AccountID, m.SiteAccess, m.Decision, messages.Clean(it.Summary)}
		for i, value := range row {
			row[i] = csvCell(value)
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return b.Bytes(), w.Error()
}
