package upstream

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"order_gateway/core"
	pb "order_gateway/gen/exchange/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

type fakeCashServer struct {
	pb.UnimplementedCashOwnerServer
	ack   *pb.PlaceOrderAck
	err   error
	delay time.Duration
	got   []*pb.PlaceOrderRequest
}

func (f *fakeCashServer) PlaceOrder(ctx context.Context, r *pb.PlaceOrderRequest) (*pb.PlaceOrderAck, error) {
	f.got = append(f.got, r)
	if f.delay > 0 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.ack, nil
}

func dial(t *testing.T, srv *fakeCashServer) *CashOwnerClient {
	t.Helper()
	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	pb.RegisterCashOwnerServer(server, srv)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewCashOwnerClient(pb.NewCashOwnerClient(conn), "gw-1")
}

func TestCashOwnerSubmitTranslatesFields(t *testing.T) {
	srv := &fakeCashServer{ack: &pb.PlaceOrderAck{Accepted: true}}
	client := dial(t, srv)
	req := core.OrderRequest{
		AccountID:     12,
		ClientOrderID: "c-34",
		Symbol:        56,
		Side:          core.Sell,
		PriceMinor:    7800,
		Qty:           9,
	}
	ack, err := client.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !ack.Accepted {
		t.Fatalf("ack = %+v, want accepted", ack)
	}
	if len(srv.got) != 1 {
		t.Fatalf("server got %d requests, want 1", len(srv.got))
	}
	got := srv.got[0]
	if got.AccountId != 12 || got.ClientOrderId != "c-34" || got.Symbol != 56 || got.Side != pb.Side_SIDE_SELL || got.PriceMinor != 7800 || got.Quantity != 9 || got.GatewayId != "gw-1" {
		t.Fatalf("server received %+v, fields not translated as expected", got)
	}
}

func TestCashOwnerSubmitMapsKnownReject(t *testing.T) {
	client := dial(t, &fakeCashServer{ack: &pb.PlaceOrderAck{
		Accepted:       false,
		Reason:         pb.CashRejectReason_CASH_REJECT_REASON_INSUFFICIENT_CASH,
		ShortfallMinor: 35000,
	}})
	ack, err := client.Submit(context.Background(), core.OrderRequest{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if ack.Accepted || ack.Reason != core.ReasonInsufficientCash || ack.ShortFallMinor != 35000 {
		t.Fatalf("ack = %+v, want cash rejection with shortfall 35000", ack)
	}
}

func TestCashOwnerSubmitMapsUnknownReject(t *testing.T) {
	client := dial(t, &fakeCashServer{ack: &pb.PlaceOrderAck{
		Accepted: false,
		Reason:   pb.CashRejectReason(99),
	}})
	ack, err := client.Submit(context.Background(), core.OrderRequest{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if ack.Accepted || ack.Reason != core.ReasonUnknown {
		t.Fatalf("ack = %+v, want unknown rejection", ack)
	}
}

func TestCashOwnerSubmitWrapsStatusError(t *testing.T) {
	client := dial(t, &fakeCashServer{err: status.Error(codes.Unavailable, "down")})
	_, err := client.Submit(context.Background(), core.OrderRequest{})
	if !errors.Is(err, core.ErrNoAnswer) {
		t.Fatalf("error = %v, want errors.Is(ErrNoAnswer)", err)
	}
}

func TestCashOwnerSubmitWrapsDeadline(t *testing.T) {
	client := dial(t, &fakeCashServer{ack: &pb.PlaceOrderAck{Accepted: true}, delay: 200 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Submit(ctx, core.OrderRequest{})
	if !errors.Is(err, core.ErrNoAnswer) {
		t.Fatalf("error = %v, want errors.Is(ErrNoAnswer)", err)
	}
}
