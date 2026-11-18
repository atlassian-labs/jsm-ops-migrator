// Package run is the engine behind plan and apply: the step contract, the
// order of the calls, the plan versions, the confirmation, the write journal
// and the mapping from PagerDuty objects to JSM objects.
//
// The engine owns the calls, not the migration rules. Each entity, such as
// users or teams, is a Step: it reads its part of both sides, plans one item
// per call, and makes each call when apply reaches it. The engine reads the
// sides, orders the items by what they depend on, keeps every plan version
// and every result, and never skips or changes a call on its own.
package run
