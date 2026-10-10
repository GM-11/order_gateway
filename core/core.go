package core

import (
	"context"
	"errors"
	"time"
)

type (
	AccountID       uint64
	ClientRequestID uint64
	ConnID          uint64
	SymbolID        uint32
	OrderID         uint64
)

type Side uint8

const (
	Buy Side = iota + 1
	Sell
)

type OrderRequest struct {
	AccountID       AccountID
	ClientRequestID ClientRequestID
	Symbol          SymbolID
	Side            Side
	PriceMinor      int64
	Qty             int64
}

type RejectReason uint8

const (
	ReasonNone RejectReason = iota
	ReasonInsufficientCash
	ReasonInsufficientHoldings
	ReasonEngineRejected
	ReasonUnknown
)

type Acknowledge struct {
	Accepted       bool
	Reason         RejectReason
	ShortFallMinor int64
}

var ErrNoAnswer = errors.New("cash owner: no answer, outcome unknown")

type CashOwner interface {
	Submit(ctx context.Context, req OrderRequest) (Acknowledge, error)
}

type MsgKind uint8

const (
	MsgRejected MsgKind = iota + 1
	MsgOrderResult
)

type OutMsg struct {
	Kind            MsgKind
	ClientRequestID ClientRequestID
	OrderID         OrderID
	Reason          RejectReason
	ShortfallMinor  int64
}

type Sender interface {
	Send(conn ConnID, msg OutMsg) bool
}

type EngineReply struct {
	AccountID       AccountID
	ClientRequestID ClientRequestID
	OrderID         OrderID
	Rejected        bool
}

type requestKey struct {
	account AccountID
	request ClientRequestID
}

type pendingEntry struct {
	conn   ConnID
	sentAt time.Time
}

type Gateway struct {
	cash          CashOwner
	out           Sender
	now           func() time.Time
	submitTimeout time.Duration
	pending       map[requestKey]pendingEntry
}

func New(cash CashOwner, out Sender, now func() time.Time, submitTimeout time.Duration) *Gateway {
	return &Gateway{
		cash:          cash,
		out:           out,
		now:           now,
		submitTimeout: submitTimeout,
		pending:       make(map[requestKey]pendingEntry),
	}
}

func (g *Gateway) OnPlace(ctx context.Context, conn ConnID, req OrderRequest) {
	key := requestKey{req.AccountID, req.ClientRequestID}
	g.pending[key] = pendingEntry{conn: conn, sentAt: g.now()}

	ctx, cancel := context.WithTimeout(ctx, g.submitTimeout)
	defer cancel()
	ack, err := g.cash.Submit(ctx, req)

	switch {
	case err != nil:
		return
	case !ack.Accepted:
		delete(g.pending, key)
		g.out.Send(conn, OutMsg{Kind: MsgRejected, ClientRequestID: req.ClientRequestID, Reason: ack.Reason})
	}
}

func (g *Gateway) OnReply(r EngineReply) {
	key := requestKey{r.AccountID, r.ClientRequestID}
	e, ok := g.pending[key]
	if !ok {
		return
	}
	delete(g.pending, key)
	msg := OutMsg{Kind: MsgOrderResult, ClientRequestID: r.ClientRequestID, OrderID: r.OrderID}
	if r.Rejected {
		msg.Reason = ReasonEngineRejected
	}
	g.out.Send(e.conn, msg)
}
