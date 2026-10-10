package core

import (
	"context"
	"testing"
	"time"
)

// fakeCash answers whatever the test tells it to, and records what it got.
type fakeCash struct {
	ack      Acknowledge
	err      error
	got      []OrderRequest
	deadline bool // did the gateway give us a deadline?
}

func (f *fakeCash) Submit(ctx context.Context, r OrderRequest) (Acknowledge, error) {
	f.got = append(f.got, r)
	_, f.deadline = ctx.Deadline()
	return f.ack, f.err
}

// fakeSender records every message instead of writing to a socket.
type sent struct {
	conn ConnID
	msg  OutMsg
}

type fakeSender struct{ sent []sent }

func (f *fakeSender) Send(c ConnID, m OutMsg) bool {
	f.sent = append(f.sent, sent{c, m})
	return true
}

// fixed clock: the test decides what time it is.
var t0 = time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return t0 }

var alice = OrderRequest{AccountID: 7, ClientRequestID: 555, Symbol: 1, Side: Buy, PriceMinor: 10_000, Qty: 50}

const aliceConn ConnID = 3

func setup(cash *fakeCash) (*Gateway, *fakeSender) {
	out := &fakeSender{}
	return New(cash, out, fixedClock, 2*time.Second), out
}

// ---- Tests ----

func TestRejectDeletesPendingAndTellsAlice(t *testing.T) {
	cash := &fakeCash{ack: Acknowledge{Accepted: false, Reason: ReasonInsufficientCash}}
	gw, out := setup(cash)

	gw.OnPlace(context.Background(), aliceConn, alice)

	if len(gw.pending) != 0 {
		t.Fatalf("pending entry should be deleted on reject, have %d", len(gw.pending))
	}
	if len(out.sent) != 1 || out.sent[0].conn != aliceConn ||
		out.sent[0].msg.Kind != MsgRejected || out.sent[0].msg.Reason != ReasonInsufficientCash {
		t.Fatalf("Alice should get exactly one insufficient-cash reject, got %+v", out.sent)
	}
	if !cash.deadline {
		t.Fatal("gateway must call Submit with a deadline")
	}
}

func TestNoAnswerIsNeverReportedAsRejected(t *testing.T) {
	gw, out := setup(&fakeCash{err: ErrNoAnswer})

	gw.OnPlace(context.Background(), aliceConn, alice)

	if len(out.sent) != 0 {
		t.Fatalf("outcome unknown: must not tell Alice anything final, sent %+v", out.sent)
	}
	if len(gw.pending) != 1 {
		t.Fatal("outcome unknown: pending entry must be kept")
	}
}

func TestAcceptedThenEngineReplyDeliversAndDeletes(t *testing.T) {
	gw, out := setup(&fakeCash{ack: Acknowledge{Accepted: true}})

	gw.OnPlace(context.Background(), aliceConn, alice)
	if len(gw.pending) != 1 || len(out.sent) != 0 {
		t.Fatal("accepted is not final: keep the note, send nothing yet")
	}
	if e := gw.pending[requestKey{7, 555}]; e.sentAt != t0 {
		t.Fatalf("sentAt should come from the injected clock, got %v", e.sentAt)
	}

	gw.OnReply(EngineReply{AccountID: 7, ClientRequestID: 555, OrderID: 9001})

	if len(gw.pending) != 0 {
		t.Fatal("engine reply is final: note must be deleted")
	}
	if len(out.sent) != 1 || out.sent[0].conn != aliceConn || out.sent[0].msg.OrderID != 9001 {
		t.Fatalf("Alice should get her order_id, got %+v", out.sent)
	}
}

func TestReplyIsMatchedByAccountNotJustRequestID(t *testing.T) {
	// Alice and Bob both picked client_request_id 555 (LLD #2 clash case).
	gw, out := setup(&fakeCash{ack: Acknowledge{Accepted: true}})
	bob := alice
	bob.AccountID = 8
	const bobConn ConnID = 4

	gw.OnPlace(context.Background(), aliceConn, alice)
	gw.OnPlace(context.Background(), bobConn, bob)
	gw.OnReply(EngineReply{AccountID: 8, ClientRequestID: 555, OrderID: 9002})

	if len(out.sent) != 1 || out.sent[0].conn != bobConn {
		t.Fatalf("Bob's reply must go to Bob's connection, got %+v", out.sent)
	}
	if _, ok := gw.pending[requestKey{7, 555}]; !ok {
		t.Fatal("Alice's note must be untouched")
	}
}
