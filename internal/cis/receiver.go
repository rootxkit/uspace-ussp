package cis

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/cis/cispclient"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// NotificationsPath is the receiver's path on every subscriber (M1).
const NotificationsPath = "/v1/cis/notifications"

// ContentTypeJOSE is the media type of a compact JWS delivery.
const ContentTypeJOSE = "application/jose"

// Receiver bounds (E-10).
const (
	// MaxNotificationBytes bounds the request body: a compact JWS of a
	// cis/change/v1 record (a few KiB; feature_ids is bounded by the
	// CISP's publication caps).
	MaxNotificationBytes = 256 << 10
	// JTITTL is how long a delivery id is remembered: longer than the
	// 5 min during which core's CompactVerifier accepts its iat, plus
	// the skew, so a replay inside that time is always caught.
	JTITTL = 10 * time.Minute
	// MaxLiveJTIs bounds the remembered delivery ids; beyond it the
	// receiver answers 503 (the CISP retries) and counts it.
	MaxLiveJTIs = 100_000
)

// ChangeSchema is the schema of a change record.
const ChangeSchema = "cis/change/v1"

// Problem slugs of the receiver.
const (
	SlugUnsupportedMediaType = "unsupported_media_type"
	SlugStoreUnavailable     = "store_unavailable"
)

// pullReasons are the reasons that trigger a pull (M16): a publication
// and every restriction reason. subscription_test, republished and any
// reason this USSP does not know are acknowledged without a pull (the
// additive-enum rule of spec 04 §4).
var pullReasons = map[string]bool{
	"publication": true, "restriction_created": true, "restriction_activated": true,
	"restriction_extended": true, "restriction_ended": true, "restriction_cancelled": true,
	"restriction_expired": true,
}

// knownAckReasons are acknowledged without a pull and are not unknown.
var knownAckReasons = map[string]bool{"subscription_test": true, "republished": true}

// Sender says who an allow-listed notification issuer is.
type Sender struct {
	// ANSP is true for the ANSP's degraded direct path (M5), false for
	// the CISP.
	ANSP bool
	// BaseHost is the host of the issuer's configured base URL: a
	// pull_url is honoured only on it (the SSRF guard).
	BaseHost string
}

// CompactVerifier verifies a compact delivery JWS (core's
// auth.CompactVerifier).
type CompactVerifier interface {
	Verify(ctx context.Context, token string) (coreauth.CompactClaims, json.RawMessage, error)
}

// ReceiverStore is what the receiver writes.
type ReceiverStore interface {
	// RememberJTI records a verified delivery id for ttl on the
	// database clock. fresh is false for a replay; full is true when the
	// live ids reached maxLive and nothing was recorded.
	RememberJTI(ctx context.Context, issuer, jti string, ttl time.Duration, maxLive int64) (fresh, full bool, err error)
	// InsertNotification logs a verified notification that triggers a
	// pull.
	InsertNotification(ctx context.Context, n Notification) error
}

// Notification is one verified change notification.
type Notification struct {
	Dataset      Dataset
	Version      int64
	FeatureIDs   []string
	Reason       string
	Issuer       string
	JTI          string
	Subscription string
	MsgID        string
}

// ReceiverConfig configures a Receiver.
type ReceiverConfig struct {
	Verifier CompactVerifier
	// Senders are the allow-listed issuers, by iss.
	Senders  map[string]Sender
	Store    ReceiverStore
	Trigger  func(Dataset, Hint)
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
}

// Receiver is POST /v1/cis/notifications.
type Receiver struct{ cfg ReceiverConfig }

// NewReceiver builds a Receiver.
func NewReceiver(cfg ReceiverConfig) *Receiver {
	if cfg.Counters == nil {
		cfg.Counters = &core.Counters{}
	}
	if cfg.Logger == nil {
		cfg.Logger = obs.Discard()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Receiver{cfg: cfg}
}

// ServeHTTP verifies the delivery, remembers its id, and answers 204;
// a publication or restriction reason also logs it and triggers a pull
// of its dataset (asynchronously: the CISP wants an answer within 2 s).
func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != ContentTypeJOSE {
		rc.cfg.Counters.Inc(CounterWebhookMalformed)
		httpx.NewProblem(http.StatusUnsupportedMediaType, SlugUnsupportedMediaType, "", "the body must be a compact JWS, "+ContentTypeJOSE).Write(w, r)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxNotificationBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			rc.tooLarge(w, r)
			return
		}
		rc.cfg.Counters.Inc(CounterWebhookMalformed)
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "the body could not be read").Write(w, r)
		return
	}
	if len(raw) > MaxNotificationBytes {
		rc.tooLarge(w, r)
		return
	}
	claims, body, err := rc.cfg.Verifier.Verify(ctx, strings.TrimSpace(string(raw)))
	if err != nil {
		rc.cfg.Counters.Inc(CounterBadSignature)
		claim := "token"
		var te *coreauth.TokenError
		if errors.As(err, &te) {
			claim = te.Claim
		}
		rc.cfg.Logger.Warn("CIS notification refused", slog.String("claim", claim), obs.Err(err))
		httpx.NewProblem(http.StatusUnauthorized, CounterBadSignature, "", "the notification is not signed by an allowed issuer for this host",
			core.Fieldf(claim, "refused")).Write(w, r)
		return
	}
	sender, ok := rc.cfg.Senders[claims.Issuer]
	if !ok {
		// The verifier's allow-list and Senders are built from the same
		// configuration; a gap is refused, never trusted.
		rc.cfg.Counters.Inc(CounterBadSignature)
		httpx.NewProblem(http.StatusUnauthorized, CounterBadSignature, "", "the issuer is not a configured sender").Write(w, r)
		return
	}
	ch, ds, ferr := decodeChange(body)
	if ferr != nil {
		rc.cfg.Counters.Inc(CounterWebhookMalformed)
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "the notification is not a "+ChangeSchema+" record", ferr).Write(w, r)
		return
	}
	fresh, full, err := rc.cfg.Store.RememberJTI(ctx, claims.Issuer, claims.JTI, JTITTL, MaxLiveJTIs)
	switch {
	case err != nil:
		rc.cfg.Counters.Inc(CounterWebhookStoreFailed)
		obs.Error(ctx, rc.cfg.Logger, "CIS notification not recorded", err)
		httpx.RetryAfter(w, 5*time.Second)
		httpx.NewProblem(http.StatusServiceUnavailable, SlugStoreUnavailable, "", "the delivery id cannot be recorded; retry").Write(w, r)
		return
	case full:
		rc.cfg.Counters.Inc(CounterWebhookJTIFull)
		httpx.RetryAfter(w, 30*time.Second)
		httpx.NewProblem(http.StatusServiceUnavailable, CounterWebhookJTIFull, "", "too many recent deliveries; retry").Write(w, r)
		return
	case !fresh:
		rc.cfg.Counters.Inc(CounterWebhookReplayed)
		rc.cfg.Logger.Info("CIS notification replayed; acknowledged without action",
			slog.String("issuer", claims.Issuer), slog.String("jti", claims.JTI))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rc.cfg.Counters.Inc(CounterWebhooks)
	log := rc.cfg.Logger.With(slog.String("issuer", claims.Issuer), slog.String("jti", claims.JTI),
		slog.String("dataset", string(ds)), slog.Int64("version", ch.Version), slog.String("reason", ch.Reason))
	if !pullReasons[ch.Reason] {
		rc.cfg.Counters.Inc(CounterWebhookAckOnly)
		if !knownAckReasons[ch.Reason] {
			rc.cfg.Counters.Inc(CounterWebhookUnknown)
		}
		log.Info("CIS notification acknowledged without a pull")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h := Hint{Version: ch.Version, ETag: ch.Etag, Issuer: claims.Issuer, At: rc.cfg.Now()}
	switch host := hostOf(ch.PullUrl); {
	case host != sender.BaseHost:
		rc.cfg.Counters.Inc(CounterPullURLMismatch)
		log.Warn("pull_url is not on the issuer's configured host; the dataset is read from the configured CISP",
			slog.String("pull_url_host", short(host)))
	case sender.ANSP:
		// The ANSP's GET /v1/restrictions/{id} has no pinned contract
		// here yet: the dataset is read from the CISP.
		log.Info("ANSP direct notification; the dataset is read from the configured CISP")
	default:
		h.PullURL = ch.PullUrl
	}
	if sender.ANSP {
		rc.cfg.Counters.Inc(CounterANSPDirect)
	}
	if err := rc.cfg.Store.InsertNotification(ctx, Notification{
		Dataset: ds, Version: ch.Version, FeatureIDs: ch.FeatureIds, Reason: ch.Reason, Issuer: claims.Issuer,
		JTI: claims.JTI, Subscription: claims.Subject, MsgID: ch.MsgId,
	}); err != nil {
		rc.cfg.Counters.Inc(CounterWebhookStoreFailed)
		log.Warn("CIS notification not logged; pulling anyway", obs.Err(err))
	}
	rc.cfg.Trigger(ds, h)
	log.Info("CIS notification accepted; pull triggered")
	w.WriteHeader(http.StatusNoContent)
}

func (rc *Receiver) tooLarge(w http.ResponseWriter, r *http.Request) {
	rc.cfg.Counters.Inc(CounterWebhookMalformed)
	httpx.NewProblem(http.StatusRequestEntityTooLarge, httpx.SlugBodyTooLarge, "", "the notification is too large").Write(w, r)
}

// change is the part of cis/change/v1 the receiver reads.
type change struct {
	cispclient.Change
	Reason string
}

// decodeChange reads a cis/change/v1 record. Members the receiver does
// not know are ignored (records are additive within v1); the ones it
// needs are checked.
func decodeChange(body json.RawMessage) (change, Dataset, *core.FieldError) {
	var c cispclient.Change
	if err := json.Unmarshal(body, &c); err != nil {
		return change{}, "", core.Fieldf("body", "not a %s record", ChangeSchema)
	}
	if string(c.Schema) != ChangeSchema {
		return change{}, "", core.Fieldf("schema", "must be %s", ChangeSchema)
	}
	ds, ok := ParseDataset(string(c.Dataset))
	if !ok {
		return change{}, "", core.Fieldf("dataset", "not a CIS dataset")
	}
	if c.Version < 0 {
		return change{}, "", core.Fieldf("version", "negative")
	}
	if c.Reason == "" {
		return change{}, "", core.Fieldf("reason", "empty")
	}
	return change{Change: c, Reason: string(c.Reason)}, ds, nil
}
