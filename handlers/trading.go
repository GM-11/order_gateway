package handlers

import (
	"context"
	"log"
	"net/http"
	"order_gateway/auth"
	"order_gateway/core"
	"order_gateway/validate"
	"order_gateway/wire"
)

// Cash is what the read and cancel endpoints need from the cash owner. Reads
// come from the cash owner (not a copy) so they line up exactly with the
// trading stream's account_seq.
type Cash interface {
	GetAccount(ctx context.Context, account core.AccountID) (core.AccountSnapshot, error)
	GetOrders(ctx context.Context, q core.OrdersQuery) ([]core.OrderState, error)
	CancelOrder(ctx context.Context, req core.CancelRequest) (core.CancelAck, error)
}

// Trading serves every /v2 order, account and position endpoint except
// POST /v2/orders (see Orders). All of them are authenticated the same way.
type Trading struct {
	Auth    auth.Verifier
	Symbols Symbols
	Cash    Cash
}

func (t *Trading) ticker(id core.SymbolID) (string, bool) {
	inst, ok := t.Symbols.ByID(id)
	return inst.Ticker, ok
}

func cashOwnerUnavailable(w http.ResponseWriter, err error) {
	log.Printf("trading: cash owner did not answer: %v", err)
	writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "cash_owner_unavailable"})
}

func orderNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, errorResponse{Error: "order_not_found"})
}

// GET /v2/orders?status=open|closed|all
func (t *Trading) ListOrders(w http.ResponseWriter, r *http.Request) {
	p, ok := authenticate(t.Auth, w, r)
	if !ok {
		return
	}
	var filter core.OrdersFilter
	switch r.URL.Query().Get("status") {
	case "", "open":
		filter = core.FilterOpen
	case "closed":
		filter = core.FilterClosed
	case "all":
		filter = core.FilterAll
	default:
		e := invalid("status", `must be "open", "closed" or "all"`)
		writeJSON(w, e.status, e.body)
		return
	}
	orders, err := t.Cash.GetOrders(r.Context(), core.OrdersQuery{Account: p.Account, Filter: filter})
	if err != nil {
		cashOwnerUnavailable(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wire.Orders(orders, t.ticker))
}

// GET /v2/orders/{order_id}
func (t *Trading) GetOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := authenticate(t.Auth, w, r)
	if !ok {
		return
	}
	symbol, seq, err := validate.ParseOrderID(r.PathValue("order_id"))
	if err != nil { // the id is opaque to clients: a bad one is simply not an order of theirs
		orderNotFound(w)
		return
	}
	t.writeOne(w, r.Context(), core.OrdersQuery{
		Account: p.Account,
		Filter:  core.FilterAll,
		Order:   &core.OrderRef{Symbol: core.SymbolID(symbol), OrderID: core.OrderID(seq)},
	})
}

// GET /v2/orders:by_client_order_id?client_order_id=abc
func (t *Trading) GetOrderByClientOrderID(w http.ResponseWriter, r *http.Request) {
	p, ok := authenticate(t.Auth, w, r)
	if !ok {
		return
	}
	coid := r.URL.Query().Get("client_order_id")
	if err := validate.ClientOrderID(coid); err != nil {
		e := invalid("client_order_id", err.Error())
		writeJSON(w, e.status, e.body)
		return
	}
	t.writeOne(w, r.Context(), core.OrdersQuery{
		Account:       p.Account,
		Filter:        core.FilterAll,
		ClientOrderID: core.ClientOrderID(coid),
	})
}

func (t *Trading) writeOne(w http.ResponseWriter, ctx context.Context, q core.OrdersQuery) {
	orders, err := t.Cash.GetOrders(ctx, q)
	if err != nil {
		cashOwnerUnavailable(w, err)
		return
	}
	if len(orders) == 0 {
		orderNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, wire.OrderFromState(orders[0], t.ticker))
}

// DELETE /v2/orders/{order_id}
func (t *Trading) CancelOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := authenticate(t.Auth, w, r)
	if !ok {
		return
	}
	symbol, seq, err := validate.ParseOrderID(r.PathValue("order_id"))
	if err != nil {
		orderNotFound(w)
		return
	}
	ack, err := t.Cash.CancelOrder(r.Context(), core.CancelRequest{
		Account: p.Account, Symbol: core.SymbolID(symbol), OrderID: core.OrderID(seq),
	})
	if err != nil {
		writeJSON(w, http.StatusGatewayTimeout, errorResponse{Error: "outcome_unknown",
			Message: "cancel may or may not have been sent; check the order"})
		return
	}
	if !ack.Accepted {
		switch ack.Reason {
		case core.CancelReasonNotFound:
			orderNotFound(w)
		case core.CancelReasonClosed:
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "order_closed",
				Message: "the order is already closed"})
		case core.CancelReasonExchangeBusy:
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "exchange_busy",
				Message: "cancel was NOT sent; safe to retry"})
		case core.CancelReasonExchangeUnavailable:
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "engine_unavailable",
				Message: "cancel was NOT sent; safe to retry"})
		default:
			log.Printf("trading: unmapped cancel reject reason %d", ack.Reason)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
		}
		return
	}
	if ack.Order == nil {
		log.Printf("trading: cash owner accepted a cancel without returning the order")
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
		return
	}
	state := *ack.Order
	state.Status = core.StatusPendingCancel // the final result arrives on the trading stream
	writeJSON(w, http.StatusAccepted, wire.OrderFromState(state, t.ticker))
}

// NotSupportedYet answers the [later] endpoints (PATCH /v2/orders/{id}, DELETE /v2/orders).
func (t *Trading) NotSupportedYet(w http.ResponseWriter, r *http.Request) {
	if _, ok := authenticate(t.Auth, w, r); !ok {
		return
	}
	e := notSupported("method", r.Method+" "+r.URL.Path+" is not built yet")
	writeJSON(w, e.status, e.body)
}

func (t *Trading) snapshot(w http.ResponseWriter, r *http.Request) (core.AccountSnapshot, bool) {
	p, ok := authenticate(t.Auth, w, r)
	if !ok {
		return core.AccountSnapshot{}, false
	}
	snap, err := t.Cash.GetAccount(r.Context(), p.Account)
	if err != nil {
		cashOwnerUnavailable(w, err)
		return snap, false
	}
	if !snap.Found {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "account_not_found"})
		return snap, false
	}
	return snap, true
}

// GET /v2/account
func (t *Trading) Account(w http.ResponseWriter, r *http.Request) {
	if snap, ok := t.snapshot(w, r); ok {
		writeJSON(w, http.StatusOK, wire.AccountFromSnapshot(snap))
	}
}

// GET /v2/positions
func (t *Trading) Positions(w http.ResponseWriter, r *http.Request) {
	if snap, ok := t.snapshot(w, r); ok {
		writeJSON(w, http.StatusOK, wire.Positions(snap.Positions, t.ticker))
	}
}

// GET /v2/positions/{symbol}
func (t *Trading) Position(w http.ResponseWriter, r *http.Request) {
	snap, ok := t.snapshot(w, r)
	if !ok {
		return
	}
	inst, known := t.Symbols.Lookup(r.PathValue("symbol"))
	if known {
		for _, pos := range snap.Positions {
			if pos.Symbol == inst.ID && pos.FreeQty+pos.LockedQty != 0 {
				writeJSON(w, http.StatusOK, wire.PositionFromCore(pos, t.ticker))
				return
			}
		}
	}
	writeJSON(w, http.StatusNotFound, errorResponse{Error: "position_not_found"})
}
