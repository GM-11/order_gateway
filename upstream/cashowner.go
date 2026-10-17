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

// Every gRPC error below becomes core.ErrNoAnswer: "no answer, outcome unknown",
// never a business answer.
func noAnswer(err error) error { return fmt.Errorf("%w: %v", core.ErrNoAnswer, err) }

func (c *CashOwnerClient) Submit(ctx context.Context, req core.OrderRequest) (core.Acknowledge, error) {
	resp, err := c.rpc.PlaceOrder(ctx, c.toProto(req))
	if err != nil {
		return core.Acknowledge{}, noAnswer(err)
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
		// Spelled out, not left UNSPECIFIED: only limit + gtc are built today.
		TimeInForce: pb.TimeInForce_TIME_IN_FORCE_GTC,
		Type:        pb.OrderType_ORDER_TYPE_LIMIT,
	}
}

func fromProto(a *pb.PlaceOrderAck) core.Acknowledge {
	ack := core.Acknowledge{
		Accepted:       a.GetAccepted(),
		ShortFallMinor: a.GetShortfallMinor(),
		Duplicate:      a.GetDuplicate(),
		Order:          orderFromProto(a.GetOrder()),
	}
	if !a.GetAccepted() {
		ack.Reason = cashRejectFromProto(a.GetReason())
		if ack.Reason == core.ReasonNone {
			ack.Reason = core.ReasonUnknown // refused, but no reason we recognise
		}
	}
	return ack
}

func (c *CashOwnerClient) CancelOrder(ctx context.Context, req core.CancelRequest) (core.CancelAck, error) {
	resp, err := c.rpc.CancelOrder(ctx, &pb.CancelOrderRequest{
		AccountId: uint64(req.Account),
		Symbol:    uint32(req.Symbol),
		OrderId:   uint64(req.OrderID),
		GatewayId: c.gatewayID,
	})
	if err != nil {
		return core.CancelAck{}, noAnswer(err)
	}
	return core.CancelAck{
		Accepted: resp.GetAccepted(),
		Reason:   cancelReasonFromProto(resp.GetReason()),
		Order:    orderFromProto(resp.GetOrder()),
	}, nil
}

func (c *CashOwnerClient) GetAccount(ctx context.Context, account core.AccountID) (core.AccountSnapshot, error) {
	resp, err := c.rpc.GetAccount(ctx, &pb.GetAccountRequest{AccountId: uint64(account)})
	if err != nil {
		return core.AccountSnapshot{}, noAnswer(err)
	}
	return snapshotFromProto(resp), nil
}

func (c *CashOwnerClient) GetOrders(ctx context.Context, q core.OrdersQuery) ([]core.OrderState, error) {
	req := &pb.GetOrdersRequest{AccountId: uint64(q.Account)}
	switch q.Filter {
	case core.FilterOpen:
		req.Filter = pb.GetOrdersRequest_FILTER_OPEN
	case core.FilterClosed:
		req.Filter = pb.GetOrdersRequest_FILTER_CLOSED
	case core.FilterAll:
		req.Filter = pb.GetOrdersRequest_FILTER_ALL
	}
	if q.Order != nil {
		id, sym := uint64(q.Order.OrderID), uint32(q.Order.Symbol)
		req.OrderId, req.Symbol = &id, &sym
	}
	if q.ClientOrderID != "" {
		coid := string(q.ClientOrderID)
		req.ClientOrderId = &coid
	}
	resp, err := c.rpc.GetOrders(ctx, req)
	if err != nil {
		return nil, noAnswer(err)
	}
	return ordersFromProto(resp.GetOrders()), nil
}
