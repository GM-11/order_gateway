package upstream

import (
	"context"
	"fmt"

	"order_gateway/core"
	pb "order_gateway/gen/exchange/v1"
)

type CashOwnerClient struct {
	rpc       pb.CashOwnerClient
	gatewayID string
}

var _ core.CashOwner = (*CashOwnerClient)(nil)

func NewCashOwnerClient(rpc pb.CashOwnerClient, gatewayID string) *CashOwnerClient {
	return &CashOwnerClient{rpc: rpc, gatewayID: gatewayID}
}

func (c *CashOwnerClient) Submit(ctx context.Context, req core.OrderRequest) (core.Acknowledge, error) {
	resp, err := c.rpc.PlaceOrder(ctx, c.toProto(req))
	if err != nil {
		return core.Acknowledge{}, fmt.Errorf("%w: %v", core.ErrNoAnswer, err)
	}
	return fromProto(resp), nil
}

func (c *CashOwnerClient) toProto(r core.OrderRequest) *pb.PlaceOrderRequest {
	side := pb.Side_SIDE_BUY
	if r.Side == core.Sell {
		side = pb.Side_SIDE_SELL
	}
	return &pb.PlaceOrderRequest{
		AccountId:     uint64(r.AccountID),
		ClientOrderId: string(r.ClientOrderID),
		Symbol:        uint32(r.Symbol),
		Side:          side,
		PriceMinor:    r.PriceMinor,
		Quantity:      r.Qty,
		GatewayId:     c.gatewayID,
	}
}

func fromProto(a *pb.PlaceOrderAck) core.Acknowledge {
	if a.GetAccepted() {
		return core.Acknowledge{Accepted: true, ShortFallMinor: a.GetShortfallMinor()}
	}
	reason := core.ReasonUnknown
	switch a.GetReason() {
	case pb.CashRejectReason_CASH_REJECT_REASON_INSUFFICIENT_CASH:
		reason = core.ReasonInsufficientCash
	case pb.CashRejectReason_CASH_REJECT_REASON_INSUFFICIENT_HOLDINGS:
		reason = core.ReasonInsufficientHoldings
	}
	return core.Acknowledge{Accepted: false, Reason: reason, ShortFallMinor: a.GetShortfallMinor()}
}
