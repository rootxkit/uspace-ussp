package coordination

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// statusOf maps a fuzzed number onto an HTTP status 100 to 599.
func statusOf(n uint16) int { return 100 + int(n)%500 }

func permanentStatus(s int) bool {
	switch s {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// checkAnswerError holds an error decoded from an ANSP answer to its
// contract: permanent exactly for a refusal of the request, retryable
// otherwise, the status kept, the detail clipped and valid UTF-8 (it is
// stored as the notice's last error), Retry-After never negative.
func checkAnswerError(t *testing.T, status int, err error) {
	t.Helper()
	var perm *PermanentError
	var retry *RetryableError
	switch {
	case errors.As(err, &perm):
		if !permanentStatus(status) || perm.Status != status || len(perm.Detail) > 200 || !utf8.ValidString(perm.Detail) {
			t.Fatalf("status %d: %#v", status, perm)
		}
	case errors.As(err, &retry):
		if permanentStatus(status) || retry.Status != status || len(retry.Detail) > 200 || !utf8.ValidString(retry.Detail) || retry.RetryAfter < 0 {
			t.Fatalf("status %d: %#v", status, retry)
		}
	default:
		t.Fatalf("status %d: an error of neither kind: %v", status, err)
	}
}

func fuzzSeeds(f *testing.F) {
	f.Add(uint16(102), "", []byte(`{"ack_id":"ack-1","state":"received","received_at":"2026-10-04T12:00:00Z"}`))
	f.Add(uint16(100), "", []byte(`{"ack_id":"ack-1","state":"received","received_at":"2026-10-04T12:00:00Z"}`))
	f.Add(uint16(100), "", []byte(`{"notice_ref":"n","state":"acknowledged","acknowledged_at":"2026-10-04T12:01:00Z","acknowledged_by":"sup-1"}`))
	f.Add(uint16(309), "", []byte(`{"type":"https://schemas.uspace.ge/problems/notice-ref-reused","detail":"seen"}`))
	f.Add(uint16(403), "30", []byte(`{"type":"https://schemas.uspace.ge/problems/unavailable","detail":"ჩავარდნა"}`))
	f.Add(uint16(329), "-5", []byte(`{"type":"x","detail":"slow down"}`))
	f.Add(uint16(301), "", []byte(`not json`))
	f.Add(uint16(400), "", []byte(`{"type":"x","detail":"`+strings.Repeat("d", 196)+`ჩავარდნა"}`))
	f.Add(uint16(100), "", []byte(`{"notice_ref":"n","state":"acknowledged","acknowledged_by":"`+strings.Repeat("s", 199)+`ჩ"}`))
	f.Add(uint16(403), "12000000000000", []byte(`{}`))
}

// FuzzReceiptOf: whatever the ANSP answers a submit, the decoder never
// panics; a receipt it takes is a received one with an ack id of 1 to 64
// characters, a repeat exactly on 200; anything else is a classified
// error (checkAnswerError).
func FuzzReceiptOf(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, n uint16, retryAfter string, body []byte) {
		status := statusOf(n)
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		r, err := receiptOf(status, h, body)
		if err != nil {
			checkAnswerError(t, status, err)
			return
		}
		if (status != http.StatusOK && status != http.StatusAccepted) || r.AckID == "" || len(r.AckID) > 64 || r.Repeat != (status == http.StatusOK) ||
			r.ReceivedAt.Location() != time.UTC {
			t.Fatalf("status %d took %+v", status, r)
		}
	})
}

// FuzzNoticeStateOf: whatever the ANSP answers a get, the decoder never
// panics; a state it takes is one of the contract's, the acknowledger
// clipped and valid UTF-8 (it is stored and shown on the console), the
// time in UTC; anything else is a classified error.
func FuzzNoticeStateOf(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, n uint16, retryAfter string, body []byte) {
		status := statusOf(n)
		h := http.Header{"Retry-After": {retryAfter}}
		s, err := noticeStateOf(status, h, body)
		if err != nil {
			checkAnswerError(t, status, err)
			return
		}
		if status != http.StatusOK || s.State == "" || len(s.AcknowledgedBy) > 200 || !utf8.ValidString(s.AcknowledgedBy) ||
			(s.AcknowledgedAt != nil && s.AcknowledgedAt.Location() != time.UTC) {
			t.Fatalf("status %d took %+v", status, s)
		}
	})
}
