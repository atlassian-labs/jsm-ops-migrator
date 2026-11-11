package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"net/url"
	"strconv"
	"strings"
)

// ErrWindow ends a list that is longer than the API lets an offset reach.
// PagerDuty's classic endpoints stop at offset + limit = 10,000; a caller that
// gets ErrWindow has every page before it and splits the list with filters.
var ErrWindow = errors.New("the list is longer than the API's paging window")

var (
	errEmptyPage  = errors.New("an empty page says that more pages follow")
	errSameCursor = errors.New("the answer gives the same cursor again")
)

// Page is one page of a list.
type Page[T any] struct {
	Items []T
	// Total is how many items the API says the whole list holds, or -1 when
	// it does not say.
	Total int
}

// Style is how an API pages a list: how the first request asks for a page,
// and how an answer gives its items and the request for the next page. The
// constructors below are the five styles the CLI meets. A style never takes a
// host or a path from an answer, so a list cannot lead a request elsewhere.
type Style[T any] struct {
	kind   styleKind
	key    string
	size   int
	window int
}

type styleKind int

const (
	styleOffset styleKind = iota + 1
	styleOffsetSize
	styleCursor
	stylePageInfo
	styleLink
)

// Offset is PagerDuty's paging: limit and offset parameters, a "more" flag
// and the items under key, with total=true so that Page.Total is known.
// window is the most that offset + limit may reach, 10,000 on the classic
// endpoints, or 0 for none.
func Offset[T any](key string, limit, window int) Style[T] {
	return Style[T]{kind: styleOffset, key: key, size: limit, window: window}
}

// OffsetSize is JSM Operations paging: offset and size parameters that the
// CLI counts itself, and the items under key. links.next only says that
// another page exists; it is a relative path without the cloudId, so it is
// never followed.
func OffsetSize[T any](key string, size int) Style[T] {
	return Style[T]{kind: styleOffsetSize, key: key, size: size}
}

// Cursor is the Teams REST API's team list paging: the answer's cursor goes
// back as the cursor parameter until it is null.
func Cursor[T any](key string) Style[T] {
	return Style[T]{kind: styleCursor, key: key}
}

// PageInfo is the Teams REST API's member list paging: a POST whose JSON body
// is {"first": first, "after": <cursor>}, answered with pageInfo.hasNextPage
// and pageInfo.endCursor. The style owns the method and the body, and marks
// the request idempotent, because it only reads.
func PageInfo[T any](key string, first int) Style[T] {
	return Style[T]{kind: stylePageInfo, key: key, size: first}
}

// Link is the Admin API's paging. links.next gives the next page's cursor,
// as the value itself or as the cursor parameter of a link; only the cursor
// is used.
func Link[T any](key string) Style[T] {
	return Style[T]{kind: styleLink, key: key}
}

func (s Style[T]) first(req Request) Request {
	switch s.kind {
	case styleOffset:
		req = withQuery(req, "limit", strconv.Itoa(s.size), "total", "true")
		if req.Query.Get("offset") == "" {
			req = withQuery(req, "offset", "0")
		}
	case styleOffsetSize:
		req = withQuery(req, "size", strconv.Itoa(s.size))
		if req.Query.Get("offset") == "" {
			req = withQuery(req, "offset", "0")
		}
	case stylePageInfo:
		req.Method = MethodPost
		req.Idempotent = true
		req.Body = map[string]any{"first": s.size}
	}
	return req
}

// next reads one answer: its page, and the request for the next page or nil
// after the last one.
func (s Style[T]) next(req Request, resp *Response) (Page[T], *Request, error) {
	page := Page[T]{Total: -1}
	var body map[string]json.RawMessage
	if err := resp.Decode(&body); err != nil {
		return page, nil, err
	}
	raw, ok := body[s.key]
	if !ok {
		return page, nil, resp.bad(fmt.Errorf("the answer has no %q list", s.key))
	}
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &page.Items); err != nil {
			return page, nil, resp.bad(decodeCause(err))
		}
	}
	var next *Request
	var err error
	switch s.kind {
	case styleOffset:
		next, err = s.nextOffset(req, resp, &page)
	case styleOffsetSize:
		next, err = nextOffsetSize(req, resp, len(page.Items))
	case styleCursor:
		next, err = nextCursor(req, resp)
	case stylePageInfo:
		next, err = s.nextPageInfo(req, resp)
	case styleLink:
		next, err = nextLink(req, resp)
	default:
		err = errors.New("a paging Style must come from one of its constructors")
	}
	return page, next, err
}

func (s Style[T]) nextOffset(req Request, resp *Response, page *Page[T]) (*Request, error) {
	var p struct {
		More  bool `json:"more"`
		Limit int  `json:"limit"`
		Total *int `json:"total"`
	}
	if err := resp.Decode(&p); err != nil {
		return nil, err
	}
	if p.Total != nil {
		page.Total = *p.Total
	}
	if !p.More {
		return nil, nil
	}
	if len(page.Items) == 0 {
		return nil, resp.bad(errEmptyPage)
	}
	// The API may give fewer items than asked for; the limit it reports is
	// the one it applied.
	limit := p.Limit
	if limit <= 0 {
		limit = s.size
	}
	offset, _ := strconv.Atoi(req.Query.Get("offset"))
	offset += limit
	if s.window > 0 && offset+limit > s.window {
		return nil, ErrWindow
	}
	r := withQuery(req, "offset", strconv.Itoa(offset), "limit", strconv.Itoa(limit))
	return &r, nil
}

func nextOffsetSize(req Request, resp *Response, n int) (*Request, error) {
	var p struct {
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	}
	if err := resp.Decode(&p); err != nil {
		return nil, err
	}
	if p.Links.Next == "" {
		return nil, nil
	}
	if n == 0 {
		return nil, resp.bad(errEmptyPage)
	}
	offset, _ := strconv.Atoi(req.Query.Get("offset"))
	r := withQuery(req, "offset", strconv.Itoa(offset+n))
	return &r, nil
}

func nextCursor(req Request, resp *Response) (*Request, error) {
	var p struct {
		Cursor *string `json:"cursor"`
	}
	if err := resp.Decode(&p); err != nil {
		return nil, err
	}
	if p.Cursor == nil || *p.Cursor == "" {
		return nil, nil
	}
	if *p.Cursor == req.Query.Get("cursor") {
		return nil, resp.bad(errSameCursor)
	}
	r := withQuery(req, "cursor", *p.Cursor)
	return &r, nil
}

func (s Style[T]) nextPageInfo(req Request, resp *Response) (*Request, error) {
	var p struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
	}
	if err := resp.Decode(&p); err != nil {
		return nil, err
	}
	if !p.PageInfo.HasNextPage {
		return nil, nil
	}
	prev, _ := req.Body.(map[string]any)
	if p.PageInfo.EndCursor == "" || prev["after"] == p.PageInfo.EndCursor {
		return nil, resp.bad(errSameCursor)
	}
	r := req
	r.Body = map[string]any{"first": s.size, "after": p.PageInfo.EndCursor}
	return &r, nil
}

func nextLink(req Request, resp *Response) (*Request, error) {
	var p struct {
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	}
	if err := resp.Decode(&p); err != nil {
		return nil, err
	}
	cursor := p.Links.Next
	if cursor == "" {
		return nil, nil
	}
	if strings.ContainsAny(cursor, "/?") {
		u, err := url.Parse(cursor)
		if err != nil {
			// url.Parse quotes its input, and the error must not quote the
			// answer.
			return nil, resp.bad(errors.New("links.next is not a link"))
		}
		if cursor = u.Query().Get("cursor"); cursor == "" {
			return nil, resp.bad(errors.New("links.next has no cursor parameter"))
		}
	}
	if cursor == req.Query.Get("cursor") {
		return nil, resp.bad(errSameCursor)
	}
	r := withQuery(req, "cursor", cursor)
	return &r, nil
}

// withQuery returns req with the query parameters set in pairs, leaving the
// caller's query untouched.
func withQuery(req Request, pairs ...string) Request {
	q := maps.Clone(req.Query)
	if q == nil {
		q = url.Values{}
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		q.Set(pairs[i], pairs[i+1])
	}
	req.Query = q
	return req
}

// Pages sends first, then the request for each following page, and yields
// every page. An error is yielded once and ends the list. The pages before it
// stay valid, so a caller can tell a partial read from a complete one; at
// ErrWindow, the page that reached the window comes first.
func Pages[T any](ctx context.Context, c *Client, first Request, style Style[T]) iter.Seq2[Page[T], error] {
	return func(yield func(Page[T], error) bool) {
		req := style.first(first)
		for {
			resp, err := c.Do(ctx, req)
			if err != nil {
				yield(Page[T]{Total: -1}, err)
				return
			}
			page, next, err := style.next(req, resp)
			if errors.Is(err, ErrWindow) {
				if yield(page, nil) {
					yield(Page[T]{Total: page.Total}, err)
				}
				return
			}
			if err != nil {
				yield(Page[T]{Total: -1}, err)
				return
			}
			if !yield(page, nil) || next == nil {
				return
			}
			req = *next
		}
	}
}
