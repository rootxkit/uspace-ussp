package ridsp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
)

type memNotifications struct {
	mu     sync.Mutex
	m      map[string]ISANotification
	getErr error
	putErr error
}

func (s *memNotifications) Get(_ context.Context, id string) (ISANotification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return ISANotification{}, false, s.getErr
	}
	n, ok := s.m[id]
	return n, ok, nil
}

func (s *memNotifications) Put(_ context.Context, n ISANotification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return s.putErr
	}
	if s.m == nil {
		s.m = map[string]ISANotification{}
	}
	s.m[n.ISAID] = n
	return nil
}

const isaID = "3e5572a0-f733-49af-bc14-8a18bd53ee39"

func notification(version string) *f3411.PutIdentificationServiceAreaNotificationParameters {
	idx := int32(3)
	now := time.Now()
	ext := f3411.Volume4D{Volume: boxVolume(geodeticBox(origin, 0.01)),
		TimeStart: &f3411.Time{Format: f3411.RFC3339, Value: now}, TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: now.Add(time.Hour)}}
	return &f3411.PutIdentificationServiceAreaNotificationParameters{
		Subscriptions: []f3411.SubscriptionState{{SubscriptionId: "11111111-1111-4111-8111-111111111111", NotificationIndex: &idx}},
		ServiceArea: &f3411.IdentificationServiceArea{Id: isaID, Owner: "peer", UssBaseUrl: "https://peer.test/rid", Version: version,
			TimeStart: *ext.TimeStart, TimeEnd: *ext.TimeEnd},
		Extents: &ext,
	}
}

func as(sub string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Claims: coreauth.Claims{Subject: sub}})
}

func post(t *testing.T, s *Server, ctx context.Context, id string, b *f3411.PutIdentificationServiceAreaNotificationParameters) (stdf3411.PostIdentificationServiceAreaResponseObject, error) {
	t.Helper()
	return s.PostIdentificationServiceArea(ctx, stdf3411.PostIdentificationServiceAreaRequestObject{Id: id, Body: b})
}

// A peer's ISA notification both ways (E-01): stored and 204; the same
// again 204; a new version 204; the same version with another entity
// 409; another sender 403; a deletion stored as deleted; a body that
// does not describe the ISA of the path 400 (each counted).
func TestNotificationReceiver(t *testing.T) {
	store := &memNotifications{}
	s := &Server{Notifications: store, Counters: &core.Counters{}}
	res, err := post(t, s, as("peer"), isaID, notification("v1"))
	if _, ok := res.(stdf3411.PostIdentificationServiceArea204Response); !ok || err != nil {
		t.Fatalf("%#v %v", res, err)
	}
	n := store.m[isaID]
	if n.Sender != "peer" || n.Deleted || n.ServiceArea.Version != "v1" || len(n.Subscriptions) != 1 || n.ReceivedAt.IsZero() {
		t.Fatalf("stored %+v", n)
	}
	for _, v := range []string{"v1", "v2"} {
		if res, _ := post(t, s, as("peer"), isaID, notification(v)); res != (stdf3411.PostIdentificationServiceArea204Response{}) {
			t.Fatalf("%s: %#v", v, res)
		}
	}
	other := notification("v2")
	other.ServiceArea.UssBaseUrl = "https://elsewhere.test/rid"
	if res, _ := post(t, s, as("peer"), isaID, other); func() bool { _, ok := res.(stdf3411.PostIdentificationServiceArea409JSONResponse); return !ok }() {
		t.Fatalf("same version, another entity: %#v", res)
	}
	if res, _ := post(t, s, as("intruder"), isaID, notification("v3")); func() bool { _, ok := res.(stdf3411.PostIdentificationServiceArea403JSONResponse); return !ok }() {
		t.Fatalf("another sender: %#v", res)
	}
	del := notification("")
	del.ServiceArea, del.Extents = nil, nil
	if res, _ := post(t, s, as("peer"), isaID, del); res != (stdf3411.PostIdentificationServiceArea204Response{}) || !store.m[isaID].Deleted {
		t.Fatalf("deletion: %#v %+v", res, store.m[isaID])
	}
	if s.Counters.Get(CounterNotifications) != 4 || s.Counters.Get(CounterNotificationConflict) != 1 || s.Counters.Get(CounterNotificationNotOwner) != 1 {
		t.Errorf("%v", s.Counters.Snapshot())
	}

	bad := map[string]func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters){
		"id": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			return "x", notification("v1")
		},
		"no body": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) { return isaID, nil },
		"no subs": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			n := notification("v1")
			n.Subscriptions = nil
			return isaID, n
		},
		"sub id": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			n := notification("v1")
			n.Subscriptions[0].SubscriptionId = "s"
			return isaID, n
		},
		"other id": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			n := notification("v1")
			n.ServiceArea.Id = flightN(9)
			return isaID, n
		},
		"no version": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			return isaID, notification("")
		},
		"base url": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			n := notification("v1")
			n.ServiceArea.UssBaseUrl = "peer"
			return isaID, n
		},
		"window": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			n := notification("v1")
			n.ServiceArea.TimeEnd = n.ServiceArea.TimeStart
			return isaID, n
		},
		"bad extents": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			n := notification("v1")
			n.Extents.Volume = f3411.Volume3D{}
			return isaID, n
		},
		"orphan extent": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			n := notification("v1")
			n.ServiceArea = nil
			return isaID, n
		},
		"too many subs": func() (string, *f3411.PutIdentificationServiceAreaNotificationParameters) {
			n := notification("v1")
			for len(n.Subscriptions) <= MaxNotificationSubscriptions {
				n.Subscriptions = append(n.Subscriptions, n.Subscriptions[0])
			}
			return isaID, n
		},
	}
	for name, mk := range bad {
		id, b := mk()
		res, err := post(t, s, as("peer"), id, b)
		if _, ok := res.(stdf3411.PostIdentificationServiceArea400JSONResponse); !ok || err != nil {
			t.Errorf("%s: %#v %v", name, res, err)
		}
	}
	if s.Counters.Get(CounterNotificationRefused) != uint64(len(bad)) {
		t.Errorf("refused %v", s.Counters.Snapshot())
	}
}

// A store that cannot read or take the notification answers an error
// (500, the sender retries), counted; no store at all likewise.
func TestNotificationStoreDown(t *testing.T) {
	st := &memNotifications{getErr: errors.New("nats down")}
	s := &Server{Notifications: st, Counters: &core.Counters{}}
	if _, err := post(t, s, as("peer"), isaID, notification("v1")); err == nil {
		t.Fatal("read failure not returned")
	}
	st.getErr, st.putErr = nil, errors.New("nats down")
	if _, err := post(t, s, as("peer"), isaID, notification("v1")); err == nil {
		t.Fatal("write failure not returned")
	}
	if _, err := post(t, &Server{Counters: s.Counters}, as("peer"), isaID, notification("v1")); !errors.Is(err, ErrNotificationsUnavailable) {
		t.Fatal(err)
	}
	if s.Counters.Get(CounterNotificationUnstored) != 3 {
		t.Errorf("%v", s.Counters.Snapshot())
	}
}
