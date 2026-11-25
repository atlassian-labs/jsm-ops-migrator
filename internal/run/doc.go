// Package run is the engine behind plan, apply, pause and resume: the step
// contract, the order of the calls, the plan versions, the confirmation, the
// write journal and the mapping from PagerDuty objects to JSM objects.
//
// The engine owns the calls, not the migration rules. Each entity, such as
// users or teams, is a Step: it reads its part of both sides, plans one item
// per call, and makes each call when apply reaches it. The engine reads the
// sides, orders the items by what they depend on, keeps every plan version
// and every result, and never skips or changes a call on its own.
//
// A run stops between two items when the operator asks it to, and resume
// continues it from the journal. Journaled creates are lookup-only. Their
// dependents wait for reconciliation while independent items continue. An
// operator can skip several verified-absent creates in one resume. Updates
// can still be retried after a lookup miss once the attempt is ten minutes
// old, up to three attempts.
package run
