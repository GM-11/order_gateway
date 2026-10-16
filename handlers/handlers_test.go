package handlers

import (
	"bytes"
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

// ── fakes ────────────────────────────────────────────────────────────────────

type mockVerifier struct {
	principal auth.Principal
	err       error
}

func (m *mockVerifier) Verify(cred auth.Credentials) (auth.Principal, error) {
	return m.principal, m.err
}

type mockSymbols struct {
	instruments map[string]refdata.Instrument
}

func (m *mockSymbols) Lookup(ticker string) (refdata.Instrument, bool) {
	inst, ok := m.instruments[ticker]
	return inst, ok
}

type mockPlacer struct {
	lastReq *core.OrderRequest
	result  core.Result
	err     error
}

func (m *mockPlacer) Place(ctx context.Context, req core.OrderRequest) (core.Result, error) {
	m.lastReq = &req
	return m.result, m.err
}

// ── helpers ───────────────────────────────────────────────────────────────────

// newOrders wires up an Orders handler with the account ID set to 1 and MOOG in
// the symbol table. Pass a non-nil placer to override the default noop placer.
func newOrders(placer *mockPlacer) (*Orders, *mockPlacer) {
	if placer == nil {
		placer = &mockPlacer{}
	}
	return &Orders{
		Auth: &mockVerifier{principal: auth.Principal{Account: core.AccountID(1)}},
		Symbols: &mockSymbols{
			instruments: map[string]refdata.Instrument{
				"MOOG": {ID: core.SymbolID(1)},
			},
		},
		Placer: placer,
	}, placer
}

// validBody returns a minimal valid JSON body as a byte slice.
func validBody() []byte {
	b, _ := json.Marshal(placeOrderRequest{
		ClientOrderID: "ord-1",
		Symbol:        "MOOG",
		Side:          "buy",
		Type:          "limit",
		TimeInForce:   "gtc",
		Price:         "101.50",
		Qty:           "10",
	})
	return b
}

func post(handler http.Handler, body []byte, ct string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/orders", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// ── auth ──────────────────────────────────────────────────────────────────────

func TestAuth_NotReady(t *testing.T) {
	placer := &mockPlacer{}
	h := &Orders{
		Auth:    &mockVerifier{err: auth.ErrNotReady},
		Symbols: &mockSymbols{},
		Placer:  placer,
	}
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	if placer.lastReq != nil {
		t.Error("placer must not be called on ErrNotReady")
	}
}

func TestAuth_BadCredentials(t *testing.T) {
	placer := &mockPlacer{}
	h := &Orders{
		Auth:    &mockVerifier{err: errors.New("bad key")},
		Symbols: &mockSymbols{},
		Placer:  placer,
	}
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if placer.lastReq != nil {
		t.Error("placer must not be called on auth error")
	}
}

// ── decode layer ──────────────────────────────────────────────────────────────

func TestDecode_WrongContentType(t *testing.T) {
	h, placer := newOrders(nil)
	w := post(h, validBody(), "text/plain")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415, got %d", w.Code)
	}
	if placer.lastReq != nil {
		t.Error("placer must not be called")
	}
}

func TestDecode_ContentTypeWithCharset(t *testing.T) {
	// application/json; charset=utf-8 is a legal Content-Type and must be accepted
	h, placer := newOrders(&mockPlacer{result: core.Result{Outcome: core.OutcomePlaced}})
	w := post(h, validBody(), "application/json; charset=utf-8")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if placer.lastReq == nil {
		t.Error("placer should have been called")
	}
}

func TestDecode_BodyTooLarge(t *testing.T) {
	h, placer := newOrders(nil)
	large := `{"client_order_id":"` + strings.Repeat("x", 5000) + `"}`
	w := post(h, []byte(large), "application/json")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", w.Code)
	}
	if placer.lastReq != nil {
		t.Error("placer must not be called")
	}
}

func TestDecode_TrustBoundary_AccountIDInBody(t *testing.T) {
	// account_id in the body must be rejected (unknown field); the real account
	// comes only from the verified principal, never from the request body.
	h, placer := newOrders(nil)
	body := `{"client_order_id":"ord-1","symbol":"MOOG","side":"buy","type":"limit",` +
		`"time_in_force":"gtc","price":"101.50","qty":"10","account_id":999}`
	w := post(h, []byte(body), "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if placer.lastReq != nil {
		t.Error("placer must not be called")
	}
}

func TestDecode_TrailingObject(t *testing.T) {
	h, placer := newOrders(nil)
	w := post(h, []byte(`{}{}`), "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if placer.lastReq != nil {
		t.Error("placer must not be called")
	}
}

func TestDecode_NumericQty(t *testing.T) {
	// Clients that send qty as a JSON number (10 not "10") get a clear 400.
	// The field is a string so the decoder returns a type-mismatch error.
	h, placer := newOrders(nil)
	body := `{"client_order_id":"ord-1","symbol":"MOOG","side":"buy","type":"limit",` +
		`"time_in_force":"gtc","price":"101.50","qty":10}`
	w := post(h, []byte(body), "application/json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (qty must be a string), got %d", w.Code)
	}
	if placer.lastReq != nil {
		t.Error("placer must not be called")
	}
}

// ── field validation (422) ────────────────────────────────────────────────────

func TestValidation_InvalidFields(t *testing.T) {
	base := placeOrderRequest{
		ClientOrderID: "ord-1",
		Symbol:        "MOOG",
		Side:          "buy",
		Type:          "limit",
		TimeInForce:   "gtc",
		Price:         "101.50",
		Qty:           "10",
	}

	tests := []struct {
		name  string
		patch func(*placeOrderRequest)
		field string
	}{
		{"empty client_order_id", func(r *placeOrderRequest) { r.ClientOrderID = "" }, "client_order_id"},
		{"space in client_order_id", func(r *placeOrderRequest) { r.ClientOrderID = "a b" }, "client_order_id"},
		{"non-ascii client_order_id", func(r *placeOrderRequest) { r.ClientOrderID = "é" }, "client_order_id"},
		{"unknown symbol", func(r *placeOrderRequest) { r.Symbol = "NOPE" }, "symbol"},
		{"uppercase side BUY", func(r *placeOrderRequest) { r.Side = "BUY" }, "side"},
		{"invalid side hold", func(r *placeOrderRequest) { r.Side = "hold" }, "side"},
		{"type market", func(r *placeOrderRequest) { r.Type = "market" }, "type"},
		{"time_in_force ioc", func(r *placeOrderRequest) { r.TimeInForce = "ioc" }, "time_in_force"},
		{"price 0.00", func(r *placeOrderRequest) { r.Price = "0.00" }, "price"},
		{"price negative", func(r *placeOrderRequest) { r.Price = "-1.00" }, "price"},
		{"price 3 decimals", func(r *placeOrderRequest) { r.Price = "101.505" }, "price"},
		{"qty 0", func(r *placeOrderRequest) { r.Qty = "0" }, "qty"},
		{"qty negative", func(r *placeOrderRequest) { r.Qty = "-1" }, "qty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, placer := newOrders(nil)
			req := base
			tt.patch(&req)
			body, _ := json.Marshal(req)
			w := post(h, body, "application/json")

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422, got %d", w.Code)
			}
			var resp errorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Field != tt.field {
				t.Errorf("want field=%q, got %q", tt.field, resp.Field)
			}
			if placer.lastReq != nil {
				t.Error("placer must not be called on validation error")
			}
		})
	}
}

// ── happy paths ───────────────────────────────────────────────────────────────

func TestHappyPath_Placed(t *testing.T) {
	placer := &mockPlacer{result: core.Result{Outcome: core.OutcomePlaced, OrderID: 999}}
	h := &Orders{
		Auth: &mockVerifier{principal: auth.Principal{Account: core.AccountID(42)}},
		Symbols: &mockSymbols{
			instruments: map[string]refdata.Instrument{"MOOG": {ID: core.SymbolID(1)}},
		},
		Placer: placer,
	}

	w := post(h, validBody(), "application/json")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp orderResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "new" {
		t.Errorf("want status=new, got %q", resp.Status)
	}
	if resp.OrderID != "999" {
		t.Errorf("want order_id=999, got %q", resp.OrderID)
	}
	if resp.ClientOrderID != "ord-1" {
		t.Errorf("want client_order_id=ord-1, got %q", resp.ClientOrderID)
	}
	if placer.lastReq == nil {
		t.Fatal("placer was not called")
	}
	// Account must come from the principal, never from the body.
	if placer.lastReq.AccountID != core.AccountID(42) {
		t.Errorf("want AccountID=42 (from principal), got %d", placer.lastReq.AccountID)
	}
}

func TestHappyPath_EngineRejected(t *testing.T) {
	placer := &mockPlacer{result: core.Result{Outcome: core.OutcomeEngineRejected, OrderID: 999}}
	h, _ := newOrders(placer)
	w := post(h, validBody(), "application/json")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp orderResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Status != "rejected" {
		t.Errorf("want status=rejected, got %q", resp.Status)
	}
}

// ── placer error paths ────────────────────────────────────────────────────────

func TestPlacer_DuplicateInFlight(t *testing.T) {
	h, _ := newOrders(&mockPlacer{err: core.ErrDuplicateInFlight})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", w.Code)
	}
	var resp errorResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Error != "duplicate_in_flight" {
		t.Errorf("want error=duplicate_in_flight, got %q", resp.Error)
	}
}

func TestPlacer_NoAnswer(t *testing.T) {
	h, _ := newOrders(&mockPlacer{err: core.ErrNoAnswer})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d", w.Code)
	}
	var resp errorResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Error != "outcome_unknown" {
		t.Errorf("want error=outcome_unknown, got %q", resp.Error)
	}
}

func TestPlacer_UnexpectedError(t *testing.T) {
	h, _ := newOrders(&mockPlacer{err: errors.New("database exploded")})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	var resp errorResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Error != "internal" {
		t.Errorf("want error=internal, got %q", resp.Error)
	}
}

func TestPlacer_UnmappedOutcome(t *testing.T) {
	// If a new Outcome value is added to core but not yet handled here,
	// the handler must return 500 rather than panic or silently succeed.
	h, _ := newOrders(&mockPlacer{result: core.Result{Outcome: core.Outcome(99)}})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

// ── cash rejection ────────────────────────────────────────────────────────────

func TestCash_InsufficientCash(t *testing.T) {
	h, _ := newOrders(&mockPlacer{result: core.Result{
		Outcome:        core.OutcomeCashRejected,
		Reason:         core.ReasonInsufficientCash,
		ShortfallMinor: 1250, // $12.50
	}})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	var resp errorResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Error != "insufficient_cash" {
		t.Errorf("want error=insufficient_cash, got %q", resp.Error)
	}
	if resp.Shortfall != "12.50" {
		t.Errorf("want shortfall=12.50, got %q", resp.Shortfall)
	}
}

func TestCash_InsufficientHoldings(t *testing.T) {
	h, _ := newOrders(&mockPlacer{result: core.Result{
		Outcome: core.OutcomeCashRejected,
		Reason:  core.ReasonInsufficientHoldings,
	}})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	var resp errorResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Error != "insufficient_holdings" {
		t.Errorf("want error=insufficient_holdings, got %q", resp.Error)
	}
	if resp.Shortfall != "" {
		t.Errorf("shortfall must be absent for holdings rejection, got %q", resp.Shortfall)
	}
}

func TestOrdersHandler_FeedNotLive(t *testing.T) {
	placer := &mockPlacer{result: core.Result{Outcome: core.OutcomePlaced, OrderID: 1}}
	handler := &Orders{
		Auth: &mockVerifier{principal: auth.Principal{Account: core.AccountID(123)}},
		Symbols: &mockSymbols{
			instruments: map[string]refdata.Instrument{"AAPL": {ID: core.SymbolID(1)}},
		},
		Placer:   placer,
		FeedLive: func() bool { return false },
	}

	body, _ := json.Marshal(placeOrderRequest{
		ClientOrderID: "order-1", Symbol: "AAPL", Side: "buy", Type: "limit",
		TimeInForce: "gtc", Price: "101.50", Qty: "10",
	})
	httpReq := httptest.NewRequest("POST", "/orders", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}
	var resp errorResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != "engine_unavailable" {
		t.Errorf("expected error 'engine_unavailable', got %q", resp.Error)
	}
	if placer.lastReq != nil {
		t.Error("expected placer not to be called")
	}
}
