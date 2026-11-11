package testkit

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
)

// Router sends every request to one test server, whatever its host, and
// records where each one was meant to go. It sits below httpx's own checks,
// so tests exercise them with the real host names. The server sees the
// intended host in r.Host.
type Router struct {
	srv  *httptest.Server
	base *url.URL

	mu   sync.Mutex
	sent []Routed
	fail func(*http.Request) error
}

// Routed is one request the Router saw.
type Routed struct {
	Method string
	// URL is where the request was meant to go, before the Router changed
	// it.
	URL string
}

// RouteTo returns a Router to srv.
func RouteTo(srv *httptest.Server) *Router {
	u, err := url.Parse(srv.URL)
	if err != nil {
		panic(err)
	}
	return &Router{srv: srv, base: u}
}

// FailWith makes the Router fail each request for which fail returns an
// error, before anything is written, as a connection that cannot be opened
// does.
func (r *Router) FailWith(fail func(*http.Request) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = fail
}

// RoundTrip implements http.RoundTripper.
func (r *Router) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.sent = append(r.sent, Routed{Method: req.Method, URL: req.URL.String()})
	fail := r.fail
	r.mu.Unlock()
	if fail != nil {
		if err := fail(req); err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, err
		}
	}
	out := req.Clone(req.Context())
	out.Host = req.URL.Host
	out.URL.Scheme = r.base.Scheme
	out.URL.Host = r.base.Host
	return r.srv.Client().Transport.RoundTrip(out)
}

// Requests returns every request the Router has seen, in order.
func (r *Router) Requests() []Routed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Routed(nil), r.sent...)
}
