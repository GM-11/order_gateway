package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"order_gateway/auth"
	"order_gateway/core"
	"order_gateway/refdata"
	"strings"
	"testing"
)

type fakeCash struct {
	snap      core.AccountSnapshot
	snapErr   error
	orders    []core.OrderState
	ordersErr error
	cancel    core.CancelAck
	cancelErr error

	gotQuery  *core.OrdersQuery
	gotCancel *core.CancelRequest
	gotAcct   *core.AccountID
}

func (f *fakeCash) GetAccount(_ context.Context, a core.AccountID) (core.AccountSnapshot, error) {
	f.gotAcct = &a
	return f.snap, f.snapErr
}
func (f *fakeCash) GetOrders(_ context.Context, q core.OrdersQuery) ([]core.OrderState, error) {
	f.gotQuery = &q
	return f.orders, f.ordersErr
}
func (f *fakeCash) CancelOrder(_ context.Context, r core.CancelRequest) (core.CancelAck, error) {
	f.gotCancel = &r
	return f.cancel, f.cancelErr
}

func newTrading(c *fakeCash) *Trading {
	return &Trading{
		Auth: &mockVerifier{principal: auth.Principal{Account: 17}},
		Symbols: &mockSymbols{instruments: map[string]refdata.Instrument{
			"MOOG": {ID: 1}, "BANANA": {ID: 2},
		}},
		Cash: c,
	}
}

func call(h http.HandlerFunc, method, target string, pathValues map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var e errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body: %s", w.Body)
	}
	return e
}

func sampleOrder() core.OrderState {
	return core.OrderState{
		Account: 17, ClientOrderID: "abc", Symbol: 1, OrderID: 812, Side: core.Buy, Type: core.TypeLimit,
		TimeInForce: core.TIFGoodTilCancelled, PriceMinor: 10000, Qty: 10, Status: core.StatusNew,
		CreatedTs: fixedNow.UnixMilli(), UpdatedTs: fixedNow.UnixMilli(),
	}
}

// ── every endpoint: authentication ───────────────────────────────────────────

func TestTrading_EveryEndpointRequiresAuth(t *testing.T) {
	tr := newTrading(&fakeCash{})
	tr.Auth = &mockVerifier{err: errors.New("nope")}
	cases := []struct {
		h      http.HandlerFunc
		method string
		target string
	}{
		{tr.ListOrders, "GET", "/v2/orders"},
		{tr.GetOrder, "GET", "/v2/orders/1-2"},
		{tr.GetOrderByClientOrderID, "GET", "/v2/orders:by_client_order_id?client_order_id=a"},
		{tr.CancelOrder, "DELETE", "/v2/orders/1-2"},
		{tr.NotSupportedYet, "PATCH", "/v2/orders/1-2"},
		{tr.NotSupportedYet, "DELETE", "/v2/orders"},
		{tr.Account, "GET", "/v2/account"},
		{tr.Positions, "GET", "/v2/positions"},
		{tr.Position, "GET", "/v2/positions/MOOG"},
	}
	for _, c := range cases {
		w := call(c.h, c.method, c.target, map[string]string{"order_id": "1-2", "symbol": "MOOG"})
		if w.Code != http.StatusUnauthorized || errCode(t, w).Error != "unauthorized" {
			t.Errorf("%s %s: got %d %s, want 401 unauthorized", c.method, c.target, w.Code, w.Body)
		}
	}
	tr.Auth = &mockVerifier{err: auth.ErrNotReady}
	if w := call(tr.Account, "GET", "/v2/account", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("auth not ready: got %d, want 503", w.Code)
	}
}

// ── GET /v2/orders ───────────────────────────────────────────────────────────

func TestListOrders_StatusFilter(t *testing.T) {
	cases := map[string]core.OrdersFilter{"": core.FilterOpen, "open": core.FilterOpen, "closed": core.FilterClosed, "all": core.FilterAll}
	for status, want := range cases {
		c := &fakeCash{orders: []core.OrderState{sampleOrder()}}
		w := call(newTrading(c).ListOrders, "GET", "/v2/orders?status="+status, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%q: got %d", status, w.Code)
		}
		if c.gotQuery.Filter != want || c.gotQuery.Account != 17 {
			t.Errorf("status=%q: query = %+v", status, c.gotQuery)
		}
		var list []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0]["order_id"] != "1-812" {
			t.Errorf("status=%q: body = %s", status, w.Body)
		}
	}
}

func TestListOrders_EmptyIsAnEmptyArray(t *testing.T) {
	w := call(newTrading(&fakeCash{}).ListOrders, "GET", "/v2/orders", nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("got %d %q, want 200 []", w.Code, w.Body)
	}
}

func TestListOrders_BadStatusIs422(t *testing.T) {
	c := &fakeCash{}
	w := call(newTrading(c).ListOrders, "GET", "/v2/orders?status=pending", nil)
	if e := errCode(t, w); w.Code != 422 || e.Error != "invalid_field" || e.Field != "status" {
		t.Errorf("got %d %+v", w.Code, e)
	}
	if c.gotQuery != nil {
		t.Error("cash owner must not be asked")
	}
}

func TestListOrders_CashOwnerDown(t *testing.T) {
	w := call(newTrading(&fakeCash{ordersErr: core.ErrNoAnswer}).ListOrders, "GET", "/v2/orders", nil)
	if e := errCode(t, w); w.Code != 503 || e.Error != "cash_owner_unavailable" {
		t.Errorf("got %d %+v", w.Code, e)
	}
}

// ── GET /v2/orders/{id} and by client id ─────────────────────────────────────

func TestGetOrder(t *testing.T) {
	c := &fakeCash{orders: []core.OrderState{sampleOrder()}}
	w := call(newTrading(c).GetOrder, "GET", "/v2/orders/1-812", map[string]string{"order_id": "1-812"})
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	q := c.gotQuery
	if q.Account != 17 || q.Order == nil || q.Order.Symbol != 1 || q.Order.OrderID != 812 {
		t.Errorf("query = %+v", q)
	}
}

func TestGetOrder_NotFoundCases(t *testing.T) {
	for _, id := range []string{"812", "x-1", "1-", "01-5", "1-812-3", ""} {
		c := &fakeCash{orders: []core.OrderState{sampleOrder()}}
		w := call(newTrading(c).GetOrder, "GET", "/v2/orders/"+id, map[string]string{"order_id": id})
		if e := errCode(t, w); w.Code != 404 || e.Error != "order_not_found" {
			t.Errorf("malformed id %q: got %d %+v, want 404 order_not_found (opaque id, not a 422)", id, w.Code, e)
		}
		if c.gotQuery != nil {
			t.Errorf("malformed id %q reached the cash owner", id)
		}
	}
	w := call(newTrading(&fakeCash{}).GetOrder, "GET", "/v2/orders/1-5", map[string]string{"order_id": "1-5"})
	if e := errCode(t, w); w.Code != 404 || e.Error != "order_not_found" {
		t.Errorf("unknown id: got %d %+v", w.Code, e)
	}
}

func TestGetOrder_CashOwnerDown(t *testing.T) {
	w := call(newTrading(&fakeCash{ordersErr: core.ErrNoAnswer}).GetOrder, "GET", "/v2/orders/1-5", map[string]string{"order_id": "1-5"})
	if w.Code != 503 {
		t.Errorf("got %d, want 503", w.Code)
	}
}

func TestGetOrderByClientOrderID(t *testing.T) {
	c := &fakeCash{orders: []core.OrderState{sampleOrder()}}
	tr := newTrading(c)
	w := call(tr.GetOrderByClientOrderID, "GET", "/v2/orders:by_client_order_id?client_order_id=abc", nil)
	if w.Code != 200 || c.gotQuery.ClientOrderID != "abc" || c.gotQuery.Account != 17 {
		t.Fatalf("got %d, query %+v", w.Code, c.gotQuery)
	}

	w = call(newTrading(&fakeCash{}).GetOrderByClientOrderID, "GET", "/v2/orders:by_client_order_id?client_order_id=abc", nil)
	if e := errCode(t, w); w.Code != 404 || e.Error != "order_not_found" {
		t.Errorf("unknown coid: got %d %+v", w.Code, e)
	}
	for _, target := range []string{"/v2/orders:by_client_order_id", "/v2/orders:by_client_order_id?client_order_id=a%20b"} {
		w = call(tr.GetOrderByClientOrderID, "GET", target, nil)
		if e := errCode(t, w); w.Code != 422 || e.Field != "client_order_id" {
			t.Errorf("%s: got %d %+v, want 422 on client_order_id", target, w.Code, e)
		}
	}
}

// ── DELETE /v2/orders/{id} ───────────────────────────────────────────────────

func TestCancelOrder_Accepted(t *testing.T) {
	o := sampleOrder()
	c := &fakeCash{cancel: core.CancelAck{Accepted: true, Order: &o}}
	w := call(newTrading(c).CancelOrder, "DELETE", "/v2/orders/1-812", map[string]string{"order_id": "1-812"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["status"] != "pending_cancel" || got["order_id"] != "1-812" {
		t.Errorf("body = %v", got)
	}
	if r := c.gotCancel; r.Account != 17 || r.Symbol != 1 || r.OrderID != 812 {
		t.Errorf("cancel request = %+v", r)
	}
}

func TestCancelOrder_Rejections(t *testing.T) {
	cases := []struct {
		reason core.CancelReason
		status int
		code   string
	}{
		{core.CancelReasonNotFound, 404, "order_not_found"},
		{core.CancelReasonClosed, 422, "order_closed"},
		{core.CancelReasonExchangeBusy, 503, "exchange_busy"},
		{core.CancelReasonExchangeUnavailable, 503, "engine_unavailable"},
		{core.CancelReasonUnknown, 500, "internal"},
		{core.CancelReasonNone, 500, "internal"},
	}
	for _, tc := range cases {
		c := &fakeCash{cancel: core.CancelAck{Accepted: false, Reason: tc.reason}}
		w := call(newTrading(c).CancelOrder, "DELETE", "/v2/orders/1-812", map[string]string{"order_id": "1-812"})
		if e := errCode(t, w); w.Code != tc.status || e.Error != tc.code {
			t.Errorf("reason %d: got %d %+v, want %d %s", tc.reason, w.Code, e, tc.status, tc.code)
		}
	}
}

func TestCancelOrder_NoAnswerIs504OutcomeUnknown(t *testing.T) {
	c := &fakeCash{cancelErr: core.ErrNoAnswer}
	w := call(newTrading(c).CancelOrder, "DELETE", "/v2/orders/1-812", map[string]string{"order_id": "1-812"})
	e := errCode(t, w)
	if w.Code != 504 || e.Error != "outcome_unknown" || !strings.Contains(e.Message, "cancel may or may not have been sent") {
		t.Errorf("got %d %+v", w.Code, e)
	}
}

func TestCancelOrder_MalformedIDIs404AndNeverSent(t *testing.T) {
	c := &fakeCash{}
	w := call(newTrading(c).CancelOrder, "DELETE", "/v2/orders/nope", map[string]string{"order_id": "nope"})
	if w.Code != 404 || c.gotCancel != nil {
		t.Errorf("got %d, cancel sent = %v", w.Code, c.gotCancel != nil)
	}
}

func TestCancelOrder_AcceptedWithoutOrderIsInternal(t *testing.T) {
	c := &fakeCash{cancel: core.CancelAck{Accepted: true}}
	w := call(newTrading(c).CancelOrder, "DELETE", "/v2/orders/1-812", map[string]string{"order_id": "1-812"})
	if w.Code != 500 {
		t.Errorf("got %d, want 500", w.Code)
	}
}

// ── [later] endpoints ────────────────────────────────────────────────────────

func TestLaterEndpointsAreNotSupportedYet(t *testing.T) {
	tr := newTrading(&fakeCash{})
	for _, c := range []struct{ method, target string }{{"PATCH", "/v2/orders/1-2"}, {"DELETE", "/v2/orders"}} {
		w := call(tr.NotSupportedYet, c.method, c.target, nil)
		if e := errCode(t, w); w.Code != 422 || e.Error != "not_supported_yet" || e.Field == "" {
			t.Errorf("%s %s: got %d %+v", c.method, c.target, w.Code, e)
		}
	}
}

// ── account and positions ────────────────────────────────────────────────────

func sampleSnapshot() core.AccountSnapshot {
	return core.AccountSnapshot{
		Found: true, Account: 17, Seq: 41,
		Balances: core.Balances{CashFreeMinor: 100050, CashLockedMinor: 2500, RealizedPnlMinor: -125},
		Positions: []core.Position{
			{Symbol: 1, FreeQty: 4, LockedQty: 6, AvgPriceMinor: 9800, RealizedPnlMinor: 300},
			{Symbol: 2, FreeQty: 0, LockedQty: 0, AvgPriceMinor: 0, RealizedPnlMinor: 50}, // closed
		},
	}
}

func TestAccount(t *testing.T) {
	c := &fakeCash{snap: sampleSnapshot()}
	w := call(newTrading(c).Account, "GET", "/v2/account", nil)
	if w.Code != 200 || *c.gotAcct != 17 {
		t.Fatalf("got %d, account %v", w.Code, c.gotAcct)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	want := map[string]any{"account_id": "17", "cash": "1025.50", "cash_free": "1000.50", "cash_locked": "25.00",
		"realized_pnl": "-1.25", "account_seq": float64(41)}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestAccountReads_NotFoundAndUnavailable(t *testing.T) {
	tr := newTrading(&fakeCash{snap: core.AccountSnapshot{Found: false}})
	for name, h := range map[string]http.HandlerFunc{"account": tr.Account, "positions": tr.Positions, "position": tr.Position} {
		w := call(h, "GET", "/x", map[string]string{"symbol": "MOOG"})
		if e := errCode(t, w); w.Code != 404 || e.Error != "account_not_found" {
			t.Errorf("%s: got %d %+v", name, w.Code, e)
		}
	}
	tr = newTrading(&fakeCash{snapErr: core.ErrNoAnswer})
	for name, h := range map[string]http.HandlerFunc{"account": tr.Account, "positions": tr.Positions, "position": tr.Position} {
		w := call(h, "GET", "/x", map[string]string{"symbol": "MOOG"})
		if e := errCode(t, w); w.Code != 503 || e.Error != "cash_owner_unavailable" {
			t.Errorf("%s: got %d %+v", name, w.Code, e)
		}
	}
}

func TestPositions(t *testing.T) {
	w := call(newTrading(&fakeCash{snap: sampleSnapshot()}).Positions, "GET", "/v2/positions", nil)
	var got []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || len(got) != 1 { // the closed BANANA position is not listed
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	want := map[string]any{"symbol": "MOOG", "qty": "10", "qty_free": "4", "qty_locked": "6", "avg_entry_price": "98.00", "realized_pnl": "3.00"}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("%s = %v, want %v", k, got[0][k], v)
		}
	}
}

func TestPositionBySymbol(t *testing.T) {
	tr := newTrading(&fakeCash{snap: sampleSnapshot()})
	w := call(tr.Position, "GET", "/v2/positions/MOOG", map[string]string{"symbol": "MOOG"})
	if w.Code != 200 {
		t.Errorf("held position: got %d", w.Code)
	}
	for _, sym := range []string{"BANANA", "NOPE"} { // zero position, unknown symbol
		w := call(tr.Position, "GET", "/v2/positions/"+sym, map[string]string{"symbol": sym})
		if e := errCode(t, w); w.Code != 404 || e.Error != "position_not_found" {
			t.Errorf("%s: got %d %+v", sym, w.Code, e)
		}
	}
}
