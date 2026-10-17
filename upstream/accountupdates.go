package upstream

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"order_gateway/core"
	pb "order_gateway/gen/exchange/v1"
)

// AccountUpdatesReader keeps one long-lived CashOwner.SubscribeAccountUpdates
// stream open. The cash owner sends EVERY account's updates to every
// subscribed gateway; the callbacks decide who is connected here. The stream
// is live-only: after every (re)connect onLive runs so connected users can be
// re-snapshotted.
type AccountUpdatesReader struct {
	rpc        pb.CashOwnerClient
	gatewayID  string
	onUpdate   func(core.AccountUpdate)
	onLive     func()
	live       atomic.Bool
	minBackoff time.Duration
	maxBackoff time.Duration
	healthyFor time.Duration
}

// onUpdate and onLive must not block: they run on the stream's read loop.
func NewAccountUpdatesReader(rpc pb.CashOwnerClient, gatewayID string, onUpdate func(core.AccountUpdate), onLive func()) *AccountUpdatesReader {
	return &AccountUpdatesReader{
		rpc:        rpc,
		gatewayID:  gatewayID,
		onUpdate:   onUpdate,
		onLive:     onLive,
		minBackoff: 100 * time.Millisecond,
		maxBackoff: 5 * time.Second,
		healthyFor: 30 * time.Second,
	}
}

func (r *AccountUpdatesReader) Live() bool { return r.live.Load() }

func (r *AccountUpdatesReader) Serve(ctx context.Context) error {
	backoff := r.minBackoff
	for {
		started := time.Now()
		err := r.runOnce(ctx)
		r.live.Store(false)

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if status.Code(err) == codes.InvalidArgument {
			return err // config bug (empty gateway_id): retrying forever would hide it
		}
		if time.Since(started) >= r.healthyFor {
			backoff = r.minBackoff
		}
		log.Printf("upstream: account updates stream ended: %v; connected users will be re-snapshotted on reconnect; retry in ~%v", err, backoff)

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

func (r *AccountUpdatesReader) runOnce(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := r.rpc.SubscribeAccountUpdates(ctx, &pb.AccountUpdatesRequest{GatewayId: r.gatewayID})
	if err != nil {
		return err
	}
	// Headers arrive once the cash owner has registered us. Same rule as the
	// engine replies stream: not live until then.
	md, err := stream.Header()
	if err != nil {
		return err
	}
	if md == nil {
		_, err := stream.Recv()
		return err
	}
	r.live.Store(true)
	if r.onLive != nil {
		r.onLive()
	}

	for {
		u, err := stream.Recv()
		if err != nil {
			return err
		}
		r.onUpdate(updateFromProto(u))
	}
}
