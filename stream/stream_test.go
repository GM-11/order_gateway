package stream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"order_gateway/auth"
	"order_gateway/core"
	"order_gateway/deliver"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// fakeAuth: key "PK<n>"/secret "ok" and token "jwt<n>" are account n; anything else fails.
type fakeAuth struct{}

func (fakeAuth) Verify(c auth.Credentials) (auth.Principal, error) {
	if c.KeyID != "" || c.Secret != "" {
		if n, err := strconv.Atoi(strings.TrimPrefix(c.KeyID, "PK")); err == nil && c.Secret == "ok" {
			return auth.Principal{Account: core.AccountID(n), Method: auth.MethodAPIKey}, nil
		}
		return auth.Principal{}, auth.ErrBadCredentials
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(c.Bearer, "jwt")); err == nil && strings.HasPrefix(c.Bearer, "jwt") {
		return auth.Principal{Account: core.AccountID(n), Method: auth.MethodBrowser}, nil
	}
	return auth.Principal{}, auth.ErrNoCredentials
}

type fakeCash struct {
	mu    sync.Mutex
	snaps map[core.AccountID]core.AccountSnapshot
	err   error
	calls map[core.AccountID]int
	hook  func() // runs inside GetAccount, after the snapshot is read; tests block here
}

func (f *fakeCash) GetAccount(_ context.Context, a core.AccountID) (core.AccountSnapshot, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[core.AccountID]int{}
	}
	f.calls[a]++
	err, snap, hook := f.err, f.snaps[a], f.hook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return snap, err
}

func (f *fakeCash) setHook(h func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

func (f *fakeCash) set(a core.AccountID, s core.AccountSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snaps == nil {
		f.snaps = map[core.AccountID]core.AccountSnapshot{}
	}
	f.snaps[a] = s
}

func (f *fakeCash) callsFor(a core.AccountID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[a]
}

func tick(id core.SymbolID) (string, bool) {
	if id == 1 {
		return "MOOG", true
	}
	return "", false
}

type fixture struct {
	srv  *Server
	cash *fakeCash
	ts   *httptest.Server
}

func newFixture(t *testing.T, outbox int, authTimeout time.Duration) *fixture {
	t.Helper()
	cash := &fakeCash{}
	srv := &Server{Auth: fakeAuth{}, Cash: cash, Hub: deliver.NewHub(outbox), Tick: tick, AuthTimeout: authTimeout}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &fixture{srv: srv, cash: cash, ts: ts}
}

// client reads in the background (with no deadline, because a cancelled read
// closes a coder/websocket connection), so a test can wait for "nothing" without
// breaking the connection.
type client struct {
	conn   *websocket.Conn
	frames chan frameMsg
	ended  chan websocket.StatusCode
}

func (f *fixture) dial(t *testing.T) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.ts.URL, "http")+"/v2/stream", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetReadLimit(1 << 26)
	t.Cleanup(func() { _ = c.CloseNow() })
	cl := &client{conn: c, frames: make(chan frameMsg, 1024), ended: make(chan websocket.StatusCode, 1)}
	go func() {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				cl.ended <- websocket.CloseStatus(err)
				return
			}
			var m frameMsg
			if json.Unmarshal(b, &m) != nil {
				m = frameMsg{Stream: "not-json", Data: b}
			}
			cl.frames <- m
		}
	}()
	return cl
}

func write(t *testing.T, c *client, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

type frameMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

func read(t *testing.T, c *client) frameMsg {
	t.Helper()
	select {
	case m := <-c.frames:
		return m
	case code := <-c.ended:
		t.Fatalf("connection ended (code %d) while waiting for a frame", code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a frame")
	}
	return frameMsg{}
}

func expectNothing(t *testing.T, c *client, why string) {
	t.Helper()
	select {
	case m := <-c.frames:
		t.Fatalf("%s: unexpected frame %s %s", why, m.Stream, m.Data)
	case code := <-c.ended:
		t.Fatalf("%s: connection ended (code %d)", why, code)
	case <-time.After(150 * time.Millisecond):
	}
}

// closeCode waits for the connection to end (draining frames) and returns how.
func closeCode(t *testing.T, c *client) websocket.StatusCode {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case <-c.frames:
		case code := <-c.ended:
			return code
		case <-deadline:
			t.Fatal("connection did not end")
		}
	}
}

// login authenticates as account n and listens; returns after the snapshot.
func (f *fixture) login(t *testing.T, n int) *client {
	t.Helper()
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "key_id": "PK" + strconv.Itoa(n), "secret": "ok"})
	if m := read(t, c); m.Stream != "authorization" || !strings.Contains(string(m.Data), `"authorized"`) {
		t.Fatalf("auth: %+v", m)
	}
	write(t, c, map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"trade_updates"}}})
	if m := read(t, c); m.Stream != "listening" {
		t.Fatalf("listen: %+v", m)
	}
	if m := read(t, c); m.Stream != "account_snapshot" {
		t.Fatalf("snapshot: %+v", m)
	}
	return c
}

func snap(a core.AccountID, seq uint64, free int64) core.AccountSnapshot {
	return core.AccountSnapshot{Found: true, Account: a, Seq: seq, Balances: core.Balances{CashFreeMinor: free, CashLockedMinor: 500},
		Positions:  []core.Position{{Symbol: 1, FreeQty: 4, LockedQty: 6, AvgPriceMinor: 9800}},
		OpenOrders: []core.OrderState{{Account: a, ClientOrderID: "abc", Symbol: 1, OrderID: 812, Side: core.Buy, Type: core.TypeLimit, TimeInForce: core.TIFGoodTilCancelled, PriceMinor: 10000, Qty: 10, Status: core.StatusNew}}}
}

func fillUpdate(a core.AccountID, seq uint64) core.AccountUpdate {
	return core.AccountUpdate{
		Account: a, Seq: seq, TsMs: 1_792_225_800_123, Kind: core.UpdatePartialFill,
		Order:          &core.OrderState{Account: a, ClientOrderID: "abc", Symbol: 1, OrderID: 812, Side: core.Buy, PriceMinor: 10000, Qty: 10, FilledQty: 6, FilledNotionalMinor: 58800, Status: core.StatusPartiallyFilled},
		FillPriceMinor: 9800, FillQty: 6,
		Balances:  core.Balances{CashFreeMinor: 1000, CashLockedMinor: 200, RealizedPnlMinor: 5},
		Positions: []core.Position{{Symbol: 2, FreeQty: 1}, {Symbol: 1, FreeQty: 6, AvgPriceMinor: 9800}},
	}
}

// ── connection flow ──────────────────────────────────────────────────────────

func TestAuthTimeoutClosesWith4001(t *testing.T) {
	f := newFixture(t, 8, 100*time.Millisecond)
	c := f.dial(t) // says nothing
	if code := closeCode(t, c); code != CloseUnauthorized {
		t.Fatalf("closed with %d, want 4001", code)
	}
}

func TestBadAuthSendsUnauthorizedThenCloses4001(t *testing.T) {
	cases := map[string]any{
		"wrong secret":      map[string]any{"action": "auth", "key_id": "PK1", "secret": "nope"},
		"bad token":         map[string]any{"action": "auth", "token": "garbage"},
		"no credentials":    map[string]any{"action": "auth"},
		"not an auth first": map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"trade_updates"}}},
	}
	for name, msg := range cases {
		f := newFixture(t, 8, time.Second)
		c := f.dial(t)
		write(t, c, msg)
		m := read(t, c)
		if m.Stream != "authorization" || !strings.Contains(string(m.Data), `"unauthorized"`) {
			t.Errorf("%s: first frame = %+v", name, m)
		}
		if code := closeCode(t, c); code != CloseUnauthorized {
			t.Errorf("%s: closed with %d, want 4001", name, code)
		}
	}
}

func TestListenSendsListeningThenSnapshot(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	f.cash.set(1, snap(1, 41, 1000))
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "token": "jwt1"}) // a browser JWT works too
	if m := read(t, c); m.Stream != "authorization" {
		t.Fatalf("got %+v", m)
	}
	write(t, c, map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"trade_updates"}}})
	l := read(t, c)
	if l.Stream != "listening" || !strings.Contains(string(l.Data), `"trade_updates"`) {
		t.Fatalf("got %+v", l)
	}
	m := read(t, c)
	if m.Stream != "account_snapshot" {
		t.Fatalf("got %+v", m)
	}
	var d struct {
		AccountSeq uint64 `json:"account_seq"`
		Account    map[string]any
		Positions  []map[string]any
		OpenOrders []map[string]any `json:"open_orders"`
	}
	if err := json.Unmarshal(m.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.AccountSeq != 41 || d.Account["cash"] != "15.00" || d.Account["cash_free"] != "10.00" || d.Account["account_id"] != "1" {
		t.Errorf("account part: %+v", d)
	}
	if len(d.Positions) != 1 || d.Positions[0]["symbol"] != "MOOG" || d.Positions[0]["qty"] != "10" {
		t.Errorf("positions: %+v", d.Positions)
	}
	if len(d.OpenOrders) != 1 || d.OpenOrders[0]["order_id"] != "1-812" || d.OpenOrders[0]["status"] != "new" {
		t.Errorf("open_orders: %+v", d.OpenOrders)
	}
}

func TestUnknownAccountGetsAZeroSnapshot(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	f.cash.set(9, core.AccountSnapshot{Found: false})
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "key_id": "PK9", "secret": "ok"})
	read(t, c)
	write(t, c, map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"trade_updates"}}})
	read(t, c)
	m := read(t, c)
	var d struct {
		AccountSeq uint64 `json:"account_seq"`
		Account    map[string]any
		Positions  []any
		OpenOrders []any `json:"open_orders"`
	}
	_ = json.Unmarshal(m.Data, &d)
	if m.Stream != "account_snapshot" || d.AccountSeq != 0 || d.Account["cash"] != "0.00" || d.Account["account_id"] != "9" ||
		d.Positions == nil || len(d.Positions) != 0 || d.OpenOrders == nil || len(d.OpenOrders) != 0 {
		t.Errorf("got %s: %+v", m.Stream, d)
	}
}

func TestListenWithUnknownStreamGrantsNothing(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "key_id": "PK1", "secret": "ok"})
	read(t, c)
	write(t, c, map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"quotes"}}})
	l := read(t, c)
	if l.Stream != "listening" || !strings.Contains(string(l.Data), `"streams":[]`) {
		t.Errorf("got %+v", l)
	}
	f.srv.OnUpdate(fillUpdate(1, 1))
	expectNothing(t, c, "a connection that is not listening gets no updates")
}

func TestResyncSendsAFreshSnapshot(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	f.cash.set(1, snap(1, 41, 1000))
	c := f.login(t, 1)
	f.cash.set(1, snap(1, 45, 2000))
	write(t, c, map[string]any{"action": "resync"})
	m := read(t, c)
	if m.Stream != "account_snapshot" || !strings.Contains(string(m.Data), `"account_seq":45`) {
		t.Errorf("got %s %s", m.Stream, m.Data)
	}
}

func TestResyncBeforeListenIsIgnoredAndUnknownActionClosesTheConnection(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "key_id": "PK1", "secret": "ok"})
	read(t, c)
	write(t, c, map[string]any{"action": "resync"})
	expectNothing(t, c, "resync before listen")
	write(t, c, map[string]any{"action": "dance"})
	if code := closeCode(t, c); code != websocket.StatusPolicyViolation {
		t.Errorf("closed with %d, want 1008", code)
	}
}

func TestSnapshotFailureClosesTheConnection(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	f.cash.err = core.ErrNoAnswer
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "key_id": "PK1", "secret": "ok"})
	read(t, c)
	write(t, c, map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"trade_updates"}}})
	read(t, c) // listening
	if code := closeCode(t, c); code != CloseSlowConsumer {
		t.Errorf("closed with %d, want 4008", code)
	}
}

// ── routing ──────────────────────────────────────────────────────────────────

func TestUpdateGoesOnlyToThatAccountsConnections(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	alice := f.login(t, 1)
	bob := f.login(t, 2)

	f.srv.OnUpdate(fillUpdate(1, 42))
	m := read(t, alice)
	if m.Stream != "trade_updates" {
		t.Fatalf("alice got %+v", m)
	}
	expectNothing(t, bob, "bob must not see alice's update")

	f.srv.OnUpdate(fillUpdate(77, 1)) // nobody here: dropped, no panic
	expectNothing(t, alice, "an update for another account")
}

func TestTwoConnectionsOfOneAccountBothReceive(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	a1 := f.login(t, 1)
	a2 := f.login(t, 1)
	f.srv.OnUpdate(fillUpdate(1, 42))
	for i, c := range []*client{a1, a2} {
		if m := read(t, c); m.Stream != "trade_updates" || !strings.Contains(string(m.Data), `"account_seq":42`) {
			t.Errorf("connection %d got %+v", i, m)
		}
	}
}

func TestUpdatesAreDeliveredInOrderWithoutMerging(t *testing.T) {
	f := newFixture(t, 64, time.Second)
	c := f.login(t, 1)
	for seq := uint64(10); seq < 30; seq++ {
		f.srv.OnUpdate(fillUpdate(1, seq))
	}
	for seq := 10; seq < 30; seq++ {
		m := read(t, c)
		if !strings.Contains(string(m.Data), `"account_seq":`+strconv.Itoa(seq)+`,`) {
			t.Fatalf("expected seq %d, got %s", seq, m.Data)
		}
	}
}

func TestDisconnectedConnectionGetsNothingAndNothingLeaks(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	c := f.login(t, 1)
	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(2 * time.Second)
	for len(f.srv.connsOf(1)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("connection still registered after the client left")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.srv.OnUpdate(fillUpdate(1, 1)) // must not panic or block
}

func TestFillFrameShape(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	c := f.login(t, 1)
	f.srv.OnUpdate(fillUpdate(1, 42))
	m := read(t, c)
	var d map[string]any
	if err := json.Unmarshal(m.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d["event"] != "partial_fill" || d["account_seq"] != float64(42) || d["timestamp"] != "2026-10-17T08:30:00.123Z" ||
		d["price"] != "98.00" || d["qty"] != "6" {
		t.Errorf("top level: %v", d)
	}
	order := d["order"].(map[string]any)
	if order["order_id"] != "1-812" || order["symbol"] != "MOOG" || order["filled_qty"] != "6" || order["filled_avg_price"] != "98.00" || order["status"] != "partially_filled" {
		t.Errorf("order: %v", order)
	}
	acct := d["account"].(map[string]any)
	if acct["cash"] != "12.00" || acct["cash_free"] != "10.00" || acct["cash_locked"] != "2.00" || acct["realized_pnl"] != "0.05" {
		t.Errorf("account: %v", acct)
	}
	pos := d["position"].(map[string]any) // the position of the order's symbol, not the other changed one
	if pos["symbol"] != "MOOG" || pos["qty"] != "6" || pos["avg_entry_price"] != "98.00" {
		t.Errorf("position: %v", pos)
	}
	if _, has := d["transfer"]; has {
		t.Error("a fill has no transfer")
	}
}

func TestEveryOrderEventKindHasItsContractName(t *testing.T) {
	want := map[core.UpdateKind]string{
		core.UpdateNew: "new", core.UpdatePartialFill: "partial_fill", core.UpdateFill: "fill", core.UpdateCanceled: "canceled",
		core.UpdateCancelRejected: "cancel_rejected", core.UpdateRejected: "rejected", core.UpdateExpired: "expired",
		core.UpdateReplaced: "replaced", core.UpdateReplaceRejected: "replace_rejected", core.UpdateTransfer: "transfer",
	}
	for k, name := range want {
		if got := eventName(k); got != name {
			t.Errorf("kind %d -> %q, want %q", k, got, name)
		}
	}
}

func TestTransferFrameHasNoOrderAndNoPrice(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	c := f.login(t, 1)
	f.srv.OnUpdate(core.AccountUpdate{Account: 1, Seq: 43, TsMs: 1_792_225_800_123, Kind: core.UpdateTransfer,
		Transfer: &core.Transfer{ID: "t-1", Deposit: false, AmountMinor: 2550}, Balances: core.Balances{CashFreeMinor: 100}})
	var d map[string]any
	_ = json.Unmarshal(read(t, c).Data, &d)
	tr, _ := d["transfer"].(map[string]any)
	if d["event"] != "transfer" || tr["transfer_id"] != "t-1" || tr["kind"] != "withdraw" || tr["amount"] != "25.50" {
		t.Errorf("got %v", d)
	}
	for _, k := range []string{"order", "price", "qty", "position"} {
		if _, has := d[k]; has {
			t.Errorf("a transfer must not carry %q", k)
		}
	}
}

// ── account-updates stream reconnect ─────────────────────────────────────────

func TestOnLiveResendsASnapshotToEveryConnectedUser(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	f.cash.set(1, snap(1, 41, 1000))
	f.cash.set(2, snap(2, 7, 2000))
	a1 := f.login(t, 1)
	a1b := f.login(t, 1)
	b := f.login(t, 2)

	f.cash.set(1, snap(1, 50, 1000))
	f.cash.set(2, snap(2, 9, 2000))
	f.srv.OnLive() // the account-updates stream just (re)connected

	for name, c := range map[string]*client{"alice-1": a1, "alice-2": a1b} {
		if m := read(t, c); m.Stream != "account_snapshot" || !strings.Contains(string(m.Data), `"account_seq":50`) {
			t.Errorf("%s got %s %s", name, m.Stream, m.Data)
		}
	}
	if m := read(t, b); m.Stream != "account_snapshot" || !strings.Contains(string(m.Data), `"account_seq":9`) {
		t.Errorf("bob got %s %s", m.Stream, m.Data)
	}
	if n := f.cash.callsFor(1); n != 3 { // login + a1b's login + one shared re-snapshot
		t.Errorf("account 1 snapshots fetched %d times, want 3 (one per login + one per account on reconnect)", n)
	}
}

func TestOnLiveWithNobodyConnectedDoesNothing(t *testing.T) {
	f := newFixture(t, 8, time.Second)
	f.srv.OnLive()
	time.Sleep(20 * time.Millisecond)
	if n := f.cash.callsFor(1); n != 0 {
		t.Errorf("fetched %d snapshots with nobody connected", n)
	}
}

// ── slow consumer ────────────────────────────────────────────────────────────

func TestSlowConsumerIsDisconnectedWith4008(t *testing.T) {
	f := newFixture(t, 2, time.Second)
	c := f.login(t, 1)
	_ = c

	// The client stops reading. Big frames fill the socket buffers, then the
	// 2-slot outbox, and the hub drops the connection instead of waiting.
	big := fillUpdate(1, 1)
	big.Kind = core.UpdateTransfer
	big.Order = nil
	big.Transfer = &core.Transfer{ID: strings.Repeat("x", 1<<20), AmountMinor: 1}
	for i := 0; i < 80; i++ {
		f.srv.OnUpdate(big)
	}
	if code := closeCode(t, c); code != CloseSlowConsumer {
		t.Fatalf("closed with %d, want 4008", code)
	}
	if len(f.srv.connsOf(1)) != 0 {
		// the read loop unregisters right after the close; give it a moment
		time.Sleep(100 * time.Millisecond)
		if len(f.srv.connsOf(1)) != 0 {
			t.Error("slow connection still registered")
		}
	}
}

func TestUpdateHelpersDoNotNeedAHub(t *testing.T) {
	// frames are plain functions of the update; a nil ticker falls back to the symbol id
	b := updateFrame(fillUpdate(1, 1), nil)
	if !strings.Contains(string(b), `"symbol":"1"`) {
		t.Errorf("got %s", b)
	}
	var _ = errors.New
}

// ── updates arriving while a snapshot is in flight ───────────────────────────

// gate makes every GetAccount block (after reading the snapshot) until release().
type gate struct {
	entered chan struct{}
	open    chan struct{}
}

func newGate(f *fixture) *gate {
	g := &gate{entered: make(chan struct{}, 8), open: make(chan struct{})}
	f.cash.setHook(func() {
		g.entered <- struct{}{}
		<-g.open
	})
	return g
}

func (g *gate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("GetAccount was never called")
	}
}

func (g *gate) release() { close(g.open) }

func expectSeq(t *testing.T, c *client, stream string, seq int) {
	t.Helper()
	m := read(t, c)
	if m.Stream != stream || !strings.Contains(string(m.Data), `"account_seq":`+strconv.Itoa(seq)+`,`) {
		t.Fatalf("want %s seq %d, got %s %s", stream, seq, m.Stream, m.Data)
	}
}

// The snapshot says 41; 41, 42, 43 arrive while it is being fetched.
func deliverWhileBlocked(f *fixture) {
	f.srv.OnUpdate(fillUpdate(1, 41))
	f.srv.OnUpdate(fillUpdate(1, 42))
	f.srv.OnUpdate(fillUpdate(1, 43))
}

func TestListen_UpdatesDuringTheSnapshotArriveAfterItInOrder(t *testing.T) {
	f := newFixture(t, 16, time.Second)
	f.cash.set(1, snap(1, 41, 1000))
	g := newGate(f)
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "key_id": "PK1", "secret": "ok"})
	read(t, c)
	write(t, c, map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"trade_updates"}}})
	if m := read(t, c); m.Stream != "listening" {
		t.Fatalf("got %+v", m)
	}
	g.waitEntered(t)
	deliverWhileBlocked(f)
	expectNothing(t, c, "nothing may reach the client before its snapshot")
	g.release()
	expectSeq(t, c, "account_snapshot", 41)
	expectSeq(t, c, "trade_updates", 42)
	expectSeq(t, c, "trade_updates", 43)
	expectNothing(t, c, "update 41 is older than the snapshot and must be dropped")

	f.srv.OnUpdate(fillUpdate(1, 44)) // holding is over: live again
	expectSeq(t, c, "trade_updates", 44)
}

func TestResync_UpdatesDuringTheSnapshotArriveAfterItInOrder(t *testing.T) {
	f := newFixture(t, 16, time.Second)
	f.cash.set(1, snap(1, 30, 1000))
	c := f.login(t, 1)
	f.cash.set(1, snap(1, 41, 1000))
	g := newGate(f)
	write(t, c, map[string]any{"action": "resync"})
	g.waitEntered(t)
	deliverWhileBlocked(f)
	expectNothing(t, c, "held while the resync snapshot is in flight")
	g.release()
	expectSeq(t, c, "account_snapshot", 41)
	expectSeq(t, c, "trade_updates", 42)
	expectSeq(t, c, "trade_updates", 43)
}

func TestOnLive_UpdatesDuringTheSnapshotArriveAfterItInOrder(t *testing.T) {
	f := newFixture(t, 16, time.Second)
	f.cash.set(1, snap(1, 30, 1000))
	c := f.login(t, 1)
	f.cash.set(1, snap(1, 41, 1000))
	g := newGate(f)
	f.srv.OnLive()
	g.waitEntered(t)
	deliverWhileBlocked(f)
	expectNothing(t, c, "held while the reconnect snapshot is in flight")
	g.release()
	expectSeq(t, c, "account_snapshot", 41)
	expectSeq(t, c, "trade_updates", 42)
	expectSeq(t, c, "trade_updates", 43)
}

func TestHoldListOverflowClosesWith4008(t *testing.T) {
	f := newFixture(t, 16, time.Second)
	f.srv.MaxHeld = 3
	f.cash.set(1, snap(1, 41, 1000))
	g := newGate(f)
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "key_id": "PK1", "secret": "ok"})
	read(t, c)
	write(t, c, map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"trade_updates"}}})
	g.waitEntered(t)
	for seq := uint64(42); seq < 46; seq++ { // the 4th does not fit
		f.srv.OnUpdate(fillUpdate(1, seq))
	}
	if code := closeCode(t, c); code != CloseSlowConsumer {
		t.Fatalf("closed with %d, want 4008", code)
	}
	g.release()
}

func TestSnapshotFailureSendsNoUpdatesAndCloses4008(t *testing.T) {
	f := newFixture(t, 16, time.Second)
	f.cash.err = core.ErrNoAnswer
	g := newGate(f)
	c := f.dial(t)
	write(t, c, map[string]any{"action": "auth", "key_id": "PK1", "secret": "ok"})
	read(t, c)
	write(t, c, map[string]any{"action": "listen", "data": map[string]any{"streams": []string{"trade_updates"}}})
	g.waitEntered(t)
	deliverWhileBlocked(f)
	g.release()
	for {
		select {
		case m := <-c.frames:
			if m.Stream == "trade_updates" || m.Stream == "account_snapshot" {
				t.Fatalf("a client without a snapshot got %s", m.Stream)
			}
			continue
		case code := <-c.ended:
			if code != CloseSlowConsumer {
				t.Fatalf("closed with %d, want 4008", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("not closed")
		}
		break
	}
}

// Resync while an OnLive snapshot is in flight: the later snapshot wins, the older result is discarded.
func TestLaterSnapshotSupersedesAnOlderOneInFlight(t *testing.T) {
	f := newFixture(t, 16, time.Second)
	f.cash.set(1, snap(1, 30, 1000))
	c := f.login(t, 1)

	f.cash.set(1, snap(1, 40, 1000))
	g1 := newGate(f)
	f.srv.OnLive() // snapshot A (seq 40) blocks
	g1.waitEntered(t)

	f.cash.set(1, snap(1, 41, 1000))
	g2 := newGate(f)
	write(t, c, map[string]any{"action": "resync"}) // snapshot B (seq 41) blocks too
	g2.waitEntered(t)
	f.srv.OnUpdate(fillUpdate(1, 42))

	g2.release()
	expectSeq(t, c, "account_snapshot", 41)
	expectSeq(t, c, "trade_updates", 42)
	g1.release() // the older result finally returns
	expectNothing(t, c, "the superseded snapshot must be discarded")
}
