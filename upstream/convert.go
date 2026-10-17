package upstream

import (
	"order_gateway/core"
	pb "order_gateway/gen/exchange/v1"
)

// proto <-> core translation for the cash owner's messages. Anything the
// gateway does not recognise maps to the "unknown" value of the target type,
// never to a success.

func engineRejectFromProto(r pb.RejectReason) core.EngineReject {
	switch r {
	case pb.RejectReason_REJECT_REASON_UNSPECIFIED, pb.RejectReason_REJECT_REASON_NONE:
		return core.EngineRejectNone
	case pb.RejectReason_REJECT_REASON_INVALID_PRICE:
		return core.EngineRejectInvalidPrice
	case pb.RejectReason_REJECT_REASON_INVALID_QUANTITY:
		return core.EngineRejectInvalidQuantity
	case pb.RejectReason_REJECT_REASON_SELF_TRADE:
		return core.EngineRejectSelfTrade
	case pb.RejectReason_REJECT_REASON_SYMBOL_HALTED:
		return core.EngineRejectSymbolHalted
	case pb.RejectReason_REJECT_REASON_PRICE_BAND:
		return core.EngineRejectPriceBand
	case pb.RejectReason_REJECT_REASON_PRICE_COLLAR:
		return core.EngineRejectPriceCollar
	case pb.RejectReason_REJECT_REASON_STOP_WOULD_TRIGGER:
		return core.EngineRejectStopWouldTrigger
	default:
		return core.EngineRejectOther
	}
}

func cashRejectFromProto(r pb.CashRejectReason) core.RejectReason {
	switch r {
	case pb.CashRejectReason_CASH_REJECT_REASON_UNSPECIFIED:
		return core.ReasonNone
	case pb.CashRejectReason_CASH_REJECT_REASON_INSUFFICIENT_CASH:
		return core.ReasonInsufficientCash
	case pb.CashRejectReason_CASH_REJECT_REASON_INSUFFICIENT_HOLDINGS:
		return core.ReasonInsufficientHoldings
	case pb.CashRejectReason_CASH_REJECT_REASON_UNKNOWN_ACCOUNT:
		return core.ReasonUnknownAccount
	case pb.CashRejectReason_CASH_REJECT_REASON_EXCHANGE_BUSY:
		return core.ReasonExchangeBusy
	case pb.CashRejectReason_CASH_REJECT_REASON_UNKNOWN_SYMBOL:
		return core.ReasonUnknownSymbol
	case pb.CashRejectReason_CASH_REJECT_REASON_EXCHANGE_UNAVAILABLE:
		return core.ReasonExchangeUnavailable
	case pb.CashRejectReason_CASH_REJECT_REASON_NOT_SUPPORTED:
		return core.ReasonNotSupported
	default:
		return core.ReasonUnknown
	}
}

func sideFromProto(s pb.Side) core.Side {
	if s == pb.Side_SIDE_SELL {
		return core.Sell
	}
	return core.Buy
}

func typeFromProto(t pb.OrderType) core.OrderType {
	if t == pb.OrderType_ORDER_TYPE_MARKET {
		return core.TypeMarket
	}
	return core.TypeLimit // UNSPECIFIED = LIMIT
}

func tifFromProto(t pb.TimeInForce) core.TimeInForce {
	switch t {
	case pb.TimeInForce_TIME_IN_FORCE_IOC:
		return core.TIFImmediateOrCancel
	case pb.TimeInForce_TIME_IN_FORCE_DAY:
		return core.TIFDay
	default:
		return core.TIFGoodTilCancelled // UNSPECIFIED = GTC
	}
}

func statusFromProto(s pb.OrderStatus) core.OrderStatus {
	switch s {
	case pb.OrderStatus_ORDER_STATUS_PENDING_NEW:
		return core.StatusPendingNew
	case pb.OrderStatus_ORDER_STATUS_NEW:
		return core.StatusNew
	case pb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED:
		return core.StatusPartiallyFilled
	case pb.OrderStatus_ORDER_STATUS_FILLED:
		return core.StatusFilled
	case pb.OrderStatus_ORDER_STATUS_CANCELED:
		return core.StatusCanceled
	case pb.OrderStatus_ORDER_STATUS_REJECTED:
		return core.StatusRejected
	case pb.OrderStatus_ORDER_STATUS_EXPIRED:
		return core.StatusExpired
	case pb.OrderStatus_ORDER_STATUS_PENDING_CANCEL:
		return core.StatusPendingCancel
	case pb.OrderStatus_ORDER_STATUS_PENDING_REPLACE:
		return core.StatusPendingReplace
	default:
		return core.StatusPendingNew // not known yet: the safest thing to claim is "not confirmed"
	}
}

func orderFromProto(o *pb.OrderState) *core.OrderState {
	if o == nil {
		return nil
	}
	out := &core.OrderState{
		Account:             core.AccountID(o.GetAccountId()),
		ClientOrderID:       core.ClientOrderID(o.GetClientOrderId()),
		Symbol:              core.SymbolID(o.GetSymbol()),
		OrderID:             core.OrderID(o.GetOrderId()),
		Side:                sideFromProto(o.GetSide()),
		Type:                typeFromProto(o.GetType()),
		TimeInForce:         tifFromProto(o.GetTimeInForce()),
		PriceMinor:          o.GetPriceMinor(),
		Qty:                 o.GetQuantity(),
		FilledQty:           o.GetFilledQuantity(),
		FilledNotionalMinor: o.GetFilledNotionalMinor(),
		Status:              statusFromProto(o.GetStatus()),
		EngineReject:        engineRejectFromProto(o.GetEngineRejectReason()),
		CashReject:          cashRejectFromProto(o.GetCashRejectReason()),
		CreatedTs:           o.GetCreatedTs(),
		UpdatedTs:           o.GetUpdatedTs(),
	}
	if o.StopPriceMinor != nil {
		v := o.GetStopPriceMinor()
		out.StopPriceMinor = &v
	}
	return out
}

func ordersFromProto(in []*pb.OrderState) []core.OrderState {
	out := make([]core.OrderState, 0, len(in))
	for _, o := range in {
		if s := orderFromProto(o); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func positionsFromProto(in []*pb.Position) []core.Position {
	out := make([]core.Position, 0, len(in))
	for _, p := range in {
		out = append(out, core.Position{
			Symbol:           core.SymbolID(p.GetSymbol()),
			FreeQty:          p.GetFreeQty(),
			LockedQty:        p.GetLockedQty(),
			AvgPriceMinor:    p.GetAvgPriceMinor(),
			RealizedPnlMinor: p.GetRealizedPnlMinor(),
		})
	}
	return out
}

func balancesFromProto(b *pb.Balances) core.Balances {
	return core.Balances{
		CashFreeMinor:    b.GetCashFreeMinor(),
		CashLockedMinor:  b.GetCashLockedMinor(),
		RealizedPnlMinor: b.GetRealizedPnlMinor(),
	}
}

func snapshotFromProto(s *pb.AccountSnapshot) core.AccountSnapshot {
	return core.AccountSnapshot{
		Found:      s.GetFound(),
		Account:    core.AccountID(s.GetAccountId()),
		Seq:        s.GetAccountSeq(),
		Balances:   balancesFromProto(s.GetBalances()),
		Positions:  positionsFromProto(s.GetPositions()),
		OpenOrders: ordersFromProto(s.GetOpenOrders()),
	}
}

func cancelReasonFromProto(r pb.CancelRejectReason) core.CancelReason {
	switch r {
	case pb.CancelRejectReason_CANCEL_REJECT_REASON_ORDER_NOT_FOUND:
		return core.CancelReasonNotFound
	case pb.CancelRejectReason_CANCEL_REJECT_REASON_ORDER_CLOSED:
		return core.CancelReasonClosed
	case pb.CancelRejectReason_CANCEL_REJECT_REASON_EXCHANGE_BUSY:
		return core.CancelReasonExchangeBusy
	case pb.CancelRejectReason_CANCEL_REJECT_REASON_EXCHANGE_UNAVAILABLE:
		return core.CancelReasonExchangeUnavailable
	case pb.CancelRejectReason_CANCEL_REJECT_REASON_UNSPECIFIED:
		return core.CancelReasonNone
	default:
		return core.CancelReasonUnknown
	}
}

func updateKindFromProto(k pb.OrderEvent_Kind) core.UpdateKind {
	switch k {
	case pb.OrderEvent_KIND_NEW:
		return core.UpdateNew
	case pb.OrderEvent_KIND_PARTIAL_FILL:
		return core.UpdatePartialFill
	case pb.OrderEvent_KIND_FILL:
		return core.UpdateFill
	case pb.OrderEvent_KIND_CANCELED:
		return core.UpdateCanceled
	case pb.OrderEvent_KIND_CANCEL_REJECTED:
		return core.UpdateCancelRejected
	case pb.OrderEvent_KIND_REJECTED:
		return core.UpdateRejected
	case pb.OrderEvent_KIND_EXPIRED:
		return core.UpdateExpired
	case pb.OrderEvent_KIND_REPLACED:
		return core.UpdateReplaced
	case pb.OrderEvent_KIND_REPLACE_REJECTED:
		return core.UpdateReplaceRejected
	default:
		return core.UpdateUnknown
	}
}

func updateFromProto(u *pb.AccountUpdate) core.AccountUpdate {
	out := core.AccountUpdate{
		Account:   core.AccountID(u.GetAccountId()),
		Seq:       u.GetAccountSeq(),
		TsMs:      u.GetTs(),
		Balances:  balancesFromProto(u.GetBalances()),
		Positions: positionsFromProto(u.GetPositions()),
	}
	switch cause := u.GetCause().(type) {
	case *pb.AccountUpdate_OrderEvent:
		e := cause.OrderEvent
		out.Kind = updateKindFromProto(e.GetKind())
		out.Order = orderFromProto(e.GetOrder())
		out.FillPriceMinor = e.GetFillPriceMinor()
		out.FillQty = e.GetFillQuantity()
	case *pb.AccountUpdate_Transfer:
		t := cause.Transfer
		out.Kind = core.UpdateTransfer
		out.Transfer = &core.Transfer{ID: t.GetTransferId(), Deposit: t.GetDeposit(), AmountMinor: t.GetAmountMinor()}
	}
	return out
}
