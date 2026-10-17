// Package stream is the trading WebSocket, GET /v2/stream (contract 3.3).
//
// A connection: auth -> listen -> account_snapshot, then every AccountUpdate
// for the user's account arrives as one trade_updates message (cause and
// balances together, never torn). The gateway never reorders, merges or drops
// updates for a connected user; gaps are the client's to detect by account_seq
// and to repair with {"action":"resync"}. A user too slow to read is
// disconnected (4008), never silently skipped.
package stream

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"order_gateway/auth"
	"order_gateway/core"
	"order_gateway/deliver"
	"order_gateway/wire"
)

// Close codes the contract names.
const (
	CloseUnauthorized websocket.StatusCode = 4001 // no/bad auth, or auth took longer than 5 s
	CloseSlowConsumer websocket.StatusCode = 4008 // outbox full
)

const (
	defaultAuthTimeout = 5 * time.Second
	readLimit          = 8 << 10
	writeTimeout       = 10 * time.Second
	snapshotTimeout    = 5 * time.Second
)

// Hub is the per-connection bounded outbox + writer (deliver.Hub).
type Hub interface {
	Add(id core.ConnID, s deliver.Socket)
	Send(id core.ConnID, msg core.OutMsg) bool
	Remove(id core.ConnID)
}

// Cash is the one read the stream needs: the snapshot that the deltas fit.
type Cash interface {
	GetAccount(ctx context.Context, account core.AccountID) (core.AccountSnapshot, error)
}

type Server struct {
	Auth           auth.Verifier
	Cash           Cash
	Hub            Hub
	Tick           wire.Ticker
	AuthTimeout    time.Duration // 0 = 5 s
	OriginPatterns []string      // extra browser origins allowed to connect (default: same host only)

	next atomic.Uint64

	MaxHeld int // updates held per connection while its snapshot is in flight; 0 = 1000

	mu        sync.Mutex                                    // guards everything below, and makes check-and-send atomic
	byAccount map[core.AccountID]map[core.ConnID]*connState // listening connections only
	conns     map[core.ConnID]*connState
}

const defaultMaxHeld = 1000

// connState is one listening connection. While holding, a snapshot for it is
// being fetched: updates are queued, not sent, so that the first frame after
// a snapshot with account_seq N is update N+1 (contract 3.3).
type connState struct {
	account   core.AccountID
	holding   bool
	gen       uint64 // bumped per snapshot; an older snapshot's result is discarded
	held      []heldUpdate
	dead      bool // overflowed or failed: already being closed with 4008
	closeSlow func()
}

type heldUpdate struct {
	seq     uint64
	payload []byte
}

var _ http.Handler = (*Server)(nil)

type socket struct{ conn *websocket.Conn }

func (s *socket) Write(data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, data)
}

func (s *socket) Close() error { return s.conn.CloseNow() }

// CloseSlow is called by the Hub from inside Send, which must never block, so
// the closing handshake runs on its own goroutine.
func (s *socket) CloseSlow() error {
	go func() { _ = s.conn.Close(CloseSlowConsumer, "too slow to read; reconnect and resync") }()
	return nil
}

type clientMessage struct {
	Action string `json:"action"`
	KeyID  string `json:"key_id"`
	Secret string `json:"secret"`
	Token  string `json:"token"`
	Data   struct {
		Streams []string `json:"streams"`
	} `json:"data"`
}

func (s *Server) authTimeout() time.Duration {
	if s.AuthTimeout > 0 {
		return s.AuthTimeout
	}
	return defaultAuthTimeout
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.OriginPatterns})
	if err != nil {
		return // Accept has already answered the HTTP request
	}
	conn.SetReadLimit(readLimit)
	defer conn.CloseNow()

	principal, ok := s.handshake(r.Context(), conn)
	if !ok {
		return
	}

	id := core.ConnID(s.next.Add(1))
	sock := &socket{conn: conn}
	s.Hub.Add(id, sock)
	defer func() {
		s.unregister(id)
		s.Hub.Remove(id)
	}()
	s.send(id, frame("authorization", map[string]string{"status": "authorized"}))

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var m clientMessage
		if err := json.Unmarshal(data, &m); err != nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "messages must be JSON objects")
			return
		}
		switch m.Action {
		case "auth":
			// already authorized; nothing to do
		case "listen":
			s.listen(id, principal.Account, m.Data.Streams, func() { _ = sock.CloseSlow() })
		case "resync":
			s.resync(id)
		default:
			_ = conn.Close(websocket.StatusPolicyViolation, "unknown action")
			return
		}
	}
}

// handshake reads the first message, which must be a good auth, within the
// timeout. On any failure the connection is closed with 4001 here. These
// frames are written directly: the Hub only takes over once the user is in.
func (s *Server) handshake(ctx context.Context, conn *websocket.Conn) (auth.Principal, bool) {
	// Not a read deadline: cancelling a coder/websocket read tears the
	// connection down without a close frame, and the client must see 4001.
	timedOut := time.AfterFunc(s.authTimeout(), func() {
		_ = conn.Close(CloseUnauthorized, "auth required within the time limit")
	})
	_, data, err := conn.Read(ctx)
	if !timedOut.Stop() || err != nil {
		return auth.Principal{}, false // timed out (already closed with 4001), or the client left
	}

	var m clientMessage
	creds := auth.Credentials{}
	if json.Unmarshal(data, &m) == nil && m.Action == "auth" {
		creds = auth.Credentials{KeyID: m.KeyID, Secret: m.Secret, Bearer: m.Token}
	}
	p, err := s.Auth.Verify(creds)
	if err != nil {
		wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
		defer wcancel()
		_ = conn.Write(wctx, websocket.MessageText, frame("authorization", map[string]string{"status": "unauthorized"}))
		_ = conn.Close(CloseUnauthorized, "unauthorized")
		return auth.Principal{}, false
	}
	return p, true
}

func (s *Server) send(id core.ConnID, payload []byte) bool {
	return s.Hub.Send(id, core.OutMsg{Kind: core.MsgRaw, Payload: payload})
}

// listen answers a listen request: `listening`, then registration (which
// starts holding updates), then the snapshot.
func (s *Server) listen(id core.ConnID, account core.AccountID, streams []string, closeSlow func()) {
	granted := []string{}
	for _, name := range streams {
		if name == "trade_updates" {
			granted = append(granted, name)
		}
	}
	s.send(id, frame("listening", map[string][]string{"streams": granted}))
	if len(granted) == 0 {
		return
	}
	gen := s.register(id, account, closeSlow)
	s.snapshot(id, account, gen)
}

func (s *Server) resync(id core.ConnID) {
	s.mu.Lock()
	st := s.conns[id]
	if st == nil || st.dead {
		s.mu.Unlock()
		return // not listening: nothing to resync
	}
	gen := s.begin(st)
	account := st.account
	s.mu.Unlock()
	s.snapshot(id, account, gen)
}

// begin marks the connection as having a snapshot in flight. Caller holds s.mu.
func (s *Server) begin(st *connState) uint64 {
	st.holding = true
	st.held = nil
	st.gen++
	return st.gen
}

// register adds a listening connection, already holding. Re-listening on a
// registered connection just starts a new snapshot.
func (s *Server) register(id core.ConnID, account core.AccountID, closeSlow func()) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		s.conns = make(map[core.ConnID]*connState)
		s.byAccount = make(map[core.AccountID]map[core.ConnID]*connState)
	}
	st := s.conns[id]
	if st == nil {
		st = &connState{account: account, closeSlow: closeSlow}
		s.conns[id] = st
		if s.byAccount[account] == nil {
			s.byAccount[account] = make(map[core.ConnID]*connState)
		}
		s.byAccount[account][id] = st
	}
	return s.begin(st)
}

func (s *Server) unregister(id core.ConnID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.conns[id]
	if !ok {
		return
	}
	delete(s.conns, id)
	delete(s.byAccount[st.account], id)
	if len(s.byAccount[st.account]) == 0 {
		delete(s.byAccount, st.account)
	}
}

// snapshot fetches the account and delivers it for the given generation.
func (s *Server) snapshot(id core.ConnID, account core.AccountID, gen uint64) {
	payload, seq, ok := s.fetchSnapshot(account)
	if !ok {
		s.failSnapshot(id, gen)
		return
	}
	s.finishSnapshot(id, gen, payload, seq)
}

// finishSnapshot pushes the snapshot, then the held updates newer than it, in
// order, then stops holding. All under s.mu, as OnUpdate's check-and-send is,
// so no update slips in between. Hub.Send never blocks.
func (s *Server) finishSnapshot(id core.ConnID, gen uint64, payload []byte, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.conns[id]
	if st == nil || st.dead || st.gen != gen {
		return // gone, failed, or a later snapshot superseded this one
	}
	s.send(id, payload)
	for _, h := range st.held {
		if h.seq > seq {
			s.send(id, h.payload)
		}
	}
	st.held = nil
	st.holding = false
}

// failSnapshot: a client without a snapshot has nothing to apply updates to,
// so it is closed (4008) and reconnects.
func (s *Server) failSnapshot(id core.ConnID, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.conns[id]; st != nil && !st.dead && st.gen == gen {
		s.kill(st)
	}
}

// kill closes a connection with 4008. Caller holds s.mu.
func (s *Server) kill(st *connState) {
	st.dead, st.holding, st.held = true, false, nil
	st.closeSlow()
}

func (s *Server) connsOf(account core.AccountID) []core.ConnID {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]core.ConnID, 0, len(s.byAccount[account]))
	for id := range s.byAccount[account] {
		ids = append(ids, id)
	}
	return ids
}

// fetchSnapshot asks the cash owner for the account as it is now.
func (s *Server) fetchSnapshot(account core.AccountID) ([]byte, uint64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	defer cancel()
	snap, err := s.Cash.GetAccount(ctx, account)
	if err != nil {
		log.Printf("stream: snapshot for account %d failed: %v", account, err)
		return nil, 0, false
	}
	if !snap.Found { // a brand-new account: zero balances, nothing held, nothing open
		snap = core.AccountSnapshot{Found: true, Account: account}
	}
	return snapshotFrame(snap, s.Tick), snap.Seq, true
}

func (s *Server) OnUpdate(u core.AccountUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.byAccount[u.Account]) == 0 {
		return
	}
	limit := s.MaxHeld
	if limit <= 0 {
		limit = defaultMaxHeld
	}
	payload := updateFrame(u, s.Tick)
	for id, st := range s.byAccount[u.Account] {
		switch {
		case st.dead:
		case st.holding:
			if len(st.held) >= limit {
				s.kill(st) // never drop silently: the client reconnects and gets a fresh snapshot
			} else {
				st.held = append(st.held, heldUpdate{u.Seq, payload})
			}
		default:
			s.send(id, payload)
		}
	}
}

// OnLive runs after every (re)connect of the account-updates stream. That
// stream is live-only, so whatever happened while it was down is lost:
// re-snapshot every connected user. Holding starts before it returns, so no
// update after this call can overtake a snapshot; the reads happen in the
// background so the stream's read loop is not held up.
func (s *Server) OnLive() {
	type job struct {
		id  core.ConnID
		gen uint64
	}
	jobs := map[core.AccountID][]job{}
	s.mu.Lock()
	for account, conns := range s.byAccount {
		for id, st := range conns {
			if !st.dead {
				jobs[account] = append(jobs[account], job{id, s.begin(st)})
			}
		}
	}
	s.mu.Unlock()

	for account, js := range jobs {
		go func() {
			payload, seq, ok := s.fetchSnapshot(account)
			for _, j := range js {
				if ok {
					s.finishSnapshot(j.id, j.gen, payload, seq)
				} else {
					s.failSnapshot(j.id, j.gen)
				}
			}
		}()
	}
}
