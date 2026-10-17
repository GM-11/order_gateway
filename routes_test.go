package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"order_gateway/auth"
	"order_gateway/core"
	"order_gateway/refdata"
)

type routeAuth struct{}

func (routeAuth) Verify(auth.Credentials) (auth.Principal, error) {
	return auth.Principal{Account: 17}, nil
}

type routePlacer struct{}

func (routePlacer) Place(context.Context, core.OrderRequest) (core.Result, error) {
	return core.Result{Outcome: core.OutcomePlaced, OrderID: 1}, nil
}

// routeCash records which query each endpoint made, so a test can tell which handler ran.
type routeCash struct {
	mu      sync.Mutex
	queries []core.OrdersQuery
	cancels []core.CancelRequest
}

func (c *routeCash) GetAccount(context.Context, core.AccountID) (core.AccountSnapshot, error) {
	return core.AccountSnapshot{Found: true, Account: 17}, nil
}
func (c *routeCash) GetOrders(_ context.Context, q core.OrdersQuery) ([]core.OrderState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, q)
	return []core.OrderState{{Symbol: 1, OrderID: 812, ClientOrderID: "abc", Status: core.StatusNew}}, nil
}
func (c *routeCash) CancelOrder(_ context.Context, r core.CancelRequest) (core.CancelAck, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels = append(c.cancels, r)
	return core.CancelAck{Accepted: true, Order: &core.OrderState{Symbol: 1, OrderID: 812}}, nil
}

func routeTable(t *testing.T) *refdata.Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "instruments.json")
	body := `{"version":1,"partitions":2,"instruments":[{"id":1,"ticker":"MOOG","partition":0,"band_bps":1000}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := refdata.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func serve(t *testing.T, cash *routeCash) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	registerRoutes(mux, routeAuth{}, routeTable(t), routePlacer{}, cash, nil)
	return mux
}

func do(mux http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// "/v2/orders:by_client_order_id" and "/v2/orders/{order_id}" are different
// paths; each must reach its own handler.
func TestRoutes_ByClientOrderIDAndByOrderIDDoNotCollide(t *testing.T) {
	cash := &routeCash{}
	mux := serve(t, cash)

	if w := do(mux, "GET", "/v2/orders:by_client_order_id?client_order_id=abc", ""); w.Code != 200 {
		t.Fatalf("by client id: %d %s", w.Code, w.Body)
	}
	if w := do(mux, "GET", "/v2/orders/1-812", ""); w.Code != 200 {
		t.Fatalf("by order id: %d %s", w.Code, w.Body)
	}
	if w := do(mux, "GET", "/v2/orders?status=all", ""); w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}

	if len(cash.queries) != 3 {
		t.Fatalf("%d cash queries, want 3", len(cash.queries))
	}
	if q := cash.queries[0]; q.ClientOrderID != "abc" || q.Order != nil {
		t.Errorf("by_client_order_id ran the wrong handler: %+v", q)
	}
	if q := cash.queries[1]; q.Order == nil || q.Order.OrderID != 812 || q.Order.Symbol != 1 || q.ClientOrderID != "" {
		t.Errorf("/v2/orders/{order_id} ran the wrong handler: %+v", q)
	}
	if q := cash.queries[2]; q.Filter != core.FilterAll || q.Order != nil || q.ClientOrderID != "" {
		t.Errorf("list ran the wrong handler: %+v", q)
	}
}

func TestRoutes_EveryContractEndpointIsRegistered(t *testing.T) {
	cash := &routeCash{}
	mux := serve(t, cash)
	post := `{"client_order_id":"a","symbol":"MOOG","side":"buy","type":"limit","time_in_force":"gtc","price":"1.00","qty":"1"}`
	cases := []struct {
		method, target, body string
		want                 int
	}{
		{"POST", "/v2/orders", post, 200},
		{"GET", "/v2/orders", "", 200},
		{"GET", "/v2/orders/1-812", "", 200},
		{"GET", "/v2/orders:by_client_order_id?client_order_id=abc", "", 200},
		{"DELETE", "/v2/orders/1-812", "", 202},
		{"PATCH", "/v2/orders/1-812", `{"qty":"5"}`, 422}, // [later]
		{"DELETE", "/v2/orders", "", 422},                 // [later]
		{"GET", "/v2/account", "", 200},
		{"GET", "/v2/positions", "", 200}, // empty list
		{"GET", "/v2/positions/MOOG", "", 404},
	}
	for _, c := range cases {
		if w := do(mux, c.method, c.target, c.body); w.Code != c.want {
			t.Errorf("%s %s: got %d, want %d: %s", c.method, c.target, w.Code, c.want, w.Body)
		}
	}
	if len(cash.cancels) != 1 || cash.cancels[0].OrderID != 812 {
		t.Errorf("DELETE /v2/orders/{id} did not reach CancelOrder: %+v", cash.cancels)
	}
}

func TestLoadConfigParsesStreamOrigins(t *testing.T) {
	c, err := loadConfig(env(map[string]string{
		"GATEWAY_ID": "gw-1", "API_KEYS_SOURCE": "keys.json", "STREAM_ORIGINS": " app.example.com, *.example.org ,,",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.StreamOrigins) != 2 || c.StreamOrigins[0] != "app.example.com" || c.StreamOrigins[1] != "*.example.org" {
		t.Errorf("StreamOrigins = %q", c.StreamOrigins)
	}
}
