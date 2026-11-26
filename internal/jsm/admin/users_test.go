package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

const searchPath = "/admin/v2/orgs/org-1/directories/-/users/search"

func searchPage(next string, rows ...map[string]string) map[string]any {
	if rows == nil {
		rows = []map[string]string{}
	}
	return map[string]any{"data": rows, "links": map[string]string{"next": next}}
}

func TestFindUsersBatchesAndFollowsPages(t *testing.T) {
	f, c := fake(t)
	emails := make([]credentials.Sensitive, 101)
	for i := range emails {
		emails[i] = credentials.NewSensitive(fmt.Sprintf("person%d@example.com", i))
	}
	emails = append(emails, emails[0])
	active := map[string]string{"email": "Person0@example.com", "accountId": "a", "accountStatus": "active"}
	suspended := map[string]string{"email": "person99@example.com", "accountId": "b", "accountStatus": "inactive"}
	conflict := map[string]string{"email": "person1@example.com", "accountId": "c", "accountStatus": "active"}
	f.Fail(http.MethodPost, searchPath,
		jsmfake.Fault{Status: 200, Body: searchPage("cursor-2", active, conflict)},
		jsmfake.Fault{Status: 200, Body: searchPage("", active, suspended,
			map[string]string{"email": "person1@example.com", "accountId": "d", "accountStatus": "active"})})
	f.Answer(http.MethodPost, searchPath, 200, searchPage(""))
	got, err := c.FindUsers(context.Background(), "org-1", emails)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(emails) || !got[0].Active() || got[0].AccountID != "a" || got[101] != got[0] {
		t.Fatalf("input order or repeated email lost: %+v", got)
	}
	if !got[1].Ambiguous || got[1].Active() || got[1].AccountID != "" || got[99].Active() || got[99].AccountID != "b" || got[100] != (admin.UserMatch{}) {
		t.Fatalf("conflict, inactive or missing account: %+v", got)
	}
	reqs := f.Requests()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests", len(reqs))
	}
	for i, r := range reqs {
		var body struct {
			Emails []string
			Limit  int
			Cursor string
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		want := emails[:100]
		if i == 2 {
			want = emails[100:101]
		}
		var addresses []string
		for _, email := range want {
			addresses = append(addresses, email.Value())
		}
		if body.Limit != 100 || !slices.Equal(body.Emails, addresses) || (i == 1 && body.Cursor != "cursor-2") || (i != 1 && body.Cursor != "") {
			t.Fatalf("unexpected batch or cursor in request %d", i)
		}
	}
}

func TestIncompleteSearchCannotReportMissingUsers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   any
		want   error
	}{
		{"read failed", http.StatusForbidden, nil, httperr.ErrForbidden},
		{"missing data", 200, map[string]any{}, httperr.ErrBadResponse},
		{"missing ID", 200, searchPage("", map[string]string{"email": "person@example.com"}), httperr.ErrBadResponse},
		{"cursor repeated", 200, searchPage("again"), httperr.ErrBadResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := fake(t)
			f.Fail(http.MethodPost, searchPath, jsmfake.Fault{Status: 200, Body: searchPage("again", map[string]string{"email": "person@example.com", "accountId": "a", "accountStatus": "active"})})
			f.Answer(http.MethodPost, searchPath, tc.status, tc.body)
			got, err := c.FindUsers(context.Background(), "org-1", []credentials.Sensitive{credentials.NewSensitive("person@example.com")})
			if got != nil || !errors.Is(err, tc.want) || strings.Contains(err.Error(), "person@") {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}
