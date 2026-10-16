package main

import (
	"errors"
	"fmt"
	"time"
)

type config struct {
	GatewayID      string
	HTTPAddr       string
	CashOwnerAddr  string
	EngineFeedAddr string
	Instruments    string
	APIKeys        string
	JWKSURL        string
	JWTIssuer      string
	JWTAudience    string
	OrderTimeout   time.Duration
}

func loadConfig(getenv func(string) string) (config, error) {
	get := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	c := config{
		GatewayID:      getenv("GATEWAY_ID"),
		HTTPAddr:       get("GATEWAY_HTTP_ADDR", "127.0.0.1:8080"),
		CashOwnerAddr:  get("CASH_OWNER_ADDR", "127.0.0.1:50071"),
		EngineFeedAddr: get("ENGINE_FEED_ADDR", "127.0.0.1:50061"),
		Instruments:    get("EXCHANGE_INSTRUMENTS", "../config/instruments.json"),
		APIKeys:        getenv("API_KEYS_SOURCE"),
		JWKSURL:        getenv("JWKS_URL"),
		JWTIssuer:      getenv("JWT_ISSUER"),
		JWTAudience:    getenv("JWT_AUDIENCE"),
	}

	var errs []error
	if c.GatewayID == "" {
		errs = append(errs, errors.New("GATEWAY_ID is required (unique per gateway process; the engine routes replies by it)"))
	}
	if c.APIKeys == "" {
		errs = append(errs, errors.New("API_KEYS_SOURCE is required (a file path or an http(s) URL)"))
	}
	if c.JWKSURL != "" && (c.JWTIssuer == "" || c.JWTAudience == "") {
		errs = append(errs, errors.New("JWT_ISSUER and JWT_AUDIENCE are required when JWKS_URL is set"))
	}
	raw := get("ORDER_TIMEOUT", "5s")
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		errs = append(errs, fmt.Errorf("ORDER_TIMEOUT %q must be a positive duration like 5s", raw))
	}
	c.OrderTimeout = d
	return c, errors.Join(errs...)
}
