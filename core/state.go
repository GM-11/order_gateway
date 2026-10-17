package core

type EngineReject uint8

const (
	EngineRejectNone EngineReject = iota
	EngineRejectInvalidPrice
	EngineRejectInvalidQuantity
	EngineRejectSelfTrade
	EngineRejectSymbolHalted
	EngineRejectPriceBand
	EngineRejectPriceCollar
	EngineRejectStopWouldTrigger
	EngineRejectOther // any other engine refusal: public reason "rejected"
)

type OrderType uint8

const (
	TypeLimit OrderType = iota + 1
	TypeMarket
	TypeStop
	TypeStopLimit
)

type TimeInForce uint8

const (
	TIFGoodTilCancelled TimeInForce = iota + 1
	TIFImmediateOrCancel
	TIFDay
)

type OrderStatus uint8

const (
	StatusPendingNew OrderStatus = iota + 1
	StatusNew
	StatusPartiallyFilled
	StatusFilled
	StatusCanceled
	StatusRejected
	StatusExpired
	StatusPendingCancel
	StatusPendingReplace
)

// OrderState is one order as the cash owner knows it right now.
type OrderState struct {
	Account             AccountID
	ClientOrderID       ClientOrderID
	Symbol              SymbolID
	OrderID             OrderID // 0 = not known yet
	Side                Side
	Type                OrderType
	TimeInForce         TimeInForce
	PriceMinor          int64
	StopPriceMinor      *int64
	Qty                 int64
	FilledQty           int64
	FilledNotionalMinor int64 // average fill price = notional / filled
	Status              OrderStatus
	EngineReject        EngineReject
	CashReject          RejectReason // ReasonNone unless the cash owner refused it
	CreatedTs           int64        // milliseconds since epoch, the sequencer's clock
	UpdatedTs           int64
}

type Position struct {
	Symbol           SymbolID
	FreeQty          int64
	LockedQty        int64
	AvgPriceMinor    int64
	RealizedPnlMinor int64
}

type Balances struct {
	CashFreeMinor    int64
	CashLockedMinor  int64
	RealizedPnlMinor int64
}

type AccountSnapshot struct {
	Found      bool
	Account    AccountID
	Seq        uint64
	Balances   Balances
	Positions  []Position
	OpenOrders []OrderState
}

type OrdersFilter uint8

const (
	FilterOpen OrdersFilter = iota + 1
	FilterClosed
	FilterAll
)

type OrderRef struct {
	Symbol  SymbolID
	OrderID OrderID
}

// OrdersQuery: Filter selects a list; Order or ClientOrderID selects exactly one.
type OrdersQuery struct {
	Account       AccountID
	Filter        OrdersFilter
	Order         *OrderRef
	ClientOrderID ClientOrderID
}

type CancelRequest struct {
	Account AccountID
	Symbol  SymbolID
	OrderID OrderID
}

type CancelReason uint8

const (
	CancelReasonNone CancelReason = iota
	CancelReasonNotFound
	CancelReasonClosed
	CancelReasonExchangeBusy
	CancelReasonExchangeUnavailable
	CancelReasonUnknown // anything the gateway does not recognise
)

type CancelAck struct {
	Accepted bool
	Reason   CancelReason
	Order    *OrderState
}

// UpdateKind says what caused an AccountUpdate.
type UpdateKind uint8

const (
	UpdateUnknown UpdateKind = iota
	UpdateNew
	UpdatePartialFill
	UpdateFill
	UpdateCanceled
	UpdateCancelRejected
	UpdateRejected
	UpdateExpired
	UpdateReplaced
	UpdateReplaceRejected
	UpdateTransfer
)

type Transfer struct {
	ID          string
	Deposit     bool
	AmountMinor int64
}

// AccountUpdate: one cause plus the resulting balances, in one message (never torn).
type AccountUpdate struct {
	Account        AccountID
	Seq            uint64 // per account, +1 per update, no gaps
	TsMs           int64
	Kind           UpdateKind
	Order          *OrderState // nil for a transfer
	FillPriceMinor int64       // fills only
	FillQty        int64       // fills only
	Transfer       *Transfer   // transfers only
	Balances       Balances    // AFTER this update
	Positions      []Position  // only the positions this update changed, AFTER
}
