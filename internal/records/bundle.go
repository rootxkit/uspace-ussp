package records

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Bundle is one day's bundle as record_bundles holds it.
type Bundle struct {
	Date    time.Time
	BuiltAt time.Time
	// Hash is the SHA-256 of the stored file, in hex.
	Hash string
	// Ref is the file's name inside the records directory.
	Ref     string
	Flights int
}

// DayFlight is a flight of a day, in the order the bundle holds them.
type DayFlight struct {
	ID        string
	StartedAt time.Time
}

// BundleStore is record_bundles and the reads a bundle needs.
type BundleStore interface {
	// Now is the database clock.
	Now(ctx context.Context) (time.Time, error)
	// DayFlights are up to n flights started in [start, end) after
	// (afterAt, afterID), in that order.
	DayFlights(ctx context.Context, start, end, afterAt time.Time, afterID string, n int) ([]DayFlight, error)
	// InsertBundle stores b; false when the day has a bundle already.
	InsertBundle(ctx context.Context, b Bundle) (bool, error)
	// Bundle is the day's bundle, ErrNotFound when it has none.
	Bundle(ctx context.Context, day time.Time) (Bundle, error)
	// MissingDays are the days from first to last (inclusive) without a
	// bundle.
	MissingDays(ctx context.Context, first, last time.Time) ([]time.Time, error)
	// AuditBundle writes the events row of a bundle built.
	AuditBundle(ctx context.Context, b Bundle) error
}

// The day's schedule (spec 02 F7, brief WP-15): a day's bundle is built
// from 01:00 UTC the next day and missed from 02:00.
const (
	BuildAfter = 25 * time.Hour
	MissAfter  = 26 * time.Hour
	// CatchUpDays is how many days back a bundle missed (api down at
	// 01:00) is still built, and a missing one still alarmed.
	CatchUpDays = 7
	// MaxBundleFlights bounds one bundle; a day with more is refused
	// whole (B-13: refused, never thinned) and stays missing.
	MaxBundleFlights = 100_000
	dayPage          = 200
)

// DepRecords is the readiness dependency of the daily bundles.
const DepRecords = "records"

// Counters of the Daily job.
const (
	CounterBundlesBuilt  = "record_bundles_built"
	CounterBundlesFailed = "record_bundles_failed"
	CounterBundlesLost   = "record_bundles_built_twice"
)

var refRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9a-f]{12}\.jsonl\.gz$`)

// ErrBundleCorrupt is a stored bundle whose bytes no longer hash to its
// recorded content_hash.
var ErrBundleCorrupt = errors.New("the stored bundle does not match its recorded hash")

// Daily builds and serves the daily bundles.
type Daily struct {
	Builder *Builder
	Store   BundleStore
	// Dir is USSP_RECORDS_DIR ("": no bundle is built, and Probe says so).
	Dir string
	// MaxFlights bounds one bundle (0: MaxBundleFlights).
	MaxFlights int
	Counters   *core.Counters
	Logger     *slog.Logger
}

func (d *Daily) logger() *slog.Logger {
	if d.Logger == nil {
		return obs.Discard()
	}
	return d.Logger
}

func (d *Daily) count(name string) {
	if d.Counters != nil {
		d.Counters.Inc(name)
	}
}

// Day is the UTC day of t.
func Day(t time.Time) time.Time {
	y, m, dd := t.UTC().Date()
	return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)
}

// Run builds the due bundles every every until ctx ends.
func (d *Daily) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := d.BuildDue(ctx); err != nil && ctx.Err() == nil {
			d.logger().LogAttrs(ctx, slog.LevelWarn, "daily record bundles not built; tried again", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// BuildDue builds every day of the last CatchUpDays that is due
// (BuildAfter its start, on the database clock) and has no bundle, and
// returns how many it built.
func (d *Daily) BuildDue(ctx context.Context) (int, error) {
	if d.Dir == "" {
		return 0, nil
	}
	now, err := d.Store.Now(ctx)
	if err != nil {
		return 0, err
	}
	last := Day(now.Add(-BuildAfter))
	missing, err := d.Store.MissingDays(ctx, last.AddDate(0, 0, -(CatchUpDays-1)), last)
	if err != nil {
		return 0, err
	}
	built := 0
	var errs []error
	for _, day := range missing {
		if _, ok, err := d.Build(ctx, day); err != nil {
			errs = append(errs, err)
		} else if ok {
			built++
		}
	}
	return built, errors.Join(errs...)
}

// Build builds day's bundle: every flight that started that day (UTC),
// one record per line, gzip, written to a file of its own in Dir and
// renamed into place, its SHA-256 then stored in record_bundles. No
// transaction or lock is held while the records are built. False when
// another builder stored the day first (its file is kept, this one is
// removed).
func (d *Daily) Build(ctx context.Context, day time.Time) (Bundle, bool, error) {
	if d.Dir == "" {
		return Bundle{}, false, errors.New("USSP_RECORDS_DIR is not set")
	}
	day = Day(day)
	b, err := d.build(ctx, day)
	if err != nil {
		d.count(CounterBundlesFailed)
		d.logger().LogAttrs(ctx, slog.LevelError, "daily record bundle not built", slog.String("day", day.Format(time.DateOnly)), obs.Err(err))
		return Bundle{}, false, err
	}
	ok, err := d.Store.InsertBundle(ctx, b)
	if err != nil || !ok {
		// The same bytes have the same name: a file the stored bundle
		// names is never removed.
		if cur, gerr := d.Store.Bundle(ctx, day); gerr != nil || cur.Ref != b.Ref {
			_ = os.Remove(filepath.Join(d.Dir, b.Ref))
		}
		if err != nil {
			d.count(CounterBundlesFailed)
			return Bundle{}, false, fmt.Errorf("record_bundles: %w", err)
		}
		d.count(CounterBundlesLost)
		return Bundle{}, false, nil
	}
	if err := d.Store.AuditBundle(ctx, b); err != nil {
		d.logger().LogAttrs(ctx, slog.LevelError, "daily record bundle built but its events row not written", slog.String("day", day.Format(time.DateOnly)), obs.Err(err))
	}
	d.count(CounterBundlesBuilt)
	d.logger().LogAttrs(ctx, slog.LevelInfo, "daily record bundle built", slog.String("day", day.Format(time.DateOnly)),
		slog.Int("flights", b.Flights), slog.String("content_hash", b.Hash), slog.String("storage_ref", b.Ref))
	return b, true, nil
}

func (d *Daily) build(ctx context.Context, day time.Time) (Bundle, error) {
	tmp, err := os.CreateTemp(d.Dir, "."+day.Format(time.DateOnly)+"-*.tmp")
	if err != nil {
		return Bundle{}, fmt.Errorf("records directory: %w", err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	bw := bufio.NewWriter(io.MultiWriter(tmp, h))
	zw, err := gzip.NewWriterLevel(bw, gzip.BestCompression)
	if err != nil {
		return Bundle{}, err
	}
	zw.ModTime = day
	zw.Name = day.Format(time.DateOnly) + ".jsonl"
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	n, limit := 0, d.MaxFlights
	if limit <= 0 {
		limit = MaxBundleFlights
	}
	afterAt, afterID := time.Unix(0, 0).UTC(), "00000000-0000-0000-0000-000000000000"
	for {
		page, err := d.Store.DayFlights(ctx, day, day.AddDate(0, 0, 1), afterAt, afterID, dayPage)
		if err != nil {
			return Bundle{}, fmt.Errorf("flights of %s: %w", day.Format(time.DateOnly), err)
		}
		for _, f := range page {
			if n++; n > limit {
				return Bundle{}, fmt.Errorf("more than %d flights on %s: refused whole, never thinned", limit, day.Format(time.DateOnly))
			}
			r, err := d.Builder.Flight(ctx, f.ID)
			if err != nil {
				return Bundle{}, fmt.Errorf("record of flight %s: %w", f.ID, err)
			}
			if err := enc.Encode(r); err != nil {
				return Bundle{}, err
			}
			afterAt, afterID = f.StartedAt, f.ID
		}
		if len(page) < dayPage {
			break
		}
	}
	if err := zw.Close(); err != nil {
		return Bundle{}, err
	}
	if err := bw.Flush(); err != nil {
		return Bundle{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Bundle{}, err
	}
	if err := tmp.Close(); err != nil {
		return Bundle{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	ref := day.Format(time.DateOnly) + "-" + sum[:12] + ".jsonl.gz"
	if err := os.Rename(tmpName, filepath.Join(d.Dir, ref)); err != nil {
		return Bundle{}, err
	}
	keep = true
	return Bundle{Date: day, Hash: sum, Ref: ref, Flights: n}, nil
}

// Open is day's bundle and its file, its bytes verified against the
// recorded hash before anything is served (ErrBundleCorrupt otherwise);
// ErrNotFound when the day has none. The caller closes the file.
func (d *Daily) Open(ctx context.Context, day time.Time) (Bundle, *os.File, error) {
	b, err := d.Store.Bundle(ctx, Day(day))
	if err != nil {
		return Bundle{}, nil, err
	}
	if d.Dir == "" {
		return Bundle{}, nil, errors.New("USSP_RECORDS_DIR is not set")
	}
	if !refRe.MatchString(b.Ref) {
		return Bundle{}, nil, fmt.Errorf("storage_ref %q is not a bundle file name", b.Ref)
	}
	f, err := os.Open(filepath.Join(d.Dir, b.Ref))
	if err != nil {
		return Bundle{}, nil, fmt.Errorf("bundle file: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return Bundle{}, nil, err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), b.Hash) {
		_ = f.Close()
		return Bundle{}, nil, ErrBundleCorrupt
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return Bundle{}, nil, err
	}
	return b, f, nil
}

// Probe is the readiness of the daily bundles: degraded without a
// records directory, and for every day of the last CatchUpDays that has
// no bundle MissAfter its start ("records: day <date> missing", an
// alarm on this side); unknown when the table cannot be read.
func (d *Daily) Probe() obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		if d.Dir == "" {
			return obs.StateDegraded, "USSP_RECORDS_DIR is not set: no daily record bundle is built"
		}
		now, err := d.Store.Now(ctx)
		if err != nil {
			return obs.StateUnknown, "the record bundles cannot be read: " + clip(err.Error())
		}
		last := Day(now.Add(-MissAfter))
		missing, err := d.Store.MissingDays(ctx, last.AddDate(0, 0, -(CatchUpDays-1)), last)
		if err != nil {
			return obs.StateUnknown, "the record bundles cannot be read: " + clip(err.Error())
		}
		if len(missing) > 0 {
			days := make([]string, 0, len(missing))
			for _, m := range missing {
				days = append(days, m.Format(time.DateOnly))
			}
			return obs.StateDegraded, "records: day " + strings.Join(days, ", ") + " missing"
		}
		return obs.StateUp, "every daily bundle through " + last.Format(time.DateOnly) + " is built"
	}
}
