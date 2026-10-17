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

// scriptedCash is a cash owner whose every answer the test sets.
type scriptedCash struct {
	pb.UnimplementedCashOwnerServer
	place   func(*pb.PlaceOrderRequest) (*pb.PlaceOrderAck, error)
	cancel  func(*pb.CancelOrderRequest) (*pb.CancelOrderAck, error)
	account func(*pb.GetAccountRequest) (*pb.AccountSnapshot, error)
	orders  func(*pb.GetOrdersRequest) (*pb.GetOrdersResponse, error)
	updates func(call int, stream grpc.ServerStreamingServer[pb.AccountUpdate]) error
	calls   atomic.Int32
	gotGW   atomic.Value
}

func (s *scriptedCash) PlaceOrder(_ context.Context, r *pb.PlaceOrderRequest) (*pb.PlaceOrderAck, error) {
	return s.place(r)
}
func (s *scriptedCash) CancelOrder(_ context.Context, r *pb.CancelOrderRequest) (*pb.CancelOrderAck, error) {
	return s.cancel(r)
}
func (s *scriptedCash) GetAccount(_ context.Context, r *pb.GetAccountRequest) (*pb.AccountSnapshot, error) {
	return s.account(r)
}
func (s *scriptedCash) GetOrders(_ context.Context, r *pb.GetOrdersRequest) (*pb.GetOrdersResponse, error) {
	return s.orders(r)
}
func (s *scriptedCash) SubscribeAccountUpdates(r *pb.AccountUpdatesRequest, stream grpc.ServerStreamingServer[pb.AccountUpdate]) error {
	s.gotGW.Store(r.GetGatewayId())
	return s.updates(int(s.calls.Add(1)), stream)
}

func dialScripted(t *testing.T, srv *scriptedCash) (*CashOwnerClient, pb.CashOwnerClient) {
	t.Helper()
	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	pb.RegisterCashOwnerServer(server, srv)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	rpc := pb.NewCashOwnerClient(conn)
	return NewCashOwnerClient(rpc, "gw-1"), rpc
}

func i64(v int64) *int64 { return &v }

func TestSubmitSendsLimitGTCExplicitly(t *testing.T) {
	var got *pb.PlaceOrderRequest
	c, _ := dialScripted(t, &scriptedCash{place: func(r *pb.PlaceOrderRequest) (*pb.PlaceOrderAck, error) {
		got = r
		return &pb.PlaceOrderAck{Accepted: true}, nil
	}})
	if _, err := c.Submit(context.Background(), core.OrderRequest{AccountID: 1, ClientOrderID: "a", Symbol: 1, Side: core.Buy, PriceMinor: 100, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if got.GetType() != pb.OrderType_ORDER_TYPE_LIMIT || got.GetTimeInForce() != pb.TimeInForce_TIME_IN_FORCE_GTC {
		t.Errorf("type=%v tif=%v, want LIMIT and GTC spelled out (not UNSPECIFIED)", got.GetType(), got.GetTimeInForce())
	}
	if got.StopPriceMinor != nil {
		t.Error("stop_price_minor must stay unset")
	}
}

func TestSubmitMapsEveryCashRejectReason(t *testing.T) {
	cases := map[pb.CashRejectReason]core.RejectReason{
		pb.CashRejectReason_CASH_REJECT_REASON_INSUFFICIENT_CASH:     core.ReasonInsufficientCash,
		pb.CashRejectReason_CASH_REJECT_REASON_INSUFFICIENT_HOLDINGS: core.ReasonInsufficientHoldings,
		pb.CashRejectReason_CASH_REJECT_REASON_UNKNOWN_ACCOUNT:       core.ReasonUnknownAccount,
		pb.CashRejectReason_CASH_REJECT_REASON_EXCHANGE_BUSY:         core.ReasonExchangeBusy,
		pb.CashRejectReason_CASH_REJECT_REASON_UNKNOWN_SYMBOL:        core.ReasonUnknownSymbol,
		pb.CashRejectReason_CASH_REJECT_REASON_EXCHANGE_UNAVAILABLE:  core.ReasonExchangeUnavailable,
		pb.CashRejectReason_CASH_REJECT_REASON_NOT_SUPPORTED:         core.ReasonNotSupported,
		pb.CashRejectReason_CASH_REJECT_REASON_UNSPECIFIED:           core.ReasonUnknown, // refused with no reason: never "success"
		pb.CashRejectReason(99):                                      core.ReasonUnknown,
	}
	for in, want := range cases {
		c, _ := dialScripted(t, &scriptedCash{place: func(*pb.PlaceOrderRequest) (*pb.PlaceOrderAck, error) {
			return &pb.PlaceOrderAck{Accepted: false, Reason: in}, nil
		}})
		ack, err := c.Submit(context.Background(), core.OrderRequest{})
		if err != nil || ack.Accepted || ack.Reason != want {
			t.Errorf("%v: got %+v / %v, want reason %d", in, ack, err, want)
		}
	}
}

func TestSubmitCarriesTheDuplicateAndItsOrder(t *testing.T) {
	c, _ := dialScripted(t, &scriptedCash{place: func(*pb.PlaceOrderRequest) (*pb.PlaceOrderAck, error) {
		return &pb.PlaceOrderAck{Accepted: true, Duplicate: true, Order: &pb.OrderState{
			AccountId: 7, ClientOrderId: "c", Symbol: 1, OrderId: 812, Side: pb.Side_SIDE_SELL,
			PriceMinor: 10050, StopPriceMinor: i64(9900), Quantity: 10, FilledQuantity: 4, FilledNotionalMinor: 40000,
			Status: pb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, CreatedTs: 1000, UpdatedTs: 2000,
			EngineRejectReason: pb.RejectReason_REJECT_REASON_PRICE_BAND, CashRejectReason: pb.CashRejectReason_CASH_REJECT_REASON_INSUFFICIENT_CASH,
		}}, nil
	}})
	ack, err := c.Submit(context.Background(), core.OrderRequest{})
	if err != nil || !ack.Duplicate || ack.Order == nil {
		t.Fatalf("got %+v / %v", ack, err)
	}
	o := *ack.Order
	if o.Account != 7 || o.ClientOrderID != "c" || o.Symbol != 1 || o.OrderID != 812 || o.Side != core.Sell ||
		o.Type != core.TypeLimit || o.TimeInForce != core.TIFGoodTilCancelled || // UNSPECIFIED = LIMIT / GTC
		o.PriceMinor != 10050 || o.StopPriceMinor == nil || *o.StopPriceMinor != 9900 ||
		o.Qty != 10 || o.FilledQty != 4 || o.FilledNotionalMinor != 40000 || o.Status != core.StatusPartiallyFilled ||
		o.EngineReject != core.EngineRejectPriceBand || o.CashReject != core.ReasonInsufficientCash ||
		o.CreatedTs != 1000 || o.UpdatedTs != 2000 {
		t.Errorf("order not translated: %+v", o)
	}
}

func TestCancelOrder(t *testing.T) {
	var got *pb.CancelOrderRequest
	c, _ := dialScripted(t, &scriptedCash{cancel: func(r *pb.CancelOrderRequest) (*pb.CancelOrderAck, error) {
		got = r
		return &pb.CancelOrderAck{Accepted: true, Order: &pb.OrderState{OrderId: 812, Status: pb.OrderStatus_ORDER_STATUS_PENDING_CANCEL}}, nil
	}})
	ack, err := c.CancelOrder(context.Background(), core.CancelRequest{Account: 7, Symbol: 1, OrderID: 812})
	if err != nil || !ack.Accepted || ack.Order == nil || ack.Order.Status != core.StatusPendingCancel {
		t.Fatalf("got %+v / %v", ack, err)
	}
	if got.GetAccountId() != 7 || got.GetSymbol() != 1 || got.GetOrderId() != 812 || got.GetGatewayId() != "gw-1" {
		t.Errorf("request = %+v", got)
	}
}

func TestCancelOrderReasons(t *testing.T) {
	cases := map[pb.CancelRejectReason]core.CancelReason{
		pb.CancelRejectReason_CANCEL_REJECT_REASON_ORDER_NOT_FOUND:      core.CancelReasonNotFound,
		pb.CancelRejectReason_CANCEL_REJECT_REASON_ORDER_CLOSED:         core.CancelReasonClosed,
		pb.CancelRejectReason_CANCEL_REJECT_REASON_EXCHANGE_BUSY:        core.CancelReasonExchangeBusy,
		pb.CancelRejectReason_CANCEL_REJECT_REASON_EXCHANGE_UNAVAILABLE: core.CancelReasonExchangeUnavailable,
		pb.CancelRejectReason_CANCEL_REJECT_REASON_UNSPECIFIED:          core.CancelReasonNone,
		pb.CancelRejectReason(77):                                       core.CancelReasonUnknown,
	}
	for in, want := range cases {
		c, _ := dialScripted(t, &scriptedCash{cancel: func(*pb.CancelOrderRequest) (*pb.CancelOrderAck, error) {
			return &pb.CancelOrderAck{Reason: in}, nil
		}})
		ack, err := c.CancelOrder(context.Background(), core.CancelRequest{})
		if err != nil || ack.Accepted || ack.Reason != want {
			t.Errorf("%v: got %+v / %v, want %d", in, ack, err, want)
		}
	}
}

func TestEveryReadAndCancelErrorBecomesNoAnswer(t *testing.T) {
	boom := status.Error(codes.Unavailable, "down")
	c, _ := dialScripted(t, &scriptedCash{
		cancel:  func(*pb.CancelOrderRequest) (*pb.CancelOrderAck, error) { return nil, boom },
		account: func(*pb.GetAccountRequest) (*pb.AccountSnapshot, error) { return nil, boom },
		orders:  func(*pb.GetOrdersRequest) (*pb.GetOrdersResponse, error) { return nil, boom },
	})
	ctx := context.Background()
	if _, err := c.CancelOrder(ctx, core.CancelRequest{}); !errors.Is(err, core.ErrNoAnswer) {
		t.Errorf("CancelOrder: %v", err)
	}
	if _, err := c.GetAccount(ctx, 1); !errors.Is(err, core.ErrNoAnswer) {
		t.Errorf("GetAccount: %v", err)
	}
	if _, err := c.GetOrders(ctx, core.OrdersQuery{Filter: core.FilterAll}); !errors.Is(err, core.ErrNoAnswer) {
		t.Errorf("GetOrders: %v", err)
	}
}

func TestGetAccount(t *testing.T) {
	c, _ := dialScripted(t, &scriptedCash{account: func(r *pb.GetAccountRequest) (*pb.AccountSnapshot, error) {
		if r.GetAccountId() != 17 {
			return nil, status.Error(codes.InvalidArgument, "wrong account")
		}
		return &pb.AccountSnapshot{
			Found: true, AccountId: 17, AccountSeq: 41,
			Balances:   &pb.Balances{CashFreeMinor: 100, CashLockedMinor: 20, RealizedPnlMinor: -3},
			Positions:  []*pb.Position{{Symbol: 1, FreeQty: 4, LockedQty: 6, AvgPriceMinor: 9800, RealizedPnlMinor: 300}},
			OpenOrders: []*pb.OrderState{{OrderId: 812, Symbol: 1}},
		}, nil
	}})
	s, err := c.GetAccount(context.Background(), 17)
	if err != nil || !s.Found || s.Seq != 41 || s.Balances != (core.Balances{CashFreeMinor: 100, CashLockedMinor: 20, RealizedPnlMinor: -3}) ||
		len(s.Positions) != 1 || s.Positions[0] != (core.Position{Symbol: 1, FreeQty: 4, LockedQty: 6, AvgPriceMinor: 9800, RealizedPnlMinor: 300}) ||
		len(s.OpenOrders) != 1 || s.OpenOrders[0].OrderID != 812 {
		t.Errorf("got %+v / %v", s, err)
	}
}

func TestGetOrdersFilters(t *testing.T) {
	var got *pb.GetOrdersRequest
	c, _ := dialScripted(t, &scriptedCash{orders: func(r *pb.GetOrdersRequest) (*pb.GetOrdersResponse, error) {
		got = r
		return &pb.GetOrdersResponse{Orders: []*pb.OrderState{{OrderId: 1}, {OrderId: 2}}}, nil
	}})
	ctx := context.Background()

	for filter, want := range map[core.OrdersFilter]pb.GetOrdersRequest_Filter{
		core.FilterOpen: pb.GetOrdersRequest_FILTER_OPEN, core.FilterClosed: pb.GetOrdersRequest_FILTER_CLOSED, core.FilterAll: pb.GetOrdersRequest_FILTER_ALL,
	} {
		orders, err := c.GetOrders(ctx, core.OrdersQuery{Account: 5, Filter: filter})
		if err != nil || len(orders) != 2 || got.GetFilter() != want || got.GetAccountId() != 5 || got.OrderId != nil || got.ClientOrderId != nil {
			t.Errorf("filter %d: got %+v / %v, request %+v", filter, orders, err, got)
		}
	}

	_, _ = c.GetOrders(ctx, core.OrdersQuery{Account: 5, Filter: core.FilterAll, Order: &core.OrderRef{Symbol: 2, OrderID: 812}})
	if got.OrderId == nil || *got.OrderId != 812 || got.Symbol == nil || *got.Symbol != 2 {
		t.Errorf("by order id: %+v", got)
	}
	_, _ = c.GetOrders(ctx, core.OrdersQuery{Account: 5, Filter: core.FilterAll, ClientOrderID: "abc"})
	if got.ClientOrderId == nil || *got.ClientOrderId != "abc" {
		t.Errorf("by client id: %+v", got)
	}
}

// ── account updates reader ───────────────────────────────────────────────────

type updateStream = grpc.ServerStreamingServer[pb.AccountUpdate]

type updateLog struct {
	mu      sync.Mutex
	updates []core.AccountUpdate
	lives   int
}

func (l *updateLog) onUpdate(u core.AccountUpdate) {
	l.mu.Lock()
	l.updates = append(l.updates, u)
	l.mu.Unlock()
}
func (l *updateLog) onLive() { l.mu.Lock(); l.lives++; l.mu.Unlock() }
func (l *updateLog) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.updates), l.lives
}

func newUpdatesReader(t *testing.T, srv *scriptedCash, l *updateLog) *AccountUpdatesReader {
	t.Helper()
	_, rpc := dialScripted(t, srv)
	r := NewAccountUpdatesReader(rpc, "gw-1", l.onUpdate, l.onLive)
	r.minBackoff, r.maxBackoff = time.Millisecond, 5*time.Millisecond
	return r
}

func runReader(t *testing.T, r *AccountUpdatesReader) (stop func() error) {
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
				t.Error("Serve did not return")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func orderUpdate(seq uint64, kind pb.OrderEvent_Kind) *pb.AccountUpdate {
	return &pb.AccountUpdate{
		AccountId: 7, AccountSeq: seq, Ts: 1_000,
		Cause: &pb.AccountUpdate_OrderEvent{OrderEvent: &pb.OrderEvent{
			Kind: kind, Order: &pb.OrderState{OrderId: 812, Symbol: 1}, FillPriceMinor: 9800, FillQuantity: 6, EngineSeq: 55,
		}},
		Balances:  &pb.Balances{CashFreeMinor: 100},
		Positions: []*pb.Position{{Symbol: 1, FreeQty: 6}},
	}
}

func TestAccountUpdatesAreConvertedAndDeliveredInOrder(t *testing.T) {
	srv := &scriptedCash{updates: func(_ int, s updateStream) error {
		if err := s.SendHeader(metadata.MD{}); err != nil {
			return err
		}
		_ = s.Send(orderUpdate(1, pb.OrderEvent_KIND_NEW))
		_ = s.Send(orderUpdate(2, pb.OrderEvent_KIND_PARTIAL_FILL))
		_ = s.Send(&pb.AccountUpdate{AccountId: 7, AccountSeq: 3, Cause: &pb.AccountUpdate_Transfer{
			Transfer: &pb.TransferEvent{TransferId: "t-1", Deposit: true, AmountMinor: 5000}}, Balances: &pb.Balances{}})
		<-s.Context().Done()
		return status.Error(codes.Canceled, "bye")
	}}
	var l updateLog
	r := newUpdatesReader(t, srv, &l)
	stop := runReader(t, r)

	waitFor(t, "3 updates", func() bool { n, _ := l.counts(); return n == 3 })
	l.mu.Lock()
	u := append([]core.AccountUpdate(nil), l.updates...)
	l.mu.Unlock()
	if u[0].Seq != 1 || u[1].Seq != 2 || u[2].Seq != 3 {
		t.Errorf("order not kept: %d %d %d", u[0].Seq, u[1].Seq, u[2].Seq)
	}
	if u[0].Kind != core.UpdateNew || u[1].Kind != core.UpdatePartialFill || u[2].Kind != core.UpdateTransfer {
		t.Errorf("kinds: %d %d %d", u[0].Kind, u[1].Kind, u[2].Kind)
	}
	if u[1].Account != 7 || u[1].FillPriceMinor != 9800 || u[1].FillQty != 6 || u[1].Order == nil || u[1].Order.OrderID != 812 ||
		u[1].Balances.CashFreeMinor != 100 || len(u[1].Positions) != 1 || u[1].Positions[0].FreeQty != 6 || u[1].TsMs != 1000 {
		t.Errorf("fields not mapped: %+v", u[1])
	}
	if u[2].Transfer == nil || u[2].Transfer.ID != "t-1" || !u[2].Transfer.Deposit || u[2].Transfer.AmountMinor != 5000 || u[2].Order != nil {
		t.Errorf("transfer not mapped: %+v", u[2])
	}
	if got := srv.gotGW.Load(); got != "gw-1" {
		t.Errorf("subscribed as %v", got)
	}
	if !r.Live() {
		t.Error("Live() = false on an open stream")
	}
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Serve returned %v", err)
	}
	if r.Live() {
		t.Error("Live() = true after Serve returned")
	}
}

func TestAccountUpdatesEveryKindIsMapped(t *testing.T) {
	cases := map[pb.OrderEvent_Kind]core.UpdateKind{
		pb.OrderEvent_KIND_NEW: core.UpdateNew, pb.OrderEvent_KIND_PARTIAL_FILL: core.UpdatePartialFill,
		pb.OrderEvent_KIND_FILL: core.UpdateFill, pb.OrderEvent_KIND_CANCELED: core.UpdateCanceled,
		pb.OrderEvent_KIND_CANCEL_REJECTED: core.UpdateCancelRejected, pb.OrderEvent_KIND_REJECTED: core.UpdateRejected,
		pb.OrderEvent_KIND_EXPIRED: core.UpdateExpired, pb.OrderEvent_KIND_REPLACED: core.UpdateReplaced,
		pb.OrderEvent_KIND_REPLACE_REJECTED: core.UpdateReplaceRejected, pb.OrderEvent_KIND_UNSPECIFIED: core.UpdateUnknown,
	}
	for in, want := range cases {
		if got := updateFromProto(orderUpdate(1, in)).Kind; got != want {
			t.Errorf("%v -> %d, want %d", in, got, want)
		}
	}
}

// The stream is live-only: every (re)connect must tell the owner of the reader (onLive),
// and Live() is false while it is down.
func TestAccountUpdatesReconnectCallsOnLiveAgain(t *testing.T) {
	srv := &scriptedCash{updates: func(call int, s updateStream) error {
		if call == 1 {
			return status.Error(codes.Unavailable, "restarting") // no headers: never live
		}
		if err := s.SendHeader(metadata.MD{}); err != nil {
			return err
		}
		if call == 2 {
			_ = s.Send(orderUpdate(1, pb.OrderEvent_KIND_NEW))
			return status.Error(codes.Unavailable, "dropped") // live, then lost
		}
		_ = s.Send(orderUpdate(2, pb.OrderEvent_KIND_NEW))
		<-s.Context().Done()
		return status.Error(codes.Canceled, "bye")
	}}
	var l updateLog
	r := newUpdatesReader(t, srv, &l)
	runReader(t, r)

	waitFor(t, "second live stream", func() bool { n, lives := l.counts(); return n == 2 && lives == 2 })
	if _, lives := l.counts(); lives != 2 {
		t.Errorf("onLive ran %d times, want 2 (once per live connection, not for the failed attempt)", lives)
	}
	if !r.Live() {
		t.Error("Live() = false on the open third stream")
	}
}

func TestAccountUpdatesInvalidArgumentEndsServe(t *testing.T) {
	srv := &scriptedCash{updates: func(int, updateStream) error { return status.Error(codes.InvalidArgument, "empty gateway_id") }}
	var l updateLog
	r := newUpdatesReader(t, srv, &l)
	done := make(chan error, 1)
	go func() { done <- r.Serve(context.Background()) }()
	select {
	case err := <-done:
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve kept retrying a config bug")
	}
}

func TestReplyWithEmptyClientOrderIDIsDropped(t *testing.T) {
	if _, ok := replyFromProto(&pb.Reply{ClientOrderId: "", RejectReason: pb.RejectReason_REJECT_REASON_NONE, OrderId: 5}); ok {
		t.Error("a cancel/modify answer must be ignored")
	}
	r, ok := replyFromProto(&pb.Reply{ClientOrderId: "a", RejectReason: pb.RejectReason_REJECT_REASON_SYMBOL_HALTED})
	if !ok || !r.Rejected || r.EngineReject != core.EngineRejectSymbolHalted {
		t.Errorf("got %+v / %v", r, ok)
	}
}
