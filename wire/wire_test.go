package wire

import (
	"encoding/json"
	"testing"

	"order_gateway/core"
)

func ticker(id core.SymbolID) (string, bool) {
	if id == 1 {
		return "MOOG", true
	}
	return "", false
}

func TestTime(t *testing.T) {
	if got := Time(1_792_225_800_123); got != "2026-10-17T08:30:00.123Z" {
		t.Errorf("Time = %q", got)
	}
	if got := Time(1_792_225_800_000); got != "2026-10-17T08:30:00.000Z" {
		t.Errorf("Time = %q (milliseconds always shown)", got)
	}
}

func TestFilledAvgPriceRoundsDownAndIsNilBeforeAFill(t *testing.T) {
	if FilledAvgPrice(0, 0) != nil || FilledAvgPrice(100, 0) != nil {
		t.Error("no fills: want nil")
	}
	cases := []struct {
		notional, qty int64
		want          string
	}{{58800, 6, "98.00"}, {58801, 6, "98.00"}, {58805, 6, "98.00"}, {58806, 6, "98.01"}, {1, 1, "0.01"}}
	for _, c := range cases {
		if got := FilledAvgPrice(c.notional, c.qty); got == nil || *got != c.want {
			t.Errorf("%d/%d = %v, want %s", c.notional, c.qty, got, c.want)
		}
	}
}

func TestOrderJSONShape(t *testing.T) {
	stop := int64(9900)
	o := OrderFromState(core.OrderState{
		Symbol: 1, OrderID: 812, ClientOrderID: "abc", Side: core.Sell, Type: core.TypeStopLimit, TimeInForce: core.TIFDay,
		PriceMinor: 10000, StopPriceMinor: &stop, Qty: 10, Status: core.StatusPendingCancel, CreatedTs: 1_792_225_800_123, UpdatedTs: 1_792_225_800_456,
	}, ticker)
	b, _ := json.Marshal(o)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	want := map[string]any{
		"order_id": "1-812", "client_order_id": "abc", "symbol": "MOOG", "side": "sell", "type": "stop_limit",
		"time_in_force": "day", "price": "100.00", "stop_price": "99.00", "qty": "10", "filled_qty": "0",
		"filled_avg_price": nil, "status": "pending_cancel", "reject_reason": nil,
		"created_at": "2026-10-17T08:30:00.123Z", "updated_at": "2026-10-17T08:30:00.456Z",
	}
	if len(m) != len(want) {
		t.Errorf("%d fields, want %d: %s", len(m), len(want), b)
	}
	for k, v := range want {
		if gv, ok := m[k]; !ok || gv != v {
			t.Errorf("%s = %v (present %v), want %v", k, gv, ok, v)
		}
	}
}

func TestRejectReasonOnlyWhenRejectedCashBeforeEngine(t *testing.T) {
	live := OrderFromState(core.OrderState{Status: core.StatusNew, EngineReject: core.EngineRejectPriceBand}, ticker)
	if live.RejectReason != nil {
		t.Error("reject_reason must be null unless status is rejected")
	}
	cash := OrderFromState(core.OrderState{Status: core.StatusRejected, CashReject: core.ReasonInsufficientHoldings}, ticker)
	if cash.RejectReason == nil || *cash.RejectReason != "insufficient_holdings" {
		t.Errorf("got %v", cash.RejectReason)
	}
	eng := OrderFromState(core.OrderState{Status: core.StatusRejected, EngineReject: core.EngineRejectSelfTrade}, ticker)
	if eng.RejectReason == nil || *eng.RejectReason != "self_trade" {
		t.Errorf("got %v", eng.RejectReason)
	}
}

func TestUnknownSymbolFallsBackToItsId(t *testing.T) {
	if got := OrderFromState(core.OrderState{Symbol: 9}, ticker).Symbol; got != "9" {
		t.Errorf("symbol = %q", got)
	}
}

func TestOrdersAndPositionsAreEmptyArraysNotNull(t *testing.T) {
	for name, v := range map[string]any{"orders": Orders(nil, ticker), "positions": Positions(nil, ticker)} {
		if b, _ := json.Marshal(v); string(b) != "[]" {
			t.Errorf("%s = %s", name, b)
		}
	}
}

func TestAccountCashIsFreePlusLocked(t *testing.T) {
	a := AccountFromSnapshot(core.AccountSnapshot{Account: 17, Seq: 3, Balances: core.Balances{CashFreeMinor: 100050, CashLockedMinor: 2500, RealizedPnlMinor: -125}})
	if a.AccountID != "17" || a.Cash != "1025.50" || a.CashFree != "1000.50" || a.CashLocked != "25.00" || a.RealizedPnl != "-1.25" || a.AccountSeq != 3 {
		t.Errorf("got %+v", a)
	}
}
