package stream

import (
	"encoding/json"
	"log"

	"order_gateway/core"
	"order_gateway/validate"
	"order_gateway/wire"
)

// frame is {"stream": name, "data": data}, one JSON object per WebSocket message.
func frame(name string, data any) []byte {
	b, err := json.Marshal(struct {
		Stream string `json:"stream"`
		Data   any    `json:"data"`
	}{name, data})
	if err != nil { // only possible for an unmarshalable type: a programming error
		log.Printf("stream: marshal %s: %v", name, err)
		return []byte(`{"stream":"error","data":{}}`)
	}
	return b
}

func snapshotFrame(s core.AccountSnapshot, tick wire.Ticker) []byte {
	return frame("account_snapshot", struct {
		AccountSeq uint64          `json:"account_seq"`
		Account    wire.Account    `json:"account"`
		Positions  []wire.Position `json:"positions"`
		OpenOrders []wire.Order    `json:"open_orders"`
	}{
		AccountSeq: s.Seq,
		Account:    wire.AccountFromSnapshot(s),
		Positions:  wire.Positions(s.Positions, tick),
		OpenOrders: wire.Orders(s.OpenOrders, tick),
	})
}

type transferJSON struct {
	TransferID string `json:"transfer_id"`
	Kind       string `json:"kind"`
	Amount     string `json:"amount"`
}

type tradeUpdate struct {
	Event      string         `json:"event"`
	AccountSeq uint64         `json:"account_seq"`
	Timestamp  string         `json:"timestamp"`
	Order      *wire.Order    `json:"order,omitempty"`
	Price      string         `json:"price,omitempty"`
	Qty        string         `json:"qty,omitempty"`
	Transfer   *transferJSON  `json:"transfer,omitempty"`
	Account    wire.Balances  `json:"account"`
	Position   *wire.Position `json:"position,omitempty"`
}

func eventName(k core.UpdateKind) string {
	switch k {
	case core.UpdateNew:
		return "new"
	case core.UpdatePartialFill:
		return "partial_fill"
	case core.UpdateFill:
		return "fill"
	case core.UpdateCanceled:
		return "canceled"
	case core.UpdateCancelRejected:
		return "cancel_rejected"
	case core.UpdateRejected:
		return "rejected"
	case core.UpdateExpired:
		return "expired"
	case core.UpdateReplaced:
		return "replaced"
	case core.UpdateReplaceRejected:
		return "replace_rejected"
	case core.UpdateTransfer:
		return "transfer"
	default:
		return "unknown"
	}
}

func updateFrame(u core.AccountUpdate, tick wire.Ticker) []byte {
	t := tradeUpdate{
		Event:      eventName(u.Kind),
		AccountSeq: u.Seq,
		Timestamp:  wire.Time(u.TsMs),
		Account:    wire.BalancesFromCore(u.Balances),
	}
	if t.Event == "unknown" {
		// Still delivered: dropping it would show the client a false gap in account_seq.
		log.Printf("stream: account %d seq %d: update with an unrecognised cause", u.Account, u.Seq)
	}
	if u.Order != nil {
		o := wire.OrderFromState(*u.Order, tick)
		t.Order = &o
	}
	if u.Kind == core.UpdatePartialFill || u.Kind == core.UpdateFill {
		t.Price = validate.FormatMinor(u.FillPriceMinor)
		t.Qty = wire.Qty(u.FillQty)
		if u.Order != nil {
			for _, p := range u.Positions {
				if p.Symbol == u.Order.Symbol {
					wp := wire.PositionFromCore(p, tick)
					t.Position = &wp
					break
				}
			}
		}
	}
	if u.Transfer != nil {
		kind := "withdraw"
		if u.Transfer.Deposit {
			kind = "deposit"
		}
		t.Transfer = &transferJSON{TransferID: u.Transfer.ID, Kind: kind, Amount: validate.FormatMinor(u.Transfer.AmountMinor)}
	}
	return frame("trade_updates", t)
}
