package upstream

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"order_gateway/core"
	pb "order_gateway/gen/exchange/v1"
)

type replyStream = grpc.ServerStreamingServer[pb.Reply]

// fakeFeed is a scripted engine. handle gets the 1-based call number, so a
// test can make the 1st subscribe fail and the 2nd succeed, and so on.
type fakeFeed struct {
	pb.UnimplementedEngineFeedServer
	calls     atomic.Int32
	gatewayID atomic.Value // string: the last gateway_id asked for
	handle    func(call int, stream replyStream) error
}

func (f *fakeFeed) SubscribeReplies(req *pb.RepliesSubscribeRequest, stream replyStream) error {
	f.gatewayID.Store(req.GetGatewayId())
	return f.handle(int(f.calls.Add(1)), stream)
}

// registered does what the real engine does once it has subscribed us:
// send headers straight away.
func registered(stream replyStream) error {
	return stream.SendHeader(metadata.MD{})
}

// idle keeps the stream open, quiet, until the client goes away.
func idle(stream replyStream) error {
	<-stream.Context().Done()
	return status.Error(codes.Canceled, "client went away")
}

// waitOr blocks until ch closes or the client goes away.
func waitOr(stream replyStream, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-stream.Context().Done():
		return status.Error(codes.Canceled, "client went away")
	}
}

func send(stream replyStream, replies ...*pb.Reply) error {
	for _, r := range replies {
		if err := stream.Send(r); err != nil {
			return err
		}
	}
	return nil
}

func reply(clientOrderID string, reason pb.RejectReason) *pb.Reply {
	return &pb.Reply{GatewayId: "gw-1", ClientOrderId: clientOrderID, AccountId: 7, OrderId: 100, RejectReason: reason}
}

type collector struct {
	mu  sync.Mutex
	got []core.EngineReply
}

func (c *collector) add(r core.EngineReply) {
	c.mu.Lock()
	c.got = append(c.got, r)
	c.mu.Unlock()
}

func (c *collector) snapshot() []core.EngineReply {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]core.EngineReply(nil), c.got...)
}

func (c *collector) len() int { return len(c.snapshot()) }

func newFeedReader(t *testing.T, f *fakeFeed, onReply func(core.EngineReply)) *EngineFeedReader {
	t.Helper()
	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	pb.RegisterEngineFeedServer(server, f)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	r := NewEngineFeedReader(pb.NewEngineFeedClient(conn), "gw-1", onReply)
	r.minBackoff = time.Millisecond // fast retries in tests
	r.maxBackoff = 5 * time.Millisecond
	return r
}

// start runs Serve in the background. The returned stop cancels it and
// returns Serve's error; it also runs at cleanup (before the server closes).
func start(t *testing.T, r *EngineFeedReader) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx) }()

	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(2 * time.Second):
				t.Error("Serve did not return after cancel")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEngineFeedDeliversRepliesInOrder(t *testing.T) {
	f := &fakeFeed{handle: func(_ int, s replyStream) error {
		if err := registered(s); err != nil {
			return err
		}
		if err := send(s,
			reply("a", pb.RejectReason_REJECT_REASON_NONE),
			reply("b", pb.RejectReason_REJECT_REASON_PRICE_BAND),
			reply("c", pb.RejectReason_REJECT_REASON_NONE),
		); err != nil {
			return err
		}
		return idle(s)
	}}
	var got collector
	r := newFeedReader(t, f, got.add)
	stop := start(t, r)

	waitFor(t, "3 replies", func() bool { return got.len() == 3 })
	replies := got.snapshot()
	for i, want := range []string{"a", "b", "c"} {
		if string(replies[i].ClientOrderID) != want {
			t.Fatalf("reply %d = %q, want %q (order must be kept)", i, replies[i].ClientOrderID, want)
		}
	}
	if replies[0].Rejected || !replies[1].Rejected || replies[2].Rejected {
		t.Errorf("Rejected flags = %v %v %v, want false true false", replies[0].Rejected, replies[1].Rejected, replies[2].Rejected)
	}
	if replies[0].AccountID != 7 || replies[0].OrderID != 100 {
		t.Errorf("fields not mapped: %+v", replies[0])
	}
	if got := f.gatewayID.Load(); got != "gw-1" {
		t.Errorf("subscribed as %v, want gw-1", got)
	}
	if !r.Live() {
		t.Error("Live() = false while the stream is open")
	}

	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Serve returned %v, want context.Canceled", err)
	}
	if r.Live() {
		t.Error("Live() = true after Serve returned")
	}
}

func TestEngineFeedRefusedStreamIsNeverLive(t *testing.T) {
	f := &fakeFeed{handle: func(int, replyStream) error {
		return status.Error(codes.AlreadyExists, "gateway gw-1 already has a SubscribeReplies stream")
	}}
	r := newFeedReader(t, f, func(core.EngineReply) {})

	err := r.runOnce(context.Background())
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("runOnce = %v, want AlreadyExists", err)
	}
	// runOnce never resets live itself, so if it had wrongly set it, it would still be true.
	if r.Live() {
		t.Fatal("a refused stream was marked live")
	}
}

func TestEngineFeedRetriesUntilRegistered(t *testing.T) {
	f := &fakeFeed{handle: func(call int, s replyStream) error {
		if call <= 2 {
			return status.Error(codes.AlreadyExists, "old stream not reaped yet")
		}
		if err := registered(s); err != nil {
			return err
		}
		return idle(s)
	}}
	r := newFeedReader(t, f, func(core.EngineReply) {})
	start(t, r)

	waitFor(t, "live after two refusals", r.Live)
	if n := f.calls.Load(); n != 3 {
		t.Errorf("subscribe calls = %d, want 3", n)
	}
}

func TestEngineFeedReconnectsAfterStreamBreaks(t *testing.T) {
	f := &fakeFeed{handle: func(call int, s replyStream) error {
		if err := registered(s); err != nil {
			return err
		}
		if call == 1 {
			if err := send(s, reply("before", pb.RejectReason_REJECT_REASON_NONE)); err != nil {
				return err
			}
			return status.Error(codes.Unavailable, "engine restarting")
		}
		if err := send(s, reply("after", pb.RejectReason_REJECT_REASON_NONE)); err != nil {
			return err
		}
		return idle(s)
	}}
	var got collector
	r := newFeedReader(t, f, got.add)
	start(t, r)

	waitFor(t, "a reply from each stream", func() bool { return got.len() == 2 })
	replies := got.snapshot()
	if replies[0].ClientOrderID != "before" || replies[1].ClientOrderID != "after" {
		t.Errorf("got %q then %q, want before then after", replies[0].ClientOrderID, replies[1].ClientOrderID)
	}
	if n := f.calls.Load(); n != 2 {
		t.Errorf("subscribe calls = %d, want 2", n)
	}
}

func TestEngineFeedNotLiveWhileReconnecting(t *testing.T) {
	end := make(chan struct{})     // closing it breaks stream 1
	release := make(chan struct{}) // closing it lets stream 2 register
	f := &fakeFeed{handle: func(call int, s replyStream) error {
		if call == 1 {
			if err := registered(s); err != nil {
				return err
			}
			if err := waitOr(s, end); err != nil {
				return err
			}
			return status.Error(codes.Unavailable, "engine restarting")
		}
		// Stream 2 is open but the engine has not registered it yet.
		if err := waitOr(s, release); err != nil {
			return err
		}
		if err := registered(s); err != nil {
			return err
		}
		return idle(s)
	}}
	r := newFeedReader(t, f, func(core.EngineReply) {})
	start(t, r)

	waitFor(t, "first stream live", r.Live)
	close(end)
	waitFor(t, "second subscribe call", func() bool { return f.calls.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	if r.Live() {
		t.Fatal("Live() = true while the new stream is open but not yet registered")
	}
	close(release)
	waitFor(t, "second stream live", r.Live)
}

func TestEngineFeedStopsOnInvalidArgument(t *testing.T) {
	f := &fakeFeed{handle: func(int, replyStream) error {
		return status.Error(codes.InvalidArgument, "gateway_id must not be empty")
	}}
	r := newFeedReader(t, f, func(core.EngineReply) {})

	done := make(chan error, 1)
	go func() { done <- r.Serve(context.Background()) }()
	select {
	case err := <-done:
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("Serve = %v, want InvalidArgument", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve kept retrying a config error")
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("subscribe calls = %d, want 1 (no retry)", n)
	}
}

func TestEngineFeedShutdownWhileIdle(t *testing.T) {
	f := &fakeFeed{handle: func(_ int, s replyStream) error {
		if err := registered(s); err != nil {
			return err
		}
		return idle(s)
	}}
	r := newFeedReader(t, f, func(core.EngineReply) {})
	stop := start(t, r)

	waitFor(t, "live", r.Live)
	began := time.Now()
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Serve = %v, want context.Canceled", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("shutdown took %v", took)
	}
	if r.Live() {
		t.Error("Live() = true after shutdown")
	}
}

func TestEngineFeedSkipsUnspecifiedReplies(t *testing.T) {
	f := &fakeFeed{handle: func(_ int, s replyStream) error {
		if err := registered(s); err != nil {
			return err
		}
		if err := send(s,
			reply("bad", pb.RejectReason_REJECT_REASON_UNSPECIFIED),
			reply("good", pb.RejectReason_REJECT_REASON_NONE),
		); err != nil {
			return err
		}
		return idle(s)
	}}
	var got collector
	r := newFeedReader(t, f, got.add)
	start(t, r)

	// Replies arrive in order, so once "good" is here, "bad" was already handled.
	waitFor(t, "a reply", func() bool { return got.len() >= 1 })
	replies := got.snapshot()
	if len(replies) != 1 || replies[0].ClientOrderID != "good" {
		t.Errorf("got %+v, want only the good reply", replies)
	}
}

func TestJitterStaysInRange(t *testing.T) {
	for _, d := range []time.Duration{0, 1, 2, 3, time.Millisecond, 5 * time.Second} {
		for i := 0; i < 1000; i++ {
			if j := jitter(d); j < d/2 || j > d {
				t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, j, d/2, d)
			}
		}
	}
}
