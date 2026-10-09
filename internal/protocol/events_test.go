package protocol

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/robmorgan/agentd/internal/session"
)

func TestEventMessagesRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 123_456_789).UTC()
	roundTripRequest(t, &Request{SubscribeEvents: &SubscribeEvents{AfterID: u64p(42), SessionID: strp("demo"), Tail: 10}})
	roundTripRequest(t, &Request{SubscribeEvents: &SubscribeEvents{}})
	roundTripRequest(t, &Request{ListEvents: &ListEvents{AfterID: u64p(1 << 40), Limit: 100}})
	roundTripRequest(t, &Request{ListEvents: &ListEvents{SessionID: strp("demo")}})
	ev := session.Event{ID: 7, SessionID: "demo", At: now, Kind: session.EventNotification,
		Attention: session.AttentionAction, Summary: "Claude needs your permission"}
	roundTripResponse(t, &Response{Event: &ev})
	// Kinds are text, so ones this build does not know still decode.
	roundTripResponse(t, &Response{Events: &[]session.Event{ev, {ID: 8, SessionID: "x", At: now, Kind: "from-the-future", Attention: session.AttentionInfo}}})
	roundTripResponse(t, &Response{Events: &[]session.Event{}})
	if (&Request{SubscribeEvents: &SubscribeEvents{}}).Role() != RoleEvents || RoleEvents.String() != "events" {
		t.Fatal("SubscribeEvents does not open an events stream")
	}
	if (&Request{ListEvents: &ListEvents{}}).Role() != RoleRequest {
		t.Fatal("ListEvents is not a one-shot request")
	}
}

// TestEventGoldenFrames pins the events messages: a CLI follows events from
// daemons of other builds.
func TestEventGoldenFrames(t *testing.T) {
	assertRequestGolden(t, &Request{SubscribeEvents: &SubscribeEvents{AfterID: u64p(7), SessionID: strp("a"), Tail: 3}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 20
		0x13, 0x00, 0x00, 0x00,
		0x01, 0x07, 0, 0, 0, 0, 0, 0, 0, // after id
		0x01, 0x01, 0x00, 0x00, 0x00, 'a', // session
		0x03, 0x00, 0x00, 0x00, // tail
	})
	assertRequestGolden(t, &Request{ListEvents: &ListEvents{Limit: 5}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x15, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 21
		0x06, 0x00, 0x00, 0x00,
		0x00, 0x00, // no after id, no session
		0x05, 0x00, 0x00, 0x00, // limit
	})
	ev := session.Event{ID: 9, SessionID: "a", At: time.Unix(1, 2).UTC(), Kind: session.EventBell,
		Attention: session.AttentionAction, Summary: "b"}
	eventBytes := []byte{
		0x09, 0, 0, 0, 0, 0, 0, 0, // id
		0x01, 0x00, 0x00, 0x00, 'a', // session
		0x01, 0, 0, 0, 0, 0, 0, 0, 0x02, 0x00, 0x00, 0x00, // at
		0x04, 0x00, 0x00, 0x00, 'b', 'e', 'l', 'l', // kind
		0x03,                        // action
		0x01, 0x00, 0x00, 0x00, 'b', // summary
	}
	assertResponseGolden(t, &Response{Event: &ev}, append([]byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 120
		0x27, 0x00, 0x00, 0x00,
	}, eventBytes...))
	assertResponseGolden(t, &Response{Events: &[]session.Event{ev}}, append([]byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x79, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 121
		0x2b, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, // count
	}, eventBytes...))
}

// A session record carries its activity fields only between peers that
// both support CapSessionActivity, appended after the base fields, so a
// peer without it decodes the record it always did.
func TestSessionActivityIsNegotiated(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	rec := session.Record{SessionID: "a", Agent: "claude", Mode: session.ModeExecute, Cwd: "/w",
		Status: session.StatusRunning, Attention: session.AttentionAction, AttentionSummary: strp("bell"),
		CreatedAt: now, UpdatedAt: now, Activity: session.ActivityWaiting, Foreground: strp("claude"),
		Title: strp("✳ Fix"), LastOutputAt: &now, AttentionAt: &now}
	resp := &Response{Sessions: &[]session.Record{rec, rec}}
	with := NegotiateFeatures(Capabilities(), []string{CapSessionActivity})

	var base, full bytes.Buffer
	if err := WriteResponse(&base, resp); err != nil {
		t.Fatal(err)
	}
	if err := WriteResponseWith(&full, with, resp); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResponseWith(bytes.NewReader(full.Bytes()), with)
	if err != nil || !reflect.DeepEqual(got, resp) {
		t.Fatalf("with the capability: %#v, %v", got, err)
	}
	got, err = ReadResponse(bytes.NewReader(base.Bytes()))
	baseRec := rec
	baseRec.Activity, baseRec.Foreground, baseRec.Title, baseRec.LastOutputAt, baseRec.AttentionAt = "", nil, nil, nil, nil
	if err != nil || !reflect.DeepEqual(got, &Response{Sessions: &[]session.Record{baseRec, baseRec}}) {
		t.Fatalf("without it: %#v, %v", got, err)
	}
	// The appended fields: activity, foreground, title, last output, attention at.
	appended := 4 + len("waiting") + 1 + 4 + len("claude") + 1 + 4 + len("✳ Fix") + 1 + 12 + 1 + 12
	if full.Len()-base.Len() != 2*appended {
		t.Fatalf("records grew by %d bytes, want %d", full.Len()-base.Len(), 2*appended)
	}
	// Neither side can misread the other: the lengths no longer line up.
	if _, err := ReadResponse(bytes.NewReader(full.Bytes())); err == nil {
		t.Fatal("base decoder accepted appended fields")
	}
	if !HasCapability(Capabilities(), CapEvents) || !HasCapability(Capabilities(), CapSessionActivity) {
		t.Fatal("this build does not advertise its event capabilities")
	}
}

// The program status fields (OSC 7501) are appended after the activity
// fields, under their own capability: a peer with neither, or only
// CapSessionActivity, decodes the record it always did.
func TestSessionStatusIsNegotiated(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	progress := 47
	rec := session.Record{SessionID: "a", Agent: "claude", Mode: session.ModeExecute, Cwd: "/w",
		Status: session.StatusRunning, Attention: session.AttentionAction, AttentionSummary: strp("permission: Run tests?"),
		CreatedAt: now, UpdatedAt: now, Activity: session.ActivityBlocked, Foreground: strp("claude"),
		LastOutputAt: &now, AttentionAt: &now,
		StatusApp: strp("claude-code"), StatusKind: strp("permission"), StatusMsg: strp("Run tests?"), StatusProgress: &progress}
	resp := &Response{Session: &rec}
	activityOnly := NegotiateFeatures(Capabilities(), []string{CapSessionActivity})
	both := NegotiateFeatures(Capabilities(), []string{CapSessionActivity, CapSessionStatus})

	var partial, full bytes.Buffer
	if err := WriteResponseWith(&partial, activityOnly, resp); err != nil {
		t.Fatal(err)
	}
	if err := WriteResponseWith(&full, both, resp); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResponseWith(bytes.NewReader(full.Bytes()), both)
	if err != nil || !reflect.DeepEqual(got, resp) {
		t.Fatalf("with the capability: %#v, %v", got, err)
	}
	got, err = ReadResponseWith(bytes.NewReader(partial.Bytes()), activityOnly)
	partialRec := rec
	partialRec.StatusApp, partialRec.StatusKind, partialRec.StatusMsg, partialRec.StatusProgress = nil, nil, nil, nil
	if err != nil || !reflect.DeepEqual(got, &Response{Session: &partialRec}) {
		t.Fatalf("without it: %#v, %v", got, err)
	}
	// The appended bytes are pinned: app, kind and msg as optional strings
	// (presence byte + u32 length + bytes), progress as an optional u32
	// (presence byte + u32). Different builds negotiate this capability
	// with each other, so the layout must not drift.
	appended := 1 + 4 + len("claude-code") + 1 + 4 + len("permission") + 1 + 4 + len("Run tests?") + 1 + 4
	if full.Len()-partial.Len() != appended {
		t.Fatalf("status fields grew the record by %d bytes, want %d", full.Len()-partial.Len(), appended)
	}
	// Neither side can misread the other: the lengths no longer line up.
	if _, err := ReadResponseWith(bytes.NewReader(full.Bytes()), activityOnly); err == nil {
		t.Fatal("a decoder without the capability accepted the appended fields")
	}
	if !HasCapability(Capabilities(), CapSessionStatus) {
		t.Fatal("this build does not advertise CapSessionStatus")
	}
	// Out-of-contract progress never reaches the wire or the reader.
	bad := -1
	rec.StatusProgress = &bad
	var clamped bytes.Buffer
	if err := WriteResponseWith(&clamped, both, &Response{Session: &rec}); err != nil {
		t.Fatal(err)
	}
	got, err = ReadResponseWith(bytes.NewReader(clamped.Bytes()), both)
	if err != nil || got.Session.StatusProgress != nil {
		t.Fatalf("negative progress crossed the wire: %#v, %v", got.Session.StatusProgress, err)
	}
}
