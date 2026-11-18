// Package discover reads the PagerDuty and JSM snapshots that plan builds
// from, and keeps them sealed in state.db with how complete each read was.
// It owns how a side is read and kept; each step owns what it reads.
package discover
