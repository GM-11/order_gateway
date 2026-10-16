package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"order_gateway/auth"
	"order_gateway/core"
	pb "order_gateway/gen/exchange/v1"
	"order_gateway/handlers"
	"order_gateway/refdata"
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

	mux := http.NewServeMux()
	mux.Handle("POST /v2/orders", &handlers.Orders{
		Auth:     chain,
		Symbols:  symbols,
		Placer:   gw,
		FeedLive: reader.Live,
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /readyz", readyHandler(map[string]func() bool{
		"auth":        chain.Ready,
		"engine_feed": reader.Live,
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
	readerExited := false
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveDone:
		runErr = fmt.Errorf("http server: %w", err)
	case err := <-readerDone:
		readerExited = true
		runErr = fmt.Errorf("engine feed reader: %w", err)
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
