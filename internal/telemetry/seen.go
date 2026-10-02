package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"time"
)

// seenKey is the key of one sample in the shared replay window: a hash
// of the client, the serial's fold key and the epoch (any printable
// characters, which a KV key may not hold), then the seq.
func seenKey(ak aircraftKey, k replayKey) string {
	h := sha256.New()
	for _, part := range []string{ak.client, ak.fold, k.epoch} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16]) + "." + strconv.FormatInt(k.seq, 10)
}

// encodeSeen is the stored value: the sample's own time and when it was
// taken, Unix nanoseconds, big-endian.
func encodeSeen(e seenEntry) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[:8], uint64(e.ts.UnixNano()))
	binary.BigEndian.PutUint64(b[8:], uint64(e.at.UnixNano()))
	return b
}

func decodeSeen(b []byte) (seenEntry, bool) {
	if len(b) != 16 {
		return seenEntry{}, false
	}
	return seenEntry{
		ts: time.Unix(0, int64(binary.BigEndian.Uint64(b[:8]))).UTC(),
		at: time.Unix(0, int64(binary.BigEndian.Uint64(b[8:]))).UTC(),
	}, true
}

// sharedDuplicate asks the shared window (B-05 across restarts and
// replicas) about a sample this process's memory does not hold. A key is
// written only once its sample was handed, so a hit is a replay of a
// published sample (duplicate), or, with another ts, a reused seq. When
// the store cannot answer the sample is taken: published twice rather
// than lost (counted).
func (in *Ingestor) sharedDuplicate(ctx context.Context, ac *aircraft, k replayKey, ts, now time.Time, window time.Duration) (dup, reused bool) {
	if in.cfg.Seen == nil {
		return false, false
	}
	v, found, err := in.cfg.Seen.Get(ctx, seenKey(ac.key, k))
	if err != nil {
		in.count(CounterSeenUnavailable)
		return false, false
	}
	if !found {
		return false, false
	}
	e, ok := decodeSeen(v)
	if !ok || now.Sub(e.at) > window {
		return false, false
	}
	if !e.ts.Equal(ts) {
		return false, true
	}
	return true, false
}
