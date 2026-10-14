package deliver

import (
	"order_gateway/core"
	"strconv"
)

type wireMsg struct {
	Type            string `json:"type"`
	ClientRequestID string `json:"client_request_id"` // string, because JavaScript breaks big 64-bit numbers
	OrderID         string `json:"order_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
	ShortfallMinor  int64  `json:"shortfall_minor,omitempty"`
}

func toWire(m core.OutMsg) wireMsg {
	w := wireMsg{
		ClientRequestID: strconv.FormatUint(uint64(m.ClientRequestID), 10),
		ShortfallMinor:  m.ShortfallMinor,
	}
	switch m.Kind {
	case core.MsgRejected:
		w.Type = "rejected"
	case core.MsgOrderResult:
		w.Type = "order_result"
		w.OrderID = strconv.FormatUint(uint64(m.OrderID), 10)
	}
	switch m.Reason {
	case core.ReasonInsufficientCash:
		w.Reason = "insufficient_cash"
	case core.ReasonInsufficientHoldings:
		w.Reason = "insufficient_holdings"
	case core.ReasonEngineRejected:
		w.Reason = "engine_rejected"
	case core.ReasonUnknown:
		w.Reason = "unknown"
	}
	return w
}
