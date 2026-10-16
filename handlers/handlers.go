package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"order_gateway/auth"
	"order_gateway/core"
	"order_gateway/refdata"
	"order_gateway/validate"
	"strconv"
)

const maxBody = 4 << 10

type Placer interface {
	Place(ctx context.Context, req core.OrderRequest) (core.Result, error)
}
type Symbols interface {
	Lookup(ticker string) (refdata.Instrument, bool)
}

type Orders struct {
	Auth     auth.Verifier
	Symbols  Symbols
	Placer   Placer
	FeedLive func() bool
}

var _ http.Handler = (*Orders)(nil)

type placeOrderRequest struct {
	ClientOrderID string `json:"client_order_id"`
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Type          string `json:"type"`
	TimeInForce   string `json:"time_in_force"`
	Price         string `json:"price"` // a string so "101.50" stays exact; no floats
	Qty           string `json:"qty"`
}

type orderResponse struct {
	ClientOrderID string `json:"client_order_id"`
	OrderID       string `json:"order_id"` // a string because JavaScript loses precision on 64-bit numbers
	Status        string `json:"status"`   // "new" or "rejected"
}

type errorResponse struct {
	Error     string `json:"error"`               // short code the client can switch on
	Field     string `json:"field,omitempty"`     // which field was wrong (422 only)
	Message   string `json:"message,omitempty"`   // text for humans
	Shortfall string `json:"shortfall,omitempty"` // "12.50", insufficient_cash only
}

type apiError struct {
	status int
	body   errorResponse
}

func invalid(field, msg string) *apiError {
	return &apiError{http.StatusUnprocessableEntity, errorResponse{Error: "invalid_field", Field: field, Message: msg}}
}

func (h *Orders) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, err := h.Auth.Verify(auth.FromRequest(r))
	switch {
	case errors.Is(err, auth.ErrNotReady): // keys not loaded yet: our fault, so they can retry
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "auth_not_ready"})
		return
	case err != nil: // no credentials or bad ones: same answer, so we don't reveal which
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}

	if h.FeedLive != nil && !h.FeedLive() {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "engine_unavailable",
			Message: "order was NOT placed; safe to retry"})
		return
	}

	in, e := decode(w, r) // ②
	if e != nil {
		writeJSON(w, e.status, e.body)
		return
	}
	req, e := h.build(in, p.Account) // ③ + ④
	if e != nil {
		writeJSON(w, e.status, e.body)
		return
	}

	res, err := h.Placer.Place(r.Context(), req)

	switch {
	case errors.Is(err, core.ErrDuplicateInFlight):
		writeJSON(w, http.StatusConflict, errorResponse{Error: "duplicate_in_flight",
			Message: "an order with this client_order_id is still being processed"})
		return
	case errors.Is(err, core.ErrNoAnswer):
		writeJSON(w, http.StatusGatewayTimeout, errorResponse{Error: "outcome_unknown",
			Message: "retry with the SAME client_order_id; never a new one"})
		return
	case err != nil:
		log.Printf("orders: unexpected Place error: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
		return
	}

	switch res.Outcome {
	case core.OutcomePlaced, core.OutcomeEngineRejected:
		status := "new"
		if res.Outcome == core.OutcomeEngineRejected {
			status = "rejected"
		}
		writeJSON(w, http.StatusOK, orderResponse{
			ClientOrderID: string(req.ClientOrderID),
			OrderID:       strconv.FormatUint(uint64(res.OrderID), 10),
			Status:        status,
		})
	case core.OutcomeCashRejected:
		body := errorResponse{Error: reasonCode(res.Reason)}
		if res.Reason == core.ReasonInsufficientCash {
			body.Shortfall = validate.FormatMinor(res.ShortfallMinor)
		}
		writeJSON(w, http.StatusForbidden, body)
	default: // a new Outcome was added to core but not mapped here
		log.Printf("orders: unmapped outcome %d", res.Outcome)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
	}
}

func reasonCode(r core.RejectReason) string {
	switch r {
	case core.ReasonInsufficientCash:
		return "insufficient_cash"
	case core.ReasonInsufficientHoldings:
		return "insufficient_holdings"
	default:
		return "rejected"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status) // headers must be set BEFORE this line; after it, they're already sent
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request) (placeOrderRequest, *apiError) {
	var in placeOrderRequest

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return in, &apiError{http.StatusUnsupportedMediaType, errorResponse{Error: "content_type_must_be_json"}}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&in); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return in, &apiError{http.StatusRequestEntityTooLarge, errorResponse{Error: "request_too_large"}}
		}
		return in, &apiError{http.StatusBadRequest, errorResponse{Error: "invalid_json", Message: err.Error()}}

	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return in, &apiError{http.StatusBadRequest, errorResponse{Error: "malformed_json", Message: "extra data after the JSON object"}}
	}
	return in, nil

}

func (h *Orders) build(in placeOrderRequest, account core.AccountID) (core.OrderRequest, *apiError) {
	var none core.OrderRequest

	if err := validate.ClientOrderID(in.ClientOrderID); err != nil {
		return none, invalid("client_order_id", err.Error())
	}
	inst, ok := h.Symbols.Lookup(in.Symbol)
	if !ok {
		return none, invalid("symbol", "unknown symbol")
	}
	var side core.Side
	switch in.Side {
	case "buy":
		side = core.Buy
	case "sell":
		side = core.Sell
	default:
		return none, invalid("side", `must be "buy" or "sell"`)
	}
	if in.Type != "limit" {
		return none, invalid("type", `only "limit" is supported`)
	}
	if in.TimeInForce != "gtc" {
		return none, invalid("time_in_force", `only "gtc" is supported`)
	}
	price, err := validate.ParseMinor(in.Price)
	if err != nil || price <= 0 { // ParseMinor happily returns 0 for "0.00"
		return none, invalid("price", "must be a positive amount like 101.50")
	}
	qty, err := validate.ParseQty(in.Qty)
	if err != nil || qty <= 0 {
		return none, invalid("qty", "must be a whole number above 0")
	}

	return core.OrderRequest{
		AccountID:     account,
		ClientOrderID: core.ClientOrderID(in.ClientOrderID),
		Symbol:        inst.ID,
		Side:          side,
		PriceMinor:    price,
		Qty:           qty,
	}, nil
}
