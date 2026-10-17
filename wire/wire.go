// Package wire holds the public JSON shapes of the order gateway (contract
// section 3.2 / 3.3) and builds them from core types. REST handlers and the
// trading WebSocket both use it, so an Order looks the same everywhere.
// Money is "101.50" strings, quantities and 64-bit ids are strings.
package wire

import (
	"strconv"
	"time"

	"order_gateway/core"
	"order_gateway/validate"
)

// Ticker turns a symbol id back into its ticker. ok=false = not in our table.
type Ticker func(core.SymbolID) (string, bool)

type Order struct {
	OrderID        string  `json:"order_id"`
	ClientOrderID  string  `json:"client_order_id"`
	Symbol         string  `json:"symbol"`
	Side           string  `json:"side"`
	Type           string  `json:"type"`
	TimeInForce    string  `json:"time_in_force"`
	Price          string  `json:"price"`
	StopPrice      *string `json:"stop_price"`
	Qty            string  `json:"qty"`
	FilledQty      string  `json:"filled_qty"`
	FilledAvgPrice *string `json:"filled_avg_price"`
	Status         string  `json:"status"`
	RejectReason   *string `json:"reject_reason"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type Position struct {
	Symbol        string `json:"symbol"`
	Qty           string `json:"qty"`
	QtyFree       string `json:"qty_free"`
	QtyLocked     string `json:"qty_locked"`
	AvgEntryPrice string `json:"avg_entry_price"`
	RealizedPnl   string `json:"realized_pnl"`
}

// Account is the body of GET /v2/account and the "account" of an account_snapshot.
type Account struct {
	AccountID   string `json:"account_id"`
	Cash        string `json:"cash"` // free + locked
	CashFree    string `json:"cash_free"`
	CashLocked  string `json:"cash_locked"`
	RealizedPnl string `json:"realized_pnl"`
	AccountSeq  uint64 `json:"account_seq"`
}

// Balances is the "account" of a trade update: balances AFTER the event.
type Balances struct {
	Cash        string `json:"cash"`
	CashFree    string `json:"cash_free"`
	CashLocked  string `json:"cash_locked"`
	RealizedPnl string `json:"realized_pnl"`
}

// Time is RFC 3339 UTC with milliseconds, from the sequencer's ms clock.
func Time(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func Qty(n int64) string { return strconv.FormatInt(n, 10) }

func sideName(s core.Side) string {
	if s == core.Sell {
		return "sell"
	}
	return "buy"
}

func typeName(t core.OrderType) string {
	switch t {
	case core.TypeMarket:
		return "market"
	case core.TypeStop:
		return "stop"
	case core.TypeStopLimit:
		return "stop_limit"
	default:
		return "limit"
	}
}

func tifName(t core.TimeInForce) string {
	switch t {
	case core.TIFImmediateOrCancel:
		return "ioc"
	case core.TIFDay:
		return "day"
	default:
		return "gtc"
	}
}

func StatusName(s core.OrderStatus) string {
	switch s {
	case core.StatusNew:
		return "new"
	case core.StatusPartiallyFilled:
		return "partially_filled"
	case core.StatusFilled:
		return "filled"
	case core.StatusCanceled:
		return "canceled"
	case core.StatusRejected:
		return "rejected"
	case core.StatusExpired:
		return "expired"
	case core.StatusPendingCancel:
		return "pending_cancel"
	case core.StatusPendingReplace:
		return "pending_replace"
	default:
		return "pending_new"
	}
}

// EngineRejectName maps the engine's refusal onto the public reject_reason list.
func EngineRejectName(r core.EngineReject) string {
	switch r {
	case core.EngineRejectInvalidPrice:
		return "invalid_price"
	case core.EngineRejectInvalidQuantity:
		return "invalid_quantity"
	case core.EngineRejectSelfTrade:
		return "self_trade"
	case core.EngineRejectSymbolHalted:
		return "symbol_halted"
	case core.EngineRejectPriceBand:
		return "price_band"
	case core.EngineRejectPriceCollar:
		return "price_collar"
	case core.EngineRejectStopWouldTrigger:
		return "stop_would_trigger"
	default:
		return "rejected"
	}
}

func rejectReason(s core.OrderState) *string {
	if s.Status != core.StatusRejected {
		return nil
	}
	var name string
	switch {
	case s.CashReject == core.ReasonInsufficientCash:
		name = "insufficient_cash"
	case s.CashReject == core.ReasonInsufficientHoldings:
		name = "insufficient_holdings"
	default:
		name = EngineRejectName(s.EngineReject)
	}
	return &name
}

// FilledAvgPrice is notional / filled rounded DOWN to the paisa; nil until the first fill.
func FilledAvgPrice(notionalMinor, filledQty int64) *string {
	if filledQty <= 0 {
		return nil
	}
	s := validate.FormatMinor(notionalMinor / filledQty)
	return &s
}

func symbolName(id core.SymbolID, tick Ticker) string {
	if tick != nil {
		if name, ok := tick(id); ok {
			return name
		}
	}
	return strconv.FormatUint(uint64(id), 10) // table out of step with the cash owner; better than an empty string
}

func OrderFromState(s core.OrderState, tick Ticker) Order {
	o := Order{
		OrderID:        validate.FormatOrderID(uint32(s.Symbol), uint64(s.OrderID)),
		ClientOrderID:  string(s.ClientOrderID),
		Symbol:         symbolName(s.Symbol, tick),
		Side:           sideName(s.Side),
		Type:           typeName(s.Type),
		TimeInForce:    tifName(s.TimeInForce),
		Price:          validate.FormatMinor(s.PriceMinor),
		Qty:            Qty(s.Qty),
		FilledQty:      Qty(s.FilledQty),
		FilledAvgPrice: FilledAvgPrice(s.FilledNotionalMinor, s.FilledQty),
		Status:         StatusName(s.Status),
		RejectReason:   rejectReason(s),
		CreatedAt:      Time(s.CreatedTs),
		UpdatedAt:      Time(s.UpdatedTs),
	}
	if s.StopPriceMinor != nil {
		v := validate.FormatMinor(*s.StopPriceMinor)
		o.StopPrice = &v
	}
	return o
}

func Orders(in []core.OrderState, tick Ticker) []Order {
	out := make([]Order, 0, len(in)) // [] not null when empty
	for _, s := range in {
		out = append(out, OrderFromState(s, tick))
	}
	return out
}

func PositionFromCore(p core.Position, tick Ticker) Position {
	return Position{
		Symbol:        symbolName(p.Symbol, tick),
		Qty:           Qty(p.FreeQty + p.LockedQty),
		QtyFree:       Qty(p.FreeQty),
		QtyLocked:     Qty(p.LockedQty),
		AvgEntryPrice: validate.FormatMinor(p.AvgPriceMinor),
		RealizedPnl:   validate.FormatMinor(p.RealizedPnlMinor),
	}
}

// Positions lists the positions with something in them; a fully closed
// position is not "held".
func Positions(in []core.Position, tick Ticker) []Position {
	out := make([]Position, 0, len(in))
	for _, p := range in {
		if p.FreeQty+p.LockedQty != 0 {
			out = append(out, PositionFromCore(p, tick))
		}
	}
	return out
}

func BalancesFromCore(b core.Balances) Balances {
	return Balances{
		Cash:        validate.FormatMinor(b.CashFreeMinor + b.CashLockedMinor),
		CashFree:    validate.FormatMinor(b.CashFreeMinor),
		CashLocked:  validate.FormatMinor(b.CashLockedMinor),
		RealizedPnl: validate.FormatMinor(b.RealizedPnlMinor),
	}
}

func AccountFromSnapshot(s core.AccountSnapshot) Account {
	b := BalancesFromCore(s.Balances)
	return Account{
		AccountID:   strconv.FormatUint(uint64(s.Account), 10),
		Cash:        b.Cash,
		CashFree:    b.CashFree,
		CashLocked:  b.CashLocked,
		RealizedPnl: b.RealizedPnl,
		AccountSeq:  s.Seq,
	}
}
