package upstream

import (
	"context"
	"log"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"order_gateway/core"
	pb "order_gateway/gen/exchange/v1"
)

type EngineFeedReader struct {
	rpc        pb.EngineFeedClient
	gatewayID  string // MUST equal the id CashOwnerClient stamps, or replies go to nobody
	onReply    func(core.EngineReply)
	live       atomic.Bool // written by Serve, read by HTTP goroutines
	minBackoff time.Duration
	maxBackoff time.Duration
	healthyFor time.Duration // a stream that lived this long resets the backoff
}

func NewEngineFeedReader(rpc pb.EngineFeedClient, gatewayID string, onReply func(core.EngineReply)) *EngineFeedReader {
	return &EngineFeedReader{
		rpc:        rpc,
		gatewayID:  gatewayID,
		onReply:    onReply,
		minBackoff: 100 * time.Millisecond,
		maxBackoff: 5 * time.Second,
		healthyFor: 30 * time.Second,
	}
}

func (r *EngineFeedReader) Live() bool { return r.live.Load() }

func (r *EngineFeedReader) Serve(ctx context.Context) error {
	backoff := r.minBackoff
	for {
		started := time.Now()
		err := r.runOnce(ctx)
		r.live.Store(false) // whatever happened, we are not subscribed any more

		if ctx.Err() != nil {
			return ctx.Err() // shutting down
		}
		if status.Code(err) == codes.InvalidArgument {
			return err // config bug (empty gateway_id): retrying forever would hide it
		}
		if time.Since(started) >= r.healthyFor {
			backoff = r.minBackoff // it was working; this is a fresh failure
		}
		if status.Code(err) == codes.AlreadyExists {
			log.Printf("upstream: engine says gateway_id %q is already subscribed (old stream not reaped yet, or a DUPLICATE gateway id); retry in ~%v", r.gatewayID, backoff)
		} else {
			log.Printf("upstream: replies stream ended: %v; in-flight orders will time out as unknown; retry in ~%v", err, backoff)
		}

		timer := time.NewTimer(jitter(backoff))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		backoff = min(backoff*2, r.maxBackoff)
	}
}

func (r *EngineFeedReader) runOnce(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // tears down the RPC on return, so the engine frees our gateway_id

	stream, err := r.rpc.SubscribeReplies(ctx, &pb.RepliesSubscribeRequest{GatewayId: r.gatewayID})
	if err != nil {
		return err
	}

	md, err := stream.Header()
	if err != nil {
		return err
	}
	if md == nil {
		_, err := stream.Recv()
		return err
	}
	r.live.Store(true)

	for {
		p, err := stream.Recv()
		if err != nil {
			return err
		}
		if reply, ok := replyFromProto(p); ok {
			r.onReply(reply) // never blocks: see core.OnReply
		}
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2+1) // +1: rand.N panics on 0
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
