// Package pd is the PagerDuty client: the general part of every PagerDuty
// read, on top of the HTTP layer. It sends only GET requests, sets the Accept
// header each API version expects, and pages lists by offset up to
// PagerDuty's 10,000 window, saying when a list goes past it. The packages
// that read PagerDuty data define the paths they read and the types they
// decode into; pd knows no endpoint except /abilities.
//
// The HTTP layer attaches the key, sends each request to the account's
// region, retries, limits the rate and removes calendar feed URLs from every
// answer before it is decoded.
package pd
