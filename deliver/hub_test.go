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
	for i := 0; i < 10; i++ {
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

	for i := 0; i < 10; i++ {
		h.Send(1, core.OutMsg{Kind: core.MsgOrderResult})
	}
	h.Send(2, core.OutMsg{Kind: core.MsgOrderResult, ClientRequestID: 7})

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
		h.Send(1, core.OutMsg{Kind: core.MsgOrderResult, ClientRequestID: core.ClientRequestID(i)})
	}
	for i := 1; i <= 5; i++ {
		select {
		case data := <-bob.written:
			var w struct {
				ClientRequestID string `json:"client_request_id"`
			}
			if err := json.Unmarshal(data, &w); err != nil {
				t.Fatal(err)
			}
			if want := strconv.Itoa(i); w.ClientRequestID != want {
				t.Fatalf("message %d: got request id %s, want %s", i, w.ClientRequestID, want)
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
		Kind:            core.MsgRejected,
		ClientRequestID: 7,
		Reason:          core.ReasonInsufficientCash,
		ShortfallMinor:  5000,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"rejected","client_request_id":"7","reason":"insufficient_cash","shortfall_minor":5000}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestToWireOrderResultKeepsBig64BitIDsExact(t *testing.T) {
	big := core.ClientRequestID(1<<62 + 1) // a JSON number this size would lose its last digit in JavaScript
	got, err := json.Marshal(toWire(core.OutMsg{
		Kind:            core.MsgOrderResult,
		ClientRequestID: big,
		OrderID:         core.OrderID(1<<63 + 5),
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"order_result","client_request_id":"4611686018427387905","order_id":"9223372036854775813"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestToWireEngineRejectHasReasonAndOrderID(t *testing.T) {
	got, _ := json.Marshal(toWire(core.OutMsg{
		Kind:            core.MsgOrderResult,
		ClientRequestID: 3,
		OrderID:         9,
		Reason:          core.ReasonEngineRejected,
	}))
	want := `{"type":"order_result","client_request_id":"3","order_id":"9","reason":"engine_rejected"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
