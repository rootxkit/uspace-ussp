package ridsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
)

// Counters of the ISA notification receiver.
const (
	CounterNotifications        = "rid_sp_isa_notifications"
	CounterNotificationRefused  = "rid_sp_isa_notification_refused"
	CounterNotificationConflict = "rid_sp_isa_notification_conflict"
	CounterNotificationNotOwner = "rid_sp_isa_notification_not_owner"
	CounterNotificationUnstored = "rid_sp_isa_notification_unstored"
)

// MaxNotificationSubscriptions bounds the subscriptions one notification
// may name (E-10); NetDSSMaxSubscriptionPerArea is ten per area.
const MaxNotificationSubscriptions = 100

var entityUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ISANotification is one ISA notification as rid-sp keeps it for the
// Display Provider views (WP-14): the latest the ISA's sender pushed,
// by ISA id, with who sent it and when we received it. Deleted is true
// for a notification without service_area (the file: the ISA was
// deleted).
type ISANotification struct {
	ISAID         string                           `json:"isa_id"`
	Sender        string                           `json:"sender"`
	ReceivedAt    time.Time                        `json:"received_at"`
	Deleted       bool                             `json:"deleted"`
	ServiceArea   *f3411.IdentificationServiceArea `json:"service_area,omitempty"`
	Extents       *f3411.Volume4D                  `json:"extents,omitempty"`
	Subscriptions []f3411.SubscriptionState        `json:"subscriptions"`
}

// NotificationStore keeps the ISA notifications.
type NotificationStore interface {
	// Get is the stored notification of isaID; false when none.
	Get(ctx context.Context, isaID string) (ISANotification, bool, error)
	Put(ctx context.Context, n ISANotification) error
}

// ErrNotificationsUnavailable is the error of a notification that could
// not be stored; it is answered 500 and counted, and the sender retries.
var ErrNotificationsUnavailable = errors.New("the ISA notification store cannot take the notification")

// sameEntity reports whether a and b describe the same ISA state.
func sameEntity(a, b ISANotification) bool {
	ja, _ := json.Marshal(struct {
		S *f3411.IdentificationServiceArea
		E *f3411.Volume4D
	}{a.ServiceArea, a.Extents})
	jb, _ := json.Marshal(struct {
		S *f3411.IdentificationServiceArea
		E *f3411.Volume4D
	}{b.ServiceArea, b.Extents})
	return bytes.Equal(ja, jb)
}

func badNotification(format string, a ...any) stdf3411.PostIdentificationServiceAreaResponseObject {
	return stdf3411.PostIdentificationServiceArea400JSONResponse{Message: message(format, a...)}
}

// checkNotification refuses a notification that does not describe the
// ISA of the path: no subscription or more than the bound, a
// service_area of another id or without a version or base URL, extents
// that do not bound, extents without a service_area.
func checkNotification(id string, b *f3411.PutIdentificationServiceAreaNotificationParameters) string {
	switch {
	case !entityUUIDRe.MatchString(id):
		return "the ISA id is not a UUID"
	case b == nil:
		return "no body"
	case len(b.Subscriptions) == 0 || len(b.Subscriptions) > MaxNotificationSubscriptions:
		return "subscriptions must name 1 to 100 subscriptions"
	case b.ServiceArea == nil && b.Extents != nil:
		return "extents without a service_area"
	}
	for _, s := range b.Subscriptions {
		if !entityUUIDRe.MatchString(s.SubscriptionId) {
			return "a subscription_id is not a UUID"
		}
	}
	if a := b.ServiceArea; a != nil {
		u, err := url.Parse(a.UssBaseUrl)
		switch {
		case a.Id != id:
			return "service_area.id is not the ISA of the path"
		case a.Version == "" || len(a.Version) > 256:
			return "service_area.version is missing or too long"
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
			return "service_area.uss_base_url is not an absolute http(s) URL"
		case a.TimeStart.Value.IsZero() || a.TimeEnd.Value.IsZero() || !a.TimeEnd.Value.After(a.TimeStart.Value):
			return "service_area has no valid time window"
		}
	}
	if b.Extents != nil {
		if _, _, _, err := f3411.Volume4DToZonesEnvelope(*b.Extents); err != nil {
			return "extents: " + err.Error()
		}
	}
	return ""
}

// PostIdentificationServiceArea is POST
// /uss/identification_service_areas/{id}: a peer Service Provider (not
// the DSS) tells us, as a Display Provider, of an ISA it created,
// changed or deleted. 400 for a notification that does not read; 403
// when we hold an earlier one of this ISA from another sender (the file:
// not the owner according to the receiving client's records); 409 for
// the same version with a different entity; 204 once stored.
func (s *Server) PostIdentificationServiceArea(ctx context.Context, req stdf3411.PostIdentificationServiceAreaRequestObject) (stdf3411.PostIdentificationServiceAreaResponseObject, error) {
	if why := checkNotification(req.Id, req.Body); why != "" {
		s.count(CounterNotificationRefused)
		return badNotification("%s", why), nil
	}
	if s.Notifications == nil {
		s.count(CounterNotificationUnstored)
		return nil, ErrNotificationsUnavailable
	}
	sender := ""
	if p, ok := auth.PrincipalFrom(ctx); ok {
		sender = p.Claims.Subject
	}
	n := ISANotification{ISAID: req.Id, Sender: sender, ReceivedAt: s.now().UTC(), Deleted: req.Body.ServiceArea == nil,
		ServiceArea: req.Body.ServiceArea, Extents: req.Body.Extents, Subscriptions: req.Body.Subscriptions}
	prev, found, err := s.Notifications.Get(ctx, req.Id)
	if err != nil {
		s.count(CounterNotificationUnstored)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "ISA notification not read back; answered 500 for a retry", slog.String("isa_id", req.Id), obs.Err(err))
		return nil, err
	}
	if found {
		switch {
		case prev.Sender != sender:
			s.count(CounterNotificationNotOwner)
			return stdf3411.PostIdentificationServiceArea403JSONResponse{Message: message("another client notified this ISA before")}, nil
		case prev.ServiceArea != nil && n.ServiceArea != nil && prev.ServiceArea.Version == n.ServiceArea.Version && !sameEntity(prev, n):
			s.count(CounterNotificationConflict)
			return stdf3411.PostIdentificationServiceArea409JSONResponse{Message: message("version %s was notified before with another entity", n.ServiceArea.Version)}, nil
		}
	}
	if err := s.Notifications.Put(ctx, n); err != nil {
		s.count(CounterNotificationUnstored)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "ISA notification not stored; answered 500 for a retry", slog.String("isa_id", req.Id), obs.Err(err))
		return nil, err
	}
	s.count(CounterNotifications)
	return stdf3411.PostIdentificationServiceArea204Response{}, nil
}

// KVNotifications is NotificationStore on the KV bucket
// rid_isa_notifications (bounded, a day's TTL), so what peers told us
// survives a restart and is shared by every rid-sp instance.
type KVNotifications struct {
	JS jetstream.JetStream
	// Timeout bounds one read or write (bus.DefaultKVTimeout).
	Timeout time.Duration
	// open replaces the bucket lookup in tests.
	open func(ctx context.Context) (notificationKV, error)
}

// notificationKV is the part of a jetstream.KeyValue the store uses.
type notificationKV interface {
	Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	Put(ctx context.Context, key string, value []byte) (uint64, error)
}

func (k KVNotifications) bucket(ctx context.Context) (notificationKV, context.Context, context.CancelFunc, error) {
	t := k.Timeout
	if t <= 0 {
		t = bus.DefaultKVTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	var kv notificationKV
	var err error
	if k.open != nil {
		kv, err = k.open(ctx)
	} else {
		kv, err = k.JS.KeyValue(ctx, bus.BucketISANotifications)
	}
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return kv, ctx, cancel, nil
}

// Get implements NotificationStore.
func (k KVNotifications) Get(ctx context.Context, isaID string) (ISANotification, bool, error) {
	kv, ctx, cancel, err := k.bucket(ctx)
	if err != nil {
		return ISANotification{}, false, err
	}
	defer cancel()
	e, err := kv.Get(ctx, isaID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return ISANotification{}, false, nil
	}
	if err != nil {
		return ISANotification{}, false, err
	}
	var n ISANotification
	if err := json.Unmarshal(e.Value(), &n); err != nil {
		return ISANotification{}, false, err
	}
	return n, true, nil
}

// Put implements NotificationStore.
func (k KVNotifications) Put(ctx context.Context, n ISANotification) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	kv, ctx, cancel, err := k.bucket(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = kv.Put(ctx, n.ISAID, b)
	return err
}
