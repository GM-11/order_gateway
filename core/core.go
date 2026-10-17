package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

type (
	AccountID     uint64
	ClientOrderID string
	ConnID        uint64
	SymbolID      uint32
	OrderID       uint64
)

type Side uint8

type Result struct {
	Outcome        Outcome
	OrderID        OrderID
	Reason         RejectReason
	ShortfallMinor int64
	EngineReject   EngineReject // OutcomeEngineRejected only
	Order          *OrderState  // OutcomeDuplicate only: the order as it stands now
}

var ErrDuplicateInFlight = errors.New("same client_order_id is still in flight")

const (
	Buy Side = iota + 1
	Sell
)

type Outcome uint8

const (
	OutcomeCashRejected Outcome = iota + 1
	OutcomePlaced
	OutcomeEngineRejected
	OutcomeDuplicate // this (account, client_order_id) was seen before and has an order id
)

type OrderRequest struct {
	AccountID     AccountID
	ClientOrderID ClientOrderID
	Symbol        SymbolID
	Side          Side
	PriceMinor    int64
	Qty           int64
}

type RejectReason uint8

const (
	ReasonNone RejectReason = iota
	ReasonInsufficientCash
	ReasonInsufficientHoldings
	ReasonEngineRejected
	ReasonUnknown
	ReasonUnknownAccount
	ReasonExchangeBusy // not placed, safe to retry
	ReasonUnknownSymbol
	ReasonExchangeUnavailable // not placed, safe to retry
	ReasonNotSupported
)

type Acknowledge struct {
	Accepted       bool
	Reason         RejectReason
	ShortFallMinor int64
	Duplicate      bool        // this (account, client_order_id) was seen before
	Order          *OrderState // set when Duplicate: the order as it stands now
}

var ErrNoAnswer = errors.New("no answer before deadline, outcome unknown")

type CashOwner interface {
	Submit(ctx context.Context, req OrderRequest) (Acknowledge, error)
}

type MsgKind uint8

const (
	MsgRejected MsgKind = iota + 1
	MsgOrderResult
	MsgRaw // Payload is a ready-made frame, sent as is (the trading stream builds its own JSON)
)

type OutMsg struct {
	Kind           MsgKind
	ClientOrderID  ClientOrderID
	OrderID        OrderID
	Reason         RejectReason
	ShortfallMinor int64
	Payload        []byte // MsgRaw only
}

type Sender interface {
	Send(conn ConnID, msg OutMsg) bool
}

type EngineReply struct {
	AccountID     AccountID
	ClientOrderID ClientOrderID
	OrderID       OrderID
	Rejected      bool
	EngineReject  EngineReject // meaningful when Rejected
}

type requestKey struct {
	account AccountID
	request ClientOrderID
}

type pendingEntry struct {
	mailbox chan Result
	sentAt  time.Time
}

type Gateway struct {
	cash    CashOwner
	now     func() time.Time
	timeout time.Duration
	pending map[requestKey]pendingEntry
	mu      sync.Mutex
}

func New(cash CashOwner, now func() time.Time, timeout time.Duration) *Gateway {
	return &Gateway{
		cash:    cash,
		now:     now,
		timeout: timeout,
		pending: make(map[requestKey]pendingEntry),
	}
}

func (g *Gateway) Place(ctx context.Context, req OrderRequest) (Result, error) {
	key := requestKey{req.AccountID, req.ClientOrderID}
	mailbox := make(chan Result, 1)
	now := g.now()
	g.mu.Lock()
	if _, busy := g.pending[key]; busy {
		g.mu.Unlock()
		return Result{}, ErrDuplicateInFlight
	}
	g.pending[key] = pendingEntry{mailbox: mailbox, sentAt: now}
	g.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	ack, err := g.cash.Submit(ctx, req)

	switch {
	case err != nil:
		g.forget(key)
		return Result{}, ErrNoAnswer

	case ack.Duplicate:
		// Seen before: no engine Reply will come for this call, so don't wait for one.
		g.forget(key)
		if ack.Order != nil && ack.Order.OrderID != 0 {
			return Result{Outcome: OutcomeDuplicate, Order: ack.Order}, nil
		}
		return Result{}, ErrDuplicateInFlight

	case !ack.Accepted:
		g.forget(key)
		return Result{Outcome: OutcomeCashRejected, Reason: ack.Reason, ShortfallMinor: ack.ShortFallMinor}, nil
	}

	select {
	case r := <-mailbox:
		return r, nil
	case <-ctx.Done():
		g.forget(key)
		return Result{}, ErrNoAnswer
	}
}

func (g *Gateway) forget(key requestKey) {
	g.mu.Lock()
	delete(g.pending, key) // deleting a missing key is a no-op in Go
	g.mu.Unlock()
}

func (g *Gateway) OnReply(r EngineReply) {
	if r.ClientOrderID == "" {
		return // a cancel/modify answer: nobody is waiting for it here
	}
	key := requestKey{r.AccountID, r.ClientOrderID}

	g.mu.Lock()
	e, ok := g.pending[key]
	if ok {
		delete(g.pending, key)
	}
	g.mu.Unlock()
	if !ok {
		return
	}

	res := Result{Outcome: OutcomePlaced, OrderID: r.OrderID}
	if r.Rejected {
		res.Outcome = OutcomeEngineRejected
		res.EngineReject = r.EngineReject
	}
	e.mailbox <- res
}
