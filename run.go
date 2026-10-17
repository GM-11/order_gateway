package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"order_gateway/auth"
	"order_gateway/core"
	"order_gateway/deliver"
	pb "order_gateway/gen/exchange/v1"
	"order_gateway/handlers"
	"order_gateway/refdata"
	"order_gateway/stream"
	"order_gateway/upstream"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	symbols, err := refdata.Load(cfg.Instruments)
	if err != nil {
		return fmt.Errorf("load instruments %s: %w", cfg.Instruments, err)
	}

	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()

	apiKeys := auth.NewAPIKeys(keySource(cfg.APIKeys), log)
	refreshOnce(ctx, "api keys", apiKeys.Refresh, log)
	go apiKeys.Run(bg, 30*time.Second)
	chain := auth.Chain{APIKeys: apiKeys}

	if cfg.JWKSURL != "" {
		jwks := auth.NewJWKSCache(cfg.JWKSURL, &http.Client{Timeout: 5 * time.Second}, 10*time.Second, time.Now, log)
		refreshOnce(ctx, "jwks", jwks.Refresh, log)
		go jwks.Run(bg, 5*time.Minute)
		chain.JWT = auth.NewJWTVerifier(jwks, cfg.JWTIssuer, cfg.JWTAudience, 30*time.Second, time.Now)
	}

	cashConn, err := grpc.NewClient(cfg.CashOwnerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("cash owner client: %w", err)
	}
	defer cashConn.Close()
	feedConn, err := grpc.NewClient(cfg.EngineFeedAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("engine feed client: %w", err)
	}
	defer feedConn.Close()

	cash := upstream.NewCashOwnerClient(pb.NewCashOwnerClient(cashConn), cfg.GatewayID)
	gw := core.New(cash, time.Now, cfg.OrderTimeout)
	reader := upstream.NewEngineFeedReader(pb.NewEngineFeedClient(feedConn), cfg.GatewayID, gw.OnReply)
	readerDone := make(chan error, 1)
	go func() { readerDone <- reader.Serve(bg) }()

	tick := func(id core.SymbolID) (string, bool) {
		inst, ok := symbols.ByID(id)
		return inst.Ticker, ok
	}
	trading := &stream.Server{
		Auth:           chain,
		Cash:           cash,
		Hub:            deliver.NewHub(streamOutbox),
		Tick:           tick,
		OriginPatterns: cfg.StreamOrigins,
	}
	updates := upstream.NewAccountUpdatesReader(pb.NewCashOwnerClient(cashConn), cfg.GatewayID, trading.OnUpdate, trading.OnLive)
	updatesDone := make(chan error, 1)
	go func() { updatesDone <- updates.Serve(bg) }()

	mux := http.NewServeMux()
	registerRoutes(mux, chain, symbols, gw, cash, reader.Live)
	mux.Handle("GET /v2/stream", trading)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /readyz", readyHandler(map[string]func() bool{
		"auth":            chain.Ready,
		"engine_feed":     reader.Live,
		"account_updates": updates.Live,
	}))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.OrderTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.HTTPAddr, err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()

	log.Info("order gateway up",
		"gateway_id", cfg.GatewayID,
		"http", ln.Addr().String(),
		"cash_owner", cfg.CashOwnerAddr,
		"engine_feed", cfg.EngineFeedAddr,
		"instruments", cfg.Instruments,
		"browser_login", cfg.JWKSURL != "",
	)

	var runErr error
	readerExited, updatesExited := false, false
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveDone:
		runErr = fmt.Errorf("http server: %w", err)
	case err := <-readerDone:
		readerExited = true
		runErr = fmt.Errorf("engine feed reader: %w", err)
	case err := <-updatesDone:
		updatesExited = true
		runErr = fmt.Errorf("account updates reader: %w", err)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.OrderTimeout+5*time.Second)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Warn("http drain did not finish", "err", err)
	}
	stopBG()
	if !readerExited {
		<-readerDone
	}
	if !updatesExited {
		<-updatesDone
	}
	log.Info("stopped")
	return runErr
}

func keySource(src string) auth.KeySource {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return auth.HTTPKeySource{URL: src, Client: &http.Client{Timeout: 5 * time.Second}}
	}
	return auth.FileKeySource{Path: src}
}

func refreshOnce(ctx context.Context, what string, refresh func(context.Context) error, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := refresh(ctx); err != nil {
		log.Warn("first load failed; starting anyway, /readyz stays 503 until it loads", "what", what, "err", err)
	}
}

func readyHandler(checks map[string]func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var down []string
		for name, ok := range checks {
			if !ok() {
				down = append(down, name)
			}
		}
		if len(down) > 0 {
			sort.Strings(down)
			http.Error(w, "not ready: "+strings.Join(down, ", "), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}
}

// streamOutbox is how many frames a trading-stream connection may have queued
// before it counts as too slow to read and is disconnected (4008).
const streamOutbox = 256

// registerRoutes wires every REST endpoint of contract 3.2. Go 1.22+ patterns:
// "/v2/orders:by_client_order_id" is a different path from "/v2/orders/{order_id}",
// so the two never compete.
func registerRoutes(mux *http.ServeMux, v auth.Verifier, symbols *refdata.Table, placer handlers.Placer, cash handlers.Cash, feedLive func() bool) {
	mux.Handle("POST /v2/orders", &handlers.Orders{Auth: v, Symbols: symbols, Placer: placer, FeedLive: feedLive})
	t := &handlers.Trading{Auth: v, Symbols: symbols, Cash: cash}
	mux.HandleFunc("GET /v2/orders", t.ListOrders)
	mux.HandleFunc("GET /v2/orders:by_client_order_id", t.GetOrderByClientOrderID)
	mux.HandleFunc("GET /v2/orders/{order_id}", t.GetOrder)
	mux.HandleFunc("DELETE /v2/orders/{order_id}", t.CancelOrder)
	mux.HandleFunc("PATCH /v2/orders/{order_id}", t.NotSupportedYet) // [later]
	mux.HandleFunc("DELETE /v2/orders", t.NotSupportedYet)           // [later]
	mux.HandleFunc("GET /v2/account", t.Account)
	mux.HandleFunc("GET /v2/positions", t.Positions)
	mux.HandleFunc("GET /v2/positions/{symbol}", t.Position)
}
