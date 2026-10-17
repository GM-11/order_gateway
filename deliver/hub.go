package deliver

import (
	"encoding/json"
	"order_gateway/core"
	"sync"
)

type Socket interface {
	Write(data []byte) error
	Close() error
}

type SlowCloser interface {
	CloseSlow() error
}

type connection struct {
	socket Socket
	outbox chan core.OutMsg
	done   chan struct{}
	once   sync.Once
}

type Hub struct {
	mu          sync.Mutex
	connections map[core.ConnID]*connection
	outboxSize  int
}

func NewHub(outboxSize int) *Hub {
	return &Hub{
		connections: make(map[core.ConnID]*connection),
		outboxSize:  outboxSize,
	}
}

var _ core.Sender = (*Hub)(nil)

func (h *Hub) Add(id core.ConnID, s Socket) {
	c := &connection{
		socket: s,
		outbox: make(chan core.OutMsg, h.outboxSize),
		done:   make(chan struct{}),
	}
	h.mu.Lock()
	old := h.connections[id]
	h.connections[id] = c
	h.mu.Unlock()
	if old != nil {
		old.close(false)
	}
	go h.writeLoop(id, c)
}

func (h *Hub) writeLoop(id core.ConnID, c *connection) {
	for {
		select {
		case msg := <-c.outbox:
			data := msg.Payload
			if msg.Kind != core.MsgRaw {
				var err error
				if data, err = json.Marshal(toWire(msg)); err != nil {
					continue
				}
			}

			if err := c.socket.Write(data); err != nil {
				h.Remove(id)
				return
			}
		case <-c.done:
			return
		}
	}
}

func (h *Hub) Send(id core.ConnID, msg core.OutMsg) bool {
	h.mu.Lock()
	c, ok := h.connections[id]
	h.mu.Unlock()
	if !ok {
		return false
	}

	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case <-c.done:
		return false // connection already finished: don't pretend we queued it
	default:
	}

	select {
	case c.outbox <- msg:
		return true
	default:
		h.drop(id, true) // a user too slow to read is disconnected, never silently skipped
		return false
	}
}

func (h *Hub) Remove(id core.ConnID) { h.drop(id, false) }

func (h *Hub) drop(id core.ConnID, slow bool) {
	h.mu.Lock()
	c, ok := h.connections[id]
	delete(h.connections, id)
	h.mu.Unlock()
	if ok {
		c.close(slow)
	}
}

func (c *connection) close(slow bool) {
	c.once.Do(func() {
		close(c.done)
		if sc, ok := c.socket.(SlowCloser); ok && slow {
			sc.CloseSlow()
			return
		}
		c.socket.Close()
	})
}
