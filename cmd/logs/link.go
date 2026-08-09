package main

import (
	hippo "github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus-gen/internal/link"
)

// A log store's associations are the ones an operator reconstructs by hand at three in the morning:
// the other lines from the same request, and what else the service was complaining about at the
// time. Three threads carry them.
//
//   - The correlation token in a line's message threads a request together as it crosses services,
//     so a failure links back to the line before it on the same token - the API gateway's ERROR
//     pointing at the database's INFO that preceded it.
//   - Per service, one incident thread chains ERROR and FATAL lines to each other, which is the
//     "what else was on fire" view.
//   - Per service, the daily event links to the day before, so a service's history is one chain of
//     days rather than a scatter of unrelated events.
//
// Only WARN and above declare memory links, while every line advances the token thread. That
// asymmetry is the demonstration: the routine INFO line that happened to be part of a failing
// request gains inbound link significance and outlives its identical neighbours, while noise that
// was never implicated in anything stays unconnected and is consolidated away first.

// Link weights. A FATAL binds what it points at harder than a WARN does, so the traces through the
// worst failures are the best protected. The incident chain outweighs a single trace link: a run of
// errors from one service is a stronger signal than any one request.
const (
	warnLinkSignificance     = 8000
	errorLinkSignificance    = 20000
	fatalLinkSignificance    = 30000
	incidentLinkSignificance = 24000
	dayLinkSignificance      = 10000
)

// Thread lifetimes, measured in lines rather than in time: the generator emits under both a
// back-dated one-shot load and a live trickle, and a bound in lines behaves the same under each,
// where a bound in nanoseconds would mean something wildly different between them.
//
// A token is only a request while the request is still running, so its thread is short - long enough
// to cross a few services, not so long that every line finds a stale head from the fixed token pool.
// An incident runs longer: errors are rare, and two of them an hour apart are still the same bad
// afternoon.
const (
	traceTTL    = 25
	incidentTTL = 500
)

// maxThreads bounds each tracker. The token pool and the service list are both fixed, so this is a
// backstop rather than a working limit.
const maxThreads = 64

// threads are the trackers a run's associations hang off: request traces keyed by correlation token,
// incident chains and daily-event chains keyed by service. The daily chain never expires - yesterday
// is a service's predecessor however long the gap - so that tracker takes no ttl.
type threads struct {
	traces    *link.Threads
	incidents *link.Threads
	days      *link.Threads
}

func newThreads() *threads {
	return &threads{
		traces:    link.NewThreads(traceTTL, maxThreads),
		incidents: link.NewThreads(incidentTTL, maxThreads),
		days:      link.NewThreads(0, maxThreads),
	}
}

// memoryLinks resolves the links a new line declares: its request trace, and - for an ERROR or
// FATAL - its service's incident chain. A line below WARN declares none, however much it is part of
// a trace; it still advances the token thread, so the failure that follows can link back to it.
func (t *threads) memoryLinks(l line) []*hippo.Link {
	if l.level.rank < warnRank {

		return nil
	}

	links := make([]*hippo.Link, 0, 2)

	if head, ok := t.traces.Head(l.token, l.seq); ok {
		links = append(links, link.New(head, traceLinkSignificance(l.level)))
	}

	if l.level.rank >= errorRank {
		if head, ok := t.incidents.Head(l.service, l.seq); ok {
			links = append(links, link.New(head, incidentLinkSignificance))
		}
	}

	return links
}

// advanceMemory moves the threads a stored line is now the head of: its token always, and its
// service's incident chain when the line was an error. An empty id - a line the service did not
// retain, or one whose write failed - breaks those threads rather than letting the next line link
// across the gap to something that was never stored.
func (t *threads) advanceMemory(l line, id string) {
	t.traces.Advance(l.token, id, l.seq)

	if l.level.rank >= errorRank {
		t.incidents.Advance(l.service, id, l.seq)
	}
}

// eventLinks is the link a service's new daily event declares to its previous day's event, or nil
// for the first day of a service's history.
func (t *threads) eventLinks(service string, day int64) []*hippo.Link {
	head, ok := t.days.Head(service, day)

	if !ok {

		return nil
	}

	return []*hippo.Link{link.New(head, dayLinkSignificance)}
}

// advanceEvent moves a service's daily chain onto its newly created event.
func (t *threads) advanceEvent(service string, id string, day int64) {
	t.days.Advance(service, id, day)
}

// traceLinkSignificance weights a trace link by the severity of the line declaring it.
func traceLinkSignificance(lvl level) int32 {
	switch {

	case lvl.rank >= fatalRank:

		return fatalLinkSignificance

	case lvl.rank >= errorRank:

		return errorLinkSignificance

	default:

		return warnLinkSignificance

	}
}
