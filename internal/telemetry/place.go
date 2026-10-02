package telemetry

import (
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/timeplace"
)

// PlacePolicy are the placement bounds of one delivery (policy values).
type PlacePolicy struct {
	// AheadTolerance: a sample whose own time is ahead of its receipt
	// (or of the batch's sent_at) by more is placed at its receipt and
	// counted (T-13: 1 s).
	AheadTolerance time.Duration
	// BacklogAfter: a sample placed further behind its receipt is
	// history (T-04, T-11); it is also how old a sample may be and still
	// be placed at its own time when nothing else places it.
	BacklogAfter time.Duration
	// AnchorMaxAge: how long the clock relation of an aircraft's live
	// samples places its later samples (T-11) before it may be relearnt.
	AnchorMaxAge time.Duration
	// MaxAge: a sample older than this (behind its sent_at, or behind
	// its receipt) is not taken at all (rejected_too_old): older than
	// anything the work queue would still hold.
	MaxAge time.Duration
}

// network is the policy of uspace-core's network rule.
func (p PlacePolicy) network() timeplace.NetworkPolicy {
	return timeplace.NetworkPolicy{MaxAgeS: p.MaxAge.Seconds(), ToleranceS: p.AheadTolerance.Seconds(), MaxLatencyS: p.BacklogAfter.Seconds()}
}

// PlaceOne is the ingest's adapter onto uspace-core's network rule
// (timeplace.PlaceNetwork, T-02; the rid_time.json network cases run
// through it): a sample of a delivery that carries the client's send
// time sentAt is placed at rx - (sentAt - ts), so the client's clock
// error cancels; a sample of a WebSocket message (no sentAt) is placed
// at its own time when that is within the tolerance ahead of rx and the
// latency bound behind it, otherwise at rx (clock_ahead, too_old,
// counted). shown is false for a sample older than MaxAge: it is not
// taken. The time source is core's, mapped at this boundary as core's
// doc says a system may: a client time believed is source_clock (the
// source's own time placed against its delivery), a placement at
// receipt is receiver.
func PlaceOne(ts time.Time, sentAt *time.Time, rx time.Time, pol PlacePolicy) (p timeplace.Placement, note timeplace.NetworkNote, shown bool) {
	p, note, shown = timeplace.PlaceNetwork(ts, sentAt, rx, pol.network())
	if shown && p.Source == core.TimeBroadcast {
		p.Source = core.TimeSourceClock
	}
	return p, note, shown
}

// anchor is the clock relation of an aircraft's live samples: the source
// time of its newest live sample and where that sample was placed, and
// when (wall clock) the relation was learnt.
type anchor struct {
	ts, placed, learnt time.Time
}

// placed is where one sample of a delivery sits.
type placed struct {
	// tsEff is the sample's own time, clamped to rx + tolerance (T-13):
	// it orders the aircraft's samples, so a far-future time cannot pin
	// the track.
	tsEff      time.Time
	capturedAt time.Time
	source     core.TimeSource
	backlog    bool
	// shown is false for a sample too old to take (rejected_too_old).
	shown bool
	note  timeplace.NetworkNote
	// aheadClamped and byAnchor say which rule moved the placement
	// (each counted).
	aheadClamped, byAnchor bool
}

// place places the samples of one aircraft received together at rx
// (spec 02 F5, LESSONS T-01, T-02, T-04, T-11, T-13):
//
//  1. a batch without sentAt (batchRule): uspace-core's batch rule,
//     timeplace.PlaceBatch over the samples' own times with the 120 s
//     bound, rx - (newest ts - its ts), so the client's clock error
//     cancels within the delivery; otherwise PlaceOne (the network rule,
//     against sentAt or against rx);
//  2. a sample whose own time is ahead of rx by more than the tolerance
//     is ordered at rx (T-13), counted;
//  3. the aircraft's anchor, the placement of its newest live sample: a
//     sample is never placed later than anchor.placed + (ts -
//     anchor.ts), so input read late (a stalled reader, a client that
//     held its samples) is placed by the producer's own time, not by the
//     read (T-11, SC-15). The anchor carries the smallest delay seen, so
//     the rule only ever moves a sample earlier. AnchorMaxAge after it
//     was learnt from a sample placed at its receipt it may be relearnt,
//     which follows a client clock that drifted or stepped; it is not
//     relearnt from a delivery the old relation places more than
//     BacklogAfter behind its receipt, because that is late input, not
//     a clock change (anchor_kept);
//  4. backlog: the client's flag, or placed more than BacklogAfter behind
//     rx (T-04 is the client's flag; the second rule is T-11's
//     backstop).
//
// The anchor is returned updated by the live samples (never by backlog
// ones).
func place(rx time.Time, frames []Frame, sentAt *time.Time, batchRule bool, a *anchor, pol PlacePolicy) (out []placed, clamped int, next *anchor, kept bool) {
	ts := make([]time.Time, len(frames))
	out = make([]placed, len(frames))
	limit := rx.Add(pol.AheadTolerance)
	for i := range frames {
		t := frames[i].TS
		if t.After(limit) {
			t, out[i].aheadClamped = rx, true
		}
		ts[i], out[i].tsEff = t, t
	}
	if batchRule {
		var captured []time.Time
		captured, clamped = timeplace.PlaceBatch(rx, ts, timeplace.DefaultMaxBatchSpacing)
		for i := range out {
			out[i].capturedAt, out[i].source, out[i].shown = captured[i], core.TimeSourceClock, true
			if out[i].aheadClamped {
				out[i].source = core.TimeReceiver
			}
		}
	} else {
		for i := range frames {
			p, note, shown := PlaceOne(frames[i].TS, sentAt, rx, pol)
			out[i].capturedAt, out[i].source, out[i].note, out[i].shown = p.CapturedAt, p.Source, note, shown
		}
	}
	if a != nil && rx.Sub(a.learnt) > pol.AnchorMaxAge {
		// Relearn unless the old relation says this delivery is late.
		late := false
		for i := range frames {
			if out[i].shown && !frames[i].Backlog && !out[i].aheadClamped &&
				out[i].capturedAt.Sub(a.placed.Add(ts[i].Sub(a.ts))) > pol.BacklogAfter {
				late = true
			}
		}
		if late {
			kept = true
		} else {
			a = nil
		}
	}
	for i := range frames {
		p := &out[i]
		if !p.shown {
			continue
		}
		if a != nil && !p.aheadClamped {
			if byAnchor := a.placed.Add(ts[i].Sub(a.ts)); byAnchor.Before(p.capturedAt) {
				p.capturedAt, p.byAnchor, p.source = byAnchor, true, core.TimeSourceClock
			}
		}
		p.backlog = frames[i].Backlog || rx.Sub(p.capturedAt) > pol.BacklogAfter
	}
	for i := range frames {
		if !out[i].shown || frames[i].Backlog || out[i].aheadClamped {
			continue
		}
		if a == nil || !ts[i].Before(a.ts) {
			learnt := rx
			if out[i].byAnchor {
				learnt = a.learnt // carried: the relation is the old one
			}
			a = &anchor{ts: ts[i], placed: out[i].capturedAt, learnt: learnt}
		}
	}
	return out, clamped, a, kept
}
