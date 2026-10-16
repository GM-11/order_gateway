package upstream

import (
	"context"
	"log"

	"order_gateway/core"
	pb "order_gateway/gen/exchange/v1"
)

type EngineFeedReader struct {
	rpc       pb.EngineFeedClient
	gatewayID string
	onReply   func(core.EngineReply)
}

func NewEngineFeedReader(rpc pb.EngineFeedClient, gatewayID string, onReply func(core.EngineReply)) *EngineFeedReader {
	return &EngineFeedReader{rpc: rpc, gatewayID: gatewayID, onReply: onReply}
}

func (r *EngineFeedReader) Run(ctx context.Context) error {
	stream, err := r.rpc.SubscribeReplies(ctx, &pb.RepliesSubscribeRequest{GatewayId: r.gatewayID})
	if err != nil {
		return err
	}
	for {
		p, err := stream.Recv() // *pb.Reply: this stream carries nothing else
		if err != nil {
			return err
		}
		reply, usable := replyFromProto(p)
		if usable {
			r.onReply(reply)
		}
	}
}

func replyFromProto(p *pb.Reply) (core.EngineReply, bool) {
	if p == nil || p.GetRejectReason() == pb.RejectReason_REJECT_REASON_UNSPECIFIED {
		log.Printf("upstream: dropping engine reply with unspecified reject reason")
		return core.EngineReply{}, false
	}
	return core.EngineReply{
		AccountID:     core.AccountID(p.GetAccountId()),
		ClientOrderID: core.ClientOrderID(p.GetClientOrderId()),
		OrderID:       core.OrderID(p.GetOrderId()),
		Rejected:      p.GetRejectReason() != pb.RejectReason_REJECT_REASON_NONE,
	}, true
}
