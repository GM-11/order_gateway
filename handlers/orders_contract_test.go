package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"order_gateway/auth"
	"order_gateway/core"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 17, 8, 30, 0, 123_000_000, time.UTC)

func ordersWith(placer *mockPlacer) *Orders {
	h, _ := newOrders(placer)
	h.Now = func() time.Time { return fixedNow }
	return h
}

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, w.Body.String())
	}
	return m
}

// POST /v2/orders answers with the full contract Order object, "as acknowledged".
func TestPost_ResponseIsTheContractOrder(t *testing.T) {
	h := ordersWith(&mockPlacer{result: core.Result{Outcome: core.OutcomePlaced, OrderID: 812}})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	got := decodeMap(t, w)
	want := map[string]any{
		"order_id": "1-812", "client_order_id": "ord-1", "symbol": "MOOG", "side": "buy", "type": "limit",
		"time_in_force": "gtc", "price": "101.50", "stop_price": nil, "qty": "10", "filled_qty": "0",
		"filled_avg_price": nil, "status": "new", "reject_reason": nil,
		"created_at": "2026-10-17T08:30:00.123Z", "updated_at": "2026-10-17T08:30:00.123Z",
	}
	if len(got) != len(want) {
		t.Errorf("got %d fields, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if gv, present := got[k]; !present || gv != v {
			t.Errorf("%s = %v (present=%v), want %v", k, gv, present, v)
		}
	}
}

func TestPost_EngineRejectReasonIsMappedOntoTheContractList(t *testing.T) {
	cases := map[core.EngineReject]string{
		core.EngineRejectInvalidPrice:     "invalid_price",
		core.EngineRejectInvalidQuantity:  "invalid_quantity",
		core.EngineRejectSelfTrade:        "self_trade",
		core.EngineRejectSymbolHalted:     "symbol_halted",
		core.EngineRejectPriceBand:        "price_band",
		core.EngineRejectPriceCollar:      "price_collar",
		core.EngineRejectStopWouldTrigger: "stop_would_trigger",
		core.EngineRejectOther:            "rejected",
		core.EngineRejectNone:             "rejected", // rejected with no reason we know: still "rejected"
	}
	for reject, name := range cases {
		h := ordersWith(&mockPlacer{result: core.Result{Outcome: core.OutcomeEngineRejected, OrderID: 7, EngineReject: reject}})
		w := post(h, validBody(), "application/json")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: want 200 (the engine said no; the order has an id), got %d", name, w.Code)
		}
		got := decodeMap(t, w)
		if got["status"] != "rejected" || got["reject_reason"] != name || got["order_id"] != "1-7" {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestPost_KnownButUnbuiltValuesAreNotSupportedYet(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*placeOrderRequest)
		field string
	}{
		{"market", func(r *placeOrderRequest) { r.Type = "market" }, "type"},
		{"stop", func(r *placeOrderRequest) { r.Type = "stop" }, "type"},
		{"stop_limit", func(r *placeOrderRequest) { r.Type = "stop_limit" }, "type"},
		{"ioc", func(r *placeOrderRequest) { r.TimeInForce = "ioc" }, "time_in_force"},
		{"day", func(r *placeOrderRequest) { r.TimeInForce = "day" }, "time_in_force"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, placer := newOrders(nil)
			req := placeOrderRequest{ClientOrderID: "ord-1", Symbol: "MOOG", Side: "buy", Type: "limit",
				TimeInForce: "gtc", Price: "101.50", Qty: "10"}
			tc.patch(&req)
			body, _ := json.Marshal(req)
			w := post(h, body, "application/json")
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422, got %d", w.Code)
			}
			var resp errorResponse
			_ = json.NewDecoder(w.Body).Decode(&resp)
			if resp.Error != "not_supported_yet" || resp.Field != tc.field {
				t.Errorf("got %+v, want not_supported_yet on %s", resp, tc.field)
			}
			if placer.lastReq != nil {
				t.Error("placer must not be called")
			}
		})
	}
}

func TestPost_CashOwnerRejectionMapping(t *testing.T) {
	cases := []struct {
		reason core.RejectReason
		status int
		code   string
		field  string
	}{
		{core.ReasonInsufficientCash, 403, "insufficient_cash", ""},
		{core.ReasonInsufficientHoldings, 403, "insufficient_holdings", ""},
		{core.ReasonUnknownAccount, 403, "account_not_found", ""},
		{core.ReasonExchangeBusy, 503, "exchange_busy", ""},
		{core.ReasonExchangeUnavailable, 503, "engine_unavailable", ""},
		{core.ReasonUnknownSymbol, 422, "invalid_field", "symbol"},
		{core.ReasonNotSupported, 422, "not_supported_yet", "type"},
		{core.ReasonUnknown, 500, "internal", ""},
		{core.ReasonNone, 500, "internal", ""},
		{core.RejectReason(200), 500, "internal", ""}, // a reason added later: never silently "rejected"
	}
	for _, tc := range cases {
		h, _ := newOrders(&mockPlacer{result: core.Result{Outcome: core.OutcomeCashRejected, Reason: tc.reason, ShortfallMinor: 1250}})
		w := post(h, validBody(), "application/json")
		var resp errorResponse
		_ = json.NewDecoder(w.Body).Decode(&resp)
		if w.Code != tc.status || resp.Error != tc.code || resp.Field != tc.field {
			t.Errorf("reason %d: got %d %+v, want %d %s field=%q", tc.reason, w.Code, resp, tc.status, tc.code, tc.field)
		}
		if tc.reason == core.ReasonInsufficientCash && resp.Shortfall != "12.50" {
			t.Errorf("shortfall = %q, want 12.50", resp.Shortfall)
		}
		if tc.reason != core.ReasonInsufficientCash && resp.Shortfall != "" {
			t.Errorf("reason %d: shortfall must only appear for insufficient_cash, got %q", tc.reason, resp.Shortfall)
		}
		if (tc.reason == core.ReasonExchangeBusy || tc.reason == core.ReasonExchangeUnavailable) &&
			!strings.Contains(resp.Message, "NOT placed") {
			t.Errorf("reason %d: message %q must say the order was not placed", tc.reason, resp.Message)
		}
	}
}

// A resend whose order the cash owner already knows is answered from that
// order, fills and all, without waiting for an engine reply.
func TestPost_DuplicateWithOrderIDAnswersFromTheKnownOrder(t *testing.T) {
	known := core.OrderState{
		Account: 1, ClientOrderID: "ord-1", Symbol: 1, OrderID: 812, Side: core.Buy, Type: core.TypeLimit,
		TimeInForce: core.TIFGoodTilCancelled, PriceMinor: 10150, Qty: 10, FilledQty: 6, FilledNotionalMinor: 5*9800 + 1*9801,
		Status: core.StatusPartiallyFilled, CreatedTs: fixedNow.UnixMilli(), UpdatedTs: fixedNow.UnixMilli() + 5,
	}
	h := ordersWith(&mockPlacer{result: core.Result{Outcome: core.OutcomeDuplicate, Order: &known}})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	got := decodeMap(t, w)
	if got["order_id"] != "1-812" || got["status"] != "partially_filled" || got["filled_qty"] != "6" {
		t.Errorf("got %v", got)
	}
	if got["filled_avg_price"] != "98.00" { // 58801 / 6 = 9800.17 paise-rounded DOWN to 9800
		t.Errorf("filled_avg_price = %v, want 98.00", got["filled_avg_price"])
	}
	if got["updated_at"] != "2026-10-17T08:30:00.128Z" {
		t.Errorf("updated_at = %v", got["updated_at"])
	}
}

func TestPost_DuplicateWithoutOrderIDIsDuplicateInFlight(t *testing.T) {
	// core turns "duplicate, order_id == 0" into ErrDuplicateInFlight; the handler maps it to 409.
	h, _ := newOrders(&mockPlacer{err: core.ErrDuplicateInFlight})
	w := post(h, validBody(), "application/json")
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", w.Code)
	}
}

func TestPost_ErrNoAnswerMessageTellsClientToReuseTheId(t *testing.T) {
	h, _ := newOrders(&mockPlacer{err: core.ErrNoAnswer})
	w := post(h, validBody(), "application/json")
	var resp errorResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if w.Code != http.StatusGatewayTimeout || !strings.Contains(resp.Message, "SAME client_order_id") {
		t.Errorf("got %d %+v", w.Code, resp)
	}
}

func TestPost_UsesNewHeadersViaVerifier(t *testing.T) {
	// The verifier sees the credentials parsed from X-Api-Key-Id / X-Api-Secret.
	var seen auth.Credentials
	h, _ := newOrders(&mockPlacer{result: core.Result{Outcome: core.OutcomePlaced, OrderID: 1}})
	h.Auth = verifierFunc(func(c auth.Credentials) (auth.Principal, error) {
		seen = c
		return auth.Principal{Account: 5}, nil
	})
	req := httptest.NewRequest("POST", "/v2/orders", strings.NewReader(string(validBody())))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key-Id", "PK1")
	req.Header.Set("X-Api-Secret", "s3cret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if seen.KeyID != "PK1" || seen.Secret != "s3cret" {
		t.Errorf("verifier saw %+v", seen)
	}
}

type verifierFunc func(auth.Credentials) (auth.Principal, error)

func (f verifierFunc) Verify(c auth.Credentials) (auth.Principal, error) { return f(c) }
