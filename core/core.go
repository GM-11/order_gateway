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

const (
	Buy Side = iota + 1
	Sell
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
	Kind           MsgKind
	ClientOrderID  ClientOrderID
	OrderID        OrderID
	Reason         RejectReason
	ShortfallMinor int64
}

type Sender interface {
	Send(conn ConnID, msg OutMsg) bool
}

type EngineReply struct {
	AccountID     AccountID
	ClientOrderID ClientOrderID
	OrderID       OrderID
	Rejected      bool
}

type requestKey struct {
	account AccountID
	request ClientOrderID
}

type orderStage uint8

const (
	stageSentToCash orderStage = iota + 1
	stageAccepted
	stageUnknown
)

type pendingEntry struct {
	conn   ConnID
	sentAt time.Time
	stage  orderStage
}

type Gateway struct {
	cash          CashOwner
	out           Sender
	now           func() time.Time
	submitTimeout time.Duration
	pending       map[requestKey]pendingEntry
	mu            sync.Mutex
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
	key := requestKey{req.AccountID, req.ClientOrderID}
	now := g.now()
	g.mu.Lock()
	g.pending[key] = pendingEntry{conn: conn, sentAt: now, stage: stageSentToCash}
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, g.submitTimeout)
	defer cancel()

	ack, err := g.cash.Submit(ctx, req)

	switch {
	case err != nil:
		g.mu.Lock()
		if entry, ok := g.pending[key]; ok {
			entry.stage = stageUnknown
			g.pending[key] = entry
		}
		g.mu.Unlock()
		return
	case ack.Accepted:
		g.mu.Lock()
		if entry, ok := g.pending[key]; ok {
			entry.stage = stageAccepted
			g.pending[key] = entry
		}
		g.mu.Unlock()
	case !ack.Accepted:
		g.mu.Lock()
		delete(g.pending, key)
		g.mu.Unlock()
		g.out.Send(conn, OutMsg{Kind: MsgRejected, ClientOrderID: req.ClientOrderID, Reason: ack.Reason, ShortfallMinor: ack.ShortFallMinor})
	}
}

func (g *Gateway) OnReply(r EngineReply) {
	key := requestKey{r.AccountID, r.ClientOrderID}
	g.mu.Lock()
	e, ok := g.pending[key]
	if !ok {
		g.mu.Unlock()
		return
	}
	delete(g.pending, key)
	g.mu.Unlock()

	msg := OutMsg{Kind: MsgOrderResult, ClientOrderID: r.ClientOrderID, OrderID: r.OrderID}
	if r.Rejected {
		msg.Reason = ReasonEngineRejected
	}
	g.out.Send(e.conn, msg)
}
