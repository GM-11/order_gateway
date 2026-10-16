package core

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Stateless fake: shares nothing, so the only shared state is the Gateway's map.
type okCash struct{}

func (okCash) Submit(context.Context, OrderRequest) (Acknowledge, error) {
	return Acknowledge{Accepted: true}, nil
}

// 1,000 handlers call Place while the engine-feed goroutine calls OnReply,
// like real traffic. Run with -race. Some replies arrive before their note
// exists and are dropped; those Place calls time out. Either way: no data
// race, no freeze, and no note left behind.
func TestPlaceAndOnReplyConcurrently(t *testing.T) {
	gw := New(okCash{}, time.Now, 200*time.Millisecond)
	const n = 1000
	var wg sync.WaitGroup

	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(i int) { // one goroutine per HTTP request
			defer wg.Done()
			gw.Place(context.Background(), OrderRequest{AccountID: 7, ClientOrderID: ClientOrderID(strconv.Itoa(i))})
		}(i)
	}
	wg.Go(func() { // the single engine-feed reader
		for i := 1; i <= n; i++ {
			gw.OnReply(EngineReply{AccountID: 7, ClientOrderID: ClientOrderID(strconv.Itoa(i)), OrderID: OrderID(i)})
		}
	})
	wg.Wait()

	if c := pendingCount(gw); c != 0 {
		t.Fatalf("every way out must clean up: %d notes left", c)
	}
}
