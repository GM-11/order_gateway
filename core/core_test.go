package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeCash answers whatever the test tells it to.
// onSubmit (optional) runs inside Submit, before it returns: tests use it to
// pause, cancel, or deliver an early engine reply.
// Safe to call from several goroutines at once (mu guards hadDeadline).
type fakeCash struct {
	ack         Acknowledge
	err         error
	onSubmit    func(ctx context.Context)
	mu          sync.Mutex
	hadDeadline bool
}

func (f *fakeCash) Submit(ctx context.Context, r OrderRequest) (Acknowledge, error) {
	_, ok := ctx.Deadline()
	f.mu.Lock()
	f.hadDeadline = ok
	f.mu.Unlock()
	if f.onSubmit != nil {
		f.onSubmit(ctx)
	}
	return f.ack, f.err
}

var t0 = time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return t0 }

var alice = OrderRequest{AccountID: 7, ClientOrderID: "c-555", Symbol: 1, Side: Buy, PriceMinor: 10_000, Qty: 50}

func setup(cash *fakeCash) *Gateway { return New(cash, fixedClock, 2*time.Second) }

func pendingCount(g *Gateway) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

type placed struct {
	res Result
	err error
}

// placeAsync runs Place in its own goroutine (Place blocks until it has an answer).
func placeAsync(g *Gateway, ctx context.Context, req OrderRequest) <-chan placed {
	done := make(chan placed, 1)
	go func() {
		r, err := g.Place(ctx, req)
		done <- placed{r, err}
	}()
	return done
}

// wait gives up after 1 s so a bug shows as a failure, not a hung test run.
func wait(t *testing.T, ch <-chan placed) placed {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(time.Second):
		t.Fatal("Place did not return within 1 s")
		return placed{}
	}
}

// ---- one test per way out of Place ----

func TestCashRejectReturnsReasonAndForgets(t *testing.T) {
	cash := &fakeCash{ack: Acknowledge{Accepted: false, Reason: ReasonInsufficientCash, ShortFallMinor: 35_000}}
	gw := setup(cash)

	res, err := gw.Place(context.Background(), alice)

	if err != nil {
		t.Fatalf("a definite no is an answer, not an error: %v", err)
	}
	if res.Outcome != OutcomeCashRejected || res.Reason != ReasonInsufficientCash || res.ShortfallMinor != 35_000 {
		t.Fatalf("got %+v, want cash-rejected, insufficient cash, shortfall 35000", res)
	}
	if n := pendingCount(gw); n != 0 {
		t.Fatalf("final answer: note must be gone, have %d", n)
	}
	if !cash.hadDeadline {
		t.Fatal("Submit must be called with a deadline")
	}
}

func TestCashNoAnswerIsUnknownNotRejected(t *testing.T) {
	gw := setup(&fakeCash{err: errors.New("rpc: connection reset")})

	res, err := gw.Place(context.Background(), alice)

	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err = %v, want ErrNoAnswer", err)
	}
	if res.Outcome == OutcomeCashRejected {
		t.Fatal("no answer must never be reported as rejected")
	}
	if n := pendingCount(gw); n != 0 {
		t.Fatalf("gave up: note must be gone, have %d", n)
	}
}

func TestCashYesThenEngineReplyReturnsOrderID(t *testing.T) {
	submitted := make(chan struct{})
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: true}, onSubmit: func(context.Context) { close(submitted) }})

	done := placeAsync(gw, context.Background(), alice)
	<-submitted
	gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "c-555", OrderID: 9001})
	p := wait(t, done)

	if p.err != nil || p.res.Outcome != OutcomePlaced || p.res.OrderID != 9001 {
		t.Fatalf("got %+v / %v, want placed with order 9001", p.res, p.err)
	}
	if n := pendingCount(gw); n != 0 {
		t.Fatalf("engine reply is final: note must be gone, have %d", n)
	}
}

func TestEngineRejectKeepsOrderID(t *testing.T) {
	submitted := make(chan struct{})
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: true}, onSubmit: func(context.Context) { close(submitted) }})

	done := placeAsync(gw, context.Background(), alice)
	<-submitted
	gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "c-555", OrderID: 9001, Rejected: true})
	p := wait(t, done)

	if p.err != nil || p.res.Outcome != OutcomeEngineRejected || p.res.OrderID != 9001 {
		t.Fatalf("got %+v / %v, want engine-rejected with order 9001 (it used a seq)", p.res, p.err)
	}
}

// The engine answers before the cash owner's "yes" gets back (reply-before-ack).
// The result waits in the one-slot mailbox; nothing is lost, no zombie note.
func TestEngineReplyBeforeCashYes(t *testing.T) {
	var gw *Gateway
	cash := &fakeCash{ack: Acknowledge{Accepted: true}}
	cash.onSubmit = func(context.Context) {
		gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "c-555", OrderID: 9001})
	}
	gw = setup(cash)

	res, err := gw.Place(context.Background(), alice)

	if err != nil || res.Outcome != OutcomePlaced || res.OrderID != 9001 {
		t.Fatalf("got %+v / %v, want placed with order 9001", res, err)
	}
	if n := pendingCount(gw); n != 0 {
		t.Fatalf("no zombie note allowed, have %d", n)
	}
}

// Alice's handler gives up after the cash owner's yes. Its note must go,
// and the late engine reply must be dropped without blocking.
func TestGiveUpForgetsAndLateReplyIsDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: true}, onSubmit: func(context.Context) { cancel() }})

	res, err := gw.Place(ctx, alice)

	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err = %v, want ErrNoAnswer after giving up (got result %+v)", err, res)
	}
	if n := pendingCount(gw); n != 0 {
		t.Fatalf("whoever gives up cleans up its own note, have %d", n)
	}

	returned := make(chan struct{})
	go func() {
		gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "c-555", OrderID: 9001})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("OnReply blocked on a late reply: the shared engine-feed goroutine would freeze")
	}
	if n := pendingCount(gw); n != 0 {
		t.Fatalf("late reply must not create a note, have %d", n)
	}
}

func TestSameIDWhileInFlightIsRefused(t *testing.T) {
	submitted := make(chan struct{})
	release := make(chan struct{})
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: true}, onSubmit: func(context.Context) {
		close(submitted)
		<-release // hold the first Place inside the cash owner
	}})

	first := placeAsync(gw, context.Background(), alice)
	<-submitted

	_, err := gw.Place(context.Background(), alice) // same account, same id
	if !errors.Is(err, ErrDuplicateInFlight) {
		t.Fatalf("err = %v, want ErrDuplicateInFlight", err)
	}

	close(release)
	gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "c-555", OrderID: 9001})
	if p := wait(t, first); p.err != nil || p.res.OrderID != 9001 {
		t.Fatalf("first request must still finish normally, got %+v / %v", p.res, p.err)
	}
}

// Alice and Bob both picked "c-555" (LLD #2 clash case): each gets their own answer.
func TestSameIDDifferentAccountsAreSeparate(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: true}, onSubmit: func(context.Context) { wg.Done() }})
	bob := alice
	bob.AccountID = 8

	aliceDone := placeAsync(gw, context.Background(), alice)
	bobDone := placeAsync(gw, context.Background(), bob)
	wg.Wait() // both notes are written

	gw.OnReply(EngineReply{AccountID: 8, ClientOrderID: "c-555", OrderID: 9002})
	gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "c-555", OrderID: 9001})

	if p := wait(t, aliceDone); p.res.OrderID != 9001 {
		t.Fatalf("Alice got %+v, want order 9001", p.res)
	}
	if p := wait(t, bobDone); p.res.OrderID != 9002 {
		t.Fatalf("Bob got %+v, want order 9002", p.res)
	}
}

// ---- duplicates (PlaceOrderAck.duplicate) ----

// The cash owner has seen this (account, id) and knows the order: answer from
// it at once. No engine Reply will come, so nothing waits for one.
func TestDuplicateWithOrderIDAnswersFromTheKnownOrder(t *testing.T) {
	known := &OrderState{Account: 7, ClientOrderID: "c-555", Symbol: 1, OrderID: 812, Qty: 50, FilledQty: 20, Status: StatusPartiallyFilled}
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: false, Duplicate: true, Order: known}})

	res, err := gw.Place(context.Background(), alice)

	if err != nil || res.Outcome != OutcomeDuplicate || res.Order == nil || res.Order.OrderID != 812 || res.Order.FilledQty != 20 {
		t.Fatalf("got %+v / %v, want OutcomeDuplicate carrying the known order", res, err)
	}
	if n := pendingCount(gw); n != 0 {
		t.Fatalf("duplicate answered: note must be gone, have %d", n)
	}
}

// accepted == true together with duplicate is still a duplicate: never wait for a Reply that will not come.
func TestDuplicateWinsOverAccepted(t *testing.T) {
	known := &OrderState{OrderID: 812}
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: true, Duplicate: true, Order: known}})
	res, err := gw.Place(context.Background(), alice)
	if err != nil || res.Outcome != OutcomeDuplicate {
		t.Fatalf("got %+v / %v", res, err)
	}
}

// Duplicate but the order has no id yet (the original is still in flight elsewhere).
func TestDuplicateWithoutOrderIDIsDuplicateInFlight(t *testing.T) {
	for name, order := range map[string]*OrderState{"nil order": nil, "order_id 0": {OrderID: 0}} {
		gw := setup(&fakeCash{ack: Acknowledge{Duplicate: true, Order: order}})

		_, err := gw.Place(context.Background(), alice)

		if !errors.Is(err, ErrDuplicateInFlight) {
			t.Fatalf("%s: err = %v, want ErrDuplicateInFlight", name, err)
		}
		if n := pendingCount(gw); n != 0 {
			t.Fatalf("%s: note must be gone, have %d", name, n)
		}
		// and a retry is not blocked by a leftover note
		if _, err := gw.Place(context.Background(), alice); !errors.Is(err, ErrDuplicateInFlight) {
			t.Fatalf("%s: retry err = %v", name, err)
		}
	}
}

// ---- engine replies ----

// A cancel/modify answer has an empty client_order_id: no pending lookup, nothing changes.
func TestReplyWithEmptyClientOrderIDIsIgnored(t *testing.T) {
	submitted := make(chan struct{})
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: true}, onSubmit: func(context.Context) { close(submitted) }})
	done := placeAsync(gw, context.Background(), alice)
	<-submitted

	gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "", OrderID: 9001})
	if n := pendingCount(gw); n != 1 {
		t.Fatalf("an empty-id reply must not touch the pending order, have %d notes", n)
	}

	gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "c-555", OrderID: 9001})
	if p := wait(t, done); p.res.OrderID != 9001 {
		t.Fatalf("got %+v", p.res)
	}
}

func TestEngineRejectReasonIsCarriedThrough(t *testing.T) {
	submitted := make(chan struct{})
	gw := setup(&fakeCash{ack: Acknowledge{Accepted: true}, onSubmit: func(context.Context) { close(submitted) }})
	done := placeAsync(gw, context.Background(), alice)
	<-submitted
	gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: "c-555", OrderID: 9001, Rejected: true, EngineReject: EngineRejectPriceBand})
	if p := wait(t, done); p.res.EngineReject != EngineRejectPriceBand {
		t.Fatalf("got %+v, want the price_band reason kept", p.res)
	}
}
