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
// when (wall clock) the relation was learnt. It is learnt from the
// receipt of WebSocket samples alone, never from a batch: a batch's
// sent_at is the client's word and could otherwise move every later live
// sample into the past.
type anchor struct {
	ts, placed, learnt time.Time
	// candOff is the receipt offset (rx - ts) a live stream has kept,
	// within the ahead tolerance, since candSince while the anchor
	// placed it more than BacklogAfter behind its receipt. Kept for
	// AnchorMaxAge, it replaces the anchor (a clock that stepped back).
	candOff   time.Duration
	candSince time.Time
	hasCand   bool
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
	// aheadClamped, byAnchor and sentAtDisagrees say which rule moved
	// the placement (each counted).
	aheadClamped, byAnchor, sentAtDisagrees bool
}

// placeStats is what one placement counted beside its samples.
type placeStats struct {
	// clamped is the batch rule's spacing clamps.
	clamped int
	// kept: the anchor was not relearnt after its age because the
	// delivery was late by it; relearnt: a consistent live stream
	// replaced it.
	kept, relearnt bool
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
//  3. a sample placed by sentAt that the anchor (receipt timing) places
//     more than BacklogAfter elsewhere is placed at its receipt, and the
//     disagreement is counted: sent_at is believed only where receipt
//     timing does not contradict it;
//  4. the aircraft's anchor, the placement of its newest live WebSocket
//     sample: a sample is never placed later than anchor.placed + (ts -
//     anchor.ts), so input read late (a stalled reader, a client that
//     held its samples) is placed by the producer's own time, not by the
//     read (T-11, SC-15). The anchor carries the smallest delay seen, so
//     the rule only ever moves a sample earlier. It is learnt only from
//     WebSocket samples (no sentAt, no batch rule) that the client did
//     not flag backlog. AnchorMaxAge after it was learnt from a sample
//     placed at its receipt it may be relearnt, which follows a client
//     clock that drifted or stepped; it is not relearnt from a delivery
//     the old relation places more than BacklogAfter behind its receipt,
//     because that may be late input, not a clock change (anchor_kept).
//     A live stream that the anchor places that late but that keeps one
//     receipt offset (within the ahead tolerance) over AnchorMaxAge of
//     receipts is a clock change, not a stall (a stall reads many
//     samples at one receipt): it replaces the anchor (anchor_relearnt);
//  5. backlog: the client's flag, or placed more than BacklogAfter behind
//     rx (T-04 is the client's flag; the second rule is T-11's
//     backstop).
//
// The anchor is returned updated by the live WebSocket samples (never by
// backlog or batch ones).
func place(rx time.Time, frames []Frame, sentAt *time.Time, batchRule bool, a *anchor, pol PlacePolicy) (out []placed, next *anchor, st placeStats) {
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
		captured, st.clamped = timeplace.PlaceBatch(rx, ts, timeplace.DefaultMaxBatchSpacing)
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
	byAnchor := func(a *anchor, i int) time.Time { return a.placed.Add(ts[i].Sub(a.ts)) }
	if sentAt != nil && a != nil {
		for i := range frames {
			p := &out[i]
			if !p.shown || p.aheadClamped || p.source == core.TimeReceiver {
				continue
			}
			byRx := byAnchor(a, i)
			if byRx.After(rx) {
				byRx = rx
			}
			if d := p.capturedAt.Sub(byRx); d > pol.BacklogAfter || d < -pol.BacklogAfter {
				p.capturedAt, p.source, p.sentAtDisagrees = rx, core.TimeReceiver, true
			}
		}
	}
	fromReceipt := sentAt == nil && !batchRule
	if fromReceipt && a != nil {
		c := *a // the caller's anchor is replaced, never changed in place
		a = &c
		for i := range frames {
			if !out[i].shown || frames[i].Backlog || out[i].aheadClamped {
				continue
			}
			if rx.Sub(byAnchor(a, i)) <= pol.BacklogAfter {
				a.hasCand = false // the anchor agrees with receipt
				continue
			}
			off := rx.Sub(ts[i])
			if d := off - a.candOff; !a.hasCand || d > pol.AheadTolerance || d < -pol.AheadTolerance {
				a.candOff, a.candSince, a.hasCand = off, rx, true
				continue
			}
			if rx.Sub(a.candSince) >= pol.AnchorMaxAge {
				a, st.relearnt = nil, true
				break
			}
		}
	}
	if fromReceipt && a != nil && rx.Sub(a.learnt) > pol.AnchorMaxAge {
		// Relearn unless the old relation says this delivery is late.
		late := false
		for i := range frames {
			if out[i].shown && !frames[i].Backlog && !out[i].aheadClamped &&
				out[i].capturedAt.Sub(byAnchor(a, i)) > pol.BacklogAfter {
				late = true
			}
		}
		if late {
			st.kept = true
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
			if t := byAnchor(a, i); t.Before(p.capturedAt) {
				p.capturedAt, p.byAnchor, p.source = t, true, core.TimeSourceClock
			}
		}
		p.backlog = frames[i].Backlog || rx.Sub(p.capturedAt) > pol.BacklogAfter
	}
	if !fromReceipt {
		return out, a, st
	}
	for i := range frames {
		if !out[i].shown || frames[i].Backlog || out[i].aheadClamped {
			continue
		}
		if a == nil || !ts[i].Before(a.ts) {
			n := &anchor{ts: ts[i], placed: out[i].capturedAt, learnt: rx}
			if a != nil {
				n.candOff, n.candSince, n.hasCand = a.candOff, a.candSince, a.hasCand
				if out[i].byAnchor {
					n.learnt = a.learnt // carried: the relation is the old one
				}
			}
			a = n
		}
	}
	return out, a, st
}
