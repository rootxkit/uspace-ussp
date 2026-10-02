package conformance

import (
	"fmt"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// The adapters: this repository's wire types onto the judgement's
// inputs. The monitor, the vectors and the tests all go through them.

// AuthorisationOf reads an intent_active value (intent/state/v1) into an
// Authorisation: the outlines and windows of its F3548 volumes through
// intent.WireVolume, the AMSL bands as WP-7 derived them (volumes_amsl;
// nothing here converts, D-01), and the deviation thresholds. An intent
// without thresholds, with a volumes_amsl entry missing, or with a volume
// that does not read is refused naming the field: it is not judged, and
// never judged as conforming (E-15).
func AuthorisationOf(b intent.StateBody) (Authorisation, error) {
	a := Authorisation{IntentID: b.IntentID}
	if b.AuthorisationNumber != nil {
		a.AuthorisationNumber = *b.AuthorisationNumber
	}
	if b.DeviationThresholds == nil {
		return Authorisation{}, core.Fieldf("deviation_thresholds", "missing: the intent cannot be judged")
	}
	a.Thresholds = Thresholds{HM: b.DeviationThresholds.HM, VM: b.DeviationThresholds.VM, TS: b.DeviationThresholds.TS}
	if len(b.Volumes) == 0 || len(b.Volumes) > MaxVolumes {
		return Authorisation{}, core.Fieldf("volumes", "has %d volumes; 1 to %d", len(b.Volumes), MaxVolumes)
	}
	if len(b.VolumesAMSL) != len(b.Volumes) {
		return Authorisation{}, core.Fieldf("volumes_amsl", "has %d entries for %d volumes", len(b.VolumesAMSL), len(b.Volumes))
	}
	for i, v := range b.Volumes {
		w, err := intent.WireVolume(v, b.VolumesAMSL[i].UndulationM)
		if err != nil {
			return Authorisation{}, core.Fieldf(fmt.Sprintf("volumes[%d]", i), "%v", err)
		}
		band := b.VolumesAMSL[i]
		a.Volumes = append(a.Volumes, Volume{
			Shape: w.Shape, LowerAMSLM: band.LowerAMSLM, UpperAMSLM: band.UpperAMSLM, Start: w.Start, End: w.End,
		})
	}
	if err := a.Validate(); err != nil {
		return Authorisation{}, err
	}
	return a, nil
}

// FlyingOf reads the F3411 operational status (C-05): Airborne and
// Emergency fly, Ground does not, anything else (Undeclared,
// RemoteIDSystemFailure, none) is unknown.
func FlyingOf(status *string) *bool {
	if status == nil {
		return nil
	}
	var v bool
	switch f3411.RIDOperationalStatus(*status) {
	case f3411.Airborne, f3411.Emergency:
		v = true
	case f3411.Ground:
		v = false
	case f3411.Undeclared, f3411.RemoteIDSystemFailure:
		return nil
	default:
		return nil
	}
	return &v
}

// InputOf reads one track/telemetry/v1 message into an Input without its
// authorisation and source decision (the caller's).
func InputOf(tr *telemetry.Track) Input {
	b := &tr.Body
	in := Input{
		Sample: Sample{
			Position: core.LatLon{LatDeg: b.Position.Lat, LonDeg: b.Position.Lng},
			AltAMSLM: b.AltAMSLM, AltSource: b.AltSource, CapturedAt: tr.CapturedAt.Time,
		},
		Flying: FlyingOf(b.Status), RxAt: tr.RxTS.Time, Backlog: tr.Backlog,
	}
	if c5, _, err := cell.Key(in.Sample.Position); err == nil {
		in.Cell5 = c5
	}
	return in
}
