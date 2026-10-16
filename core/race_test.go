package core

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Stateless fakes: they share nothing, so the only shared state left
// is the Gateway's own pending map. (fakeCash/fakeSender use append,
// which would race too and muddy the result.)
type okCash struct{}

func (okCash) Submit(ctx context.Context, r OrderRequest) (Acknowledge, error) {
	return Acknowledge{Accepted: true}, nil
}

type nopSender struct{}

func (nopSender) Send(ConnID, OutMsg) bool { return true }

// Two goroutines touch the Gateway at once, like real main.go will:
//   - browser reader:     Alice's orders arrive  -> OnPlace (adds to pending)
//   - engine-feed reader: engine replies arrive  -> OnReply (reads + deletes)
//
// Run with: go test -race -run TestTwoGoroutinesTouchPending -v ./core
// Expected today: DATA RACE + FAIL. That is the bug LLD #6 fixes.
func TestTwoGoroutinesTouchPending(t *testing.T) {
	gw := New(okCash{}, nopSender{}, time.Now, time.Second)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() { // browser reader
		defer wg.Done()
		for i := 1; i <= 1000; i++ {
			gw.OnPlace(context.Background(), 3, OrderRequest{AccountID: 7, ClientOrderID: ClientOrderID(strconv.Itoa(i))})
		}
	}()
	go func() { // engine-feed reader
		defer wg.Done()
		for i := 1; i <= 1000; i++ {
			gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: ClientOrderID(strconv.Itoa(i)), OrderID: OrderID(i)})
		}
	}()
	wg.Wait()
}
