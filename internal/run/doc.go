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
// continues it from the journal. A call that may have reached JSM without
// its answer being recorded is looked up before anything else, and sent
// again only when its step proves it did not take effect, so a crash at any
// point makes no object twice. A lookup that finds nothing is believed only
// once the call is ten minutes old, because a lookup sooner can miss what
// the call made, and a call is sent at most three times in all, whatever
// number of resumes there are.
package run
