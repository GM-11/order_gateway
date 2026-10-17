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

	mu        sync.Mutex
	byAccount map[core.AccountID]map[core.ConnID]struct{} // listening connections only
	accountOf map[core.ConnID]core.AccountID
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
	s.Hub.Add(id, &socket{conn: conn})
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
			if !s.listen(id, principal.Account, m.Data.Streams) {
				_ = conn.Close(websocket.StatusInternalError, "cash owner unavailable")
				return
			}
		case "resync":
			if !s.isListening(id) {
				continue
			}
			if !s.sendSnapshot(id, principal.Account) {
				_ = conn.Close(websocket.StatusInternalError, "cash owner unavailable")
				return
			}
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

// listen answers a listen request: `listening`, then registration, then the
// snapshot. Registering before the snapshot means no update is missed; an
// update that beats the snapshot to the client is simply older than it.
func (s *Server) listen(id core.ConnID, account core.AccountID, streams []string) bool {
	granted := []string{}
	for _, name := range streams {
		if name == "trade_updates" {
			granted = append(granted, name)
		}
	}
	s.send(id, frame("listening", map[string][]string{"streams": granted}))
	if len(granted) == 0 {
		return true
	}
	s.register(id, account)
	return s.sendSnapshot(id, account)
}

func (s *Server) register(id core.ConnID, account core.AccountID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byAccount == nil {
		s.byAccount = make(map[core.AccountID]map[core.ConnID]struct{})
		s.accountOf = make(map[core.ConnID]core.AccountID)
	}
	if s.byAccount[account] == nil {
		s.byAccount[account] = make(map[core.ConnID]struct{})
	}
	s.byAccount[account][id] = struct{}{}
	s.accountOf[id] = account
}

func (s *Server) unregister(id core.ConnID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accountOf[id]
	if !ok {
		return
	}
	delete(s.accountOf, id)
	delete(s.byAccount[account], id)
	if len(s.byAccount[account]) == 0 {
		delete(s.byAccount, account)
	}
}

func (s *Server) isListening(id core.ConnID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.accountOf[id]
	return ok
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

// snapshotFrame asks the cash owner for the account as it is now.
func (s *Server) snapshotFrame(account core.AccountID) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	defer cancel()
	snap, err := s.Cash.GetAccount(ctx, account)
	if err != nil {
		log.Printf("stream: snapshot for account %d failed: %v", account, err)
		return nil, false
	}
	if !snap.Found { // a brand-new account: zero balances, nothing held, nothing open
		snap = core.AccountSnapshot{Found: true, Account: account}
	}
	return snapshotFrame(snap, s.Tick), true
}

func (s *Server) sendSnapshot(id core.ConnID, account core.AccountID) bool {
	payload, ok := s.snapshotFrame(account)
	if !ok {
		return false
	}
	s.send(id, payload)
	return true
}

// OnUpdate routes one AccountUpdate to every connection of that account. The
// cash owner sends every account's updates to every gateway; updates for
// accounts nobody here is listening for are dropped (not ours to keep).
// It never blocks: Hub.Send drops a slow connection instead of waiting.
func (s *Server) OnUpdate(u core.AccountUpdate) {
	ids := s.connsOf(u.Account)
	if len(ids) == 0 {
		return
	}
	payload := updateFrame(u, s.Tick)
	for _, id := range ids {
		s.send(id, payload)
	}
}

// OnLive runs after every (re)connect of the account-updates stream. That
// stream is live-only, so whatever happened while it was down is lost:
// re-snapshot every connected user. It returns at once (the reads happen in
// the background) so the stream's read loop is not held up.
func (s *Server) OnLive() {
	s.mu.Lock()
	accounts := make([]core.AccountID, 0, len(s.byAccount))
	for account := range s.byAccount {
		accounts = append(accounts, account)
	}
	s.mu.Unlock()

	for _, account := range accounts {
		go func() {
			payload, ok := s.snapshotFrame(account)
			if !ok {
				return // the user will see a gap on the next update and resync
			}
			for _, id := range s.connsOf(account) {
				s.send(id, payload)
			}
		}()
	}
}
