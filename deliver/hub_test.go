package deliver

import (
	"encoding/json"
	"errors"
	"order_gateway/core"
	"strconv"
	"sync"
	"testing"
	"time"
)

type fakeSocket struct {
	blocking bool
	written  chan []byte
	closed   chan struct{}
	once     sync.Once
}

func newFake(blocking bool) *fakeSocket {
	return &fakeSocket{blocking: blocking, written: make(chan []byte, 100), closed: make(chan struct{})}
}

func (f *fakeSocket) Write(d []byte) error {
	if f.blocking {
		<-f.closed // stuck until someone closes us, like a dead network
		return errors.New("closed")
	}
	f.written <- d
	return nil
}

func (f *fakeSocket) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func TestSendNeverBlocksAndSlowUserIsDropped(t *testing.T) {
	h := NewHub(2)
	alice := newFake(true)
	h.Add(1, alice)

	start := time.Now()
	for range 10 {
		h.Send(1, core.OutMsg{Kind: core.MsgOrderResult})
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("Send blocked on a slow user")
	}
	select {
	case <-alice.closed:
	case <-time.After(time.Second):
		t.Fatal("slow user was not disconnected")
	}
}

func TestSlowUserDoesNotDelayOthers(t *testing.T) {
	h := NewHub(2)
	h.Add(1, newFake(true)) // Alice, stuck
	bob := newFake(false)
	h.Add(2, bob)

	for range 10 {
		h.Send(1, core.OutMsg{Kind: core.MsgOrderResult})
	}
	h.Send(2, core.OutMsg{Kind: core.MsgOrderResult, ClientOrderID: "c-7"})

	select {
	case <-bob.written:
	case <-time.After(time.Second):
		t.Fatal("Bob's message was delayed")
	}
}

func TestSendToUnknownConnection(t *testing.T) {
	h := NewHub(2)
	if h.Send(99, core.OutMsg{}) {
		t.Fatal("expected false")
	}
}

func TestSendToClosedConnectionReturnsFalse(t *testing.T) {
	h := NewHub(2)
	alice := newFake(false)
	h.Add(1, alice)
	c := h.connections[1] // keep a handle, as Send would after its lookup
	h.Remove(1)
	h.mu.Lock()
	h.connections[1] = c // put it back so Send finds the closed connection
	h.mu.Unlock()

	if h.Send(1, core.OutMsg{Kind: core.MsgOrderResult}) {
		t.Fatal("Send returned true for a closed connection")
	}
}

func TestAddSameIDClosesOldConnection(t *testing.T) {
	h := NewHub(2)
	first := newFake(false)
	second := newFake(false)
	h.Add(1, first)
	h.Add(1, second)

	select {
	case <-first.closed:
	case <-time.After(time.Second):
		t.Fatal("old connection was not closed")
	}
	select {
	case <-second.closed:
		t.Fatal("new connection must stay open")
	default:
	}
}

func TestMessagesArriveInOrder(t *testing.T) {
	h := NewHub(10)
	bob := newFake(false)
	h.Add(1, bob)

	for i := 1; i <= 5; i++ {
		h.Send(1, core.OutMsg{Kind: core.MsgOrderResult, ClientOrderID: core.ClientOrderID(strconv.Itoa(i))})
	}
	for i := 1; i <= 5; i++ {
		select {
		case data := <-bob.written:
			var w struct {
				ClientOrderID string `json:"client_order_id"`
			}
			if err := json.Unmarshal(data, &w); err != nil {
				t.Fatal(err)
			}
			if want := strconv.Itoa(i); w.ClientOrderID != want {
				t.Fatalf("message %d: got client_order_id %s, want %s", i, w.ClientOrderID, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("message %d never arrived", i)
		}
	}
}

func TestConcurrentSendRemoveAdd(t *testing.T) {
	h := NewHub(4)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := core.ConnID(i % 5)
				switch (i + g) % 3 {
				case 0:
					h.Add(id, newFake(i%2 == 0))
				case 1:
					h.Send(id, core.OutMsg{Kind: core.MsgOrderResult})
				default:
					h.Remove(id)
				}
			}
		}(g)
	}
	wg.Wait()
	for id := core.ConnID(0); id < 5; id++ {
		h.Remove(id)
	}
}

func TestToWireRejected(t *testing.T) {
	got, err := json.Marshal(toWire(core.OutMsg{
		Kind:           core.MsgRejected,
		ClientOrderID:  "c-7",
		Reason:         core.ReasonInsufficientCash,
		ShortfallMinor: 5000,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"rejected","client_order_id":"c-7","reason":"insufficient_cash","shortfall_minor":5000}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestToWireOrderResultKeepsIDsExact(t *testing.T) {
	// client_order_id is passed through as-is (a UUID here); order_id is a 64-bit
	// number sent as a string, because a JSON number this size would lose its
	// last digit in JavaScript.
	got, err := json.Marshal(toWire(core.OutMsg{
		Kind:          core.MsgOrderResult,
		ClientOrderID: "123e4567-e89b-12d3-a456-426614174000",
		OrderID:       core.OrderID(1<<63 + 5),
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"order_result","client_order_id":"123e4567-e89b-12d3-a456-426614174000","order_id":"9223372036854775813"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestToWireEngineRejectHasReasonAndOrderID(t *testing.T) {
	got, _ := json.Marshal(toWire(core.OutMsg{
		Kind:          core.MsgOrderResult,
		ClientOrderID: "c-3",
		OrderID:       9,
		Reason:        core.ReasonEngineRejected,
	}))
	want := `{"type":"order_result","client_order_id":"c-3","order_id":"9","reason":"engine_rejected"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

type slowAwareSocket struct {
	*fakeSocket
	slowClosed chan struct{}
}

func (s *slowAwareSocket) CloseSlow() error {
	close(s.slowClosed)
	return s.fakeSocket.Close()
}

func TestRawMessageIsWrittenAsIs(t *testing.T) {
	h := NewHub(2)
	s := newFake(false)
	h.Add(1, s)
	h.Send(1, core.OutMsg{Kind: core.MsgRaw, Payload: []byte(`{"stream":"x"}`)})
	select {
	case got := <-s.written:
		if string(got) != `{"stream":"x"}` {
			t.Fatalf("wrote %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing written")
	}
}

// A full outbox closes the socket through CloseSlow when it has one (the
// trading stream uses it to close with 4008); an ordinary Remove does not.
func TestOverflowUsesCloseSlowButRemoveDoesNot(t *testing.T) {
	h := NewHub(1)
	slow := &slowAwareSocket{fakeSocket: newFake(true), slowClosed: make(chan struct{})}
	h.Add(1, slow)
	for range 10 {
		h.Send(1, core.OutMsg{Kind: core.MsgRaw, Payload: []byte("x")})
	}
	select {
	case <-slow.slowClosed:
	case <-time.After(time.Second):
		t.Fatal("overflow did not go through CloseSlow")
	}

	h2 := NewHub(1)
	calm := &slowAwareSocket{fakeSocket: newFake(false), slowClosed: make(chan struct{})}
	h2.Add(2, calm)
	h2.Remove(2)
	select {
	case <-calm.closed:
	case <-time.After(time.Second):
		t.Fatal("Remove did not close")
	}
	select {
	case <-calm.slowClosed:
		t.Fatal("a plain Remove must not count as slow")
	default:
	}
}
