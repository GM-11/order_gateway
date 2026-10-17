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
	"order_gateway/wire"
	"time"
)

const maxBody = 4 << 10

type Placer interface {
	Place(ctx context.Context, req core.OrderRequest) (core.Result, error)
}
type Symbols interface {
	Lookup(ticker string) (refdata.Instrument, bool)
	ByID(id core.SymbolID) (refdata.Instrument, bool)
}

type Orders struct {
	Auth     auth.Verifier
	Symbols  Symbols
	Placer   Placer
	FeedLive func() bool
	Now      func() time.Time // stamps the order we acknowledge; nil = time.Now
}

var _ http.Handler = (*Orders)(nil)

type placeOrderRequest struct {
	ClientOrderID string  `json:"client_order_id"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Type          string  `json:"type"`
	TimeInForce   string  `json:"time_in_force"`
	Price         string  `json:"price"` // a string so "101.50" stays exact; no floats
	Qty           string  `json:"qty"`
	StopPrice     *string `json:"stop_price,omitempty"` // stop / stop_limit only [later]
}

// orderResponse is the contract's Order object (section 3.2).
type orderResponse = wire.Order

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

func authenticate(v auth.Verifier, w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, err := v.Verify(auth.FromRequest(r))
	switch {
	case errors.Is(err, auth.ErrNotReady): // keys not loaded yet: our fault, so they can retry
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "auth_not_ready"})
		return p, false
	case err != nil: // no credentials or bad ones: same answer, so we don't reveal which
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return p, false
	}
	return p, true
}

func (h *Orders) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Orders) ticker(id core.SymbolID) (string, bool) {
	inst, ok := h.Symbols.ByID(id)
	return inst.Ticker, ok
}

func (h *Orders) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := authenticate(h.Auth, w, r)
	if !ok {
		return
	}

	if h.FeedLive != nil && !h.FeedLive() {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "engine_unavailable", Message: notPlaced})
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
			Message: "an order with this client_order_id is still being processed; retry shortly"})
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
		// The order "as acknowledged": no fills yet. Fills, even immediate
		// ones, arrive on the trading stream.
		ms := h.now().UnixMilli()
		state := core.OrderState{
			Account:       req.AccountID,
			ClientOrderID: req.ClientOrderID,
			Symbol:        req.Symbol,
			OrderID:       res.OrderID,
			Side:          req.Side,
			Type:          core.TypeLimit,
			TimeInForce:   core.TIFGoodTilCancelled,
			PriceMinor:    req.PriceMinor,
			Qty:           req.Qty,
			Status:        core.StatusNew,
			CreatedTs:     ms,
			UpdatedTs:     ms,
		}
		if res.Outcome == core.OutcomeEngineRejected {
			state.Status = core.StatusRejected
			state.EngineReject = res.EngineReject
			if res.EngineReject == core.EngineRejectNone {
				state.EngineReject = core.EngineRejectOther
			}
		}
		order := wire.OrderFromState(state, h.ticker)
		order.Symbol = in.Symbol // exactly what the client sent
		writeJSON(w, http.StatusOK, order)
	case core.OutcomeDuplicate:
		// Answered from the order as it stands now (may include fills).
		writeJSON(w, http.StatusOK, wire.OrderFromState(*res.Order, h.ticker))
	case core.OutcomeCashRejected:
		e := cashRejection(res)
		writeJSON(w, e.status, e.body)
	default: // a new Outcome was added to core but not mapped here
		log.Printf("orders: unmapped outcome %d", res.Outcome)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
	}
}

const notPlaced = "order was NOT placed; safe to retry"

func notSupported(field, msg string) *apiError {
	return &apiError{http.StatusUnprocessableEntity, errorResponse{Error: "not_supported_yet", Field: field, Message: msg}}
}

// cashRejection maps the cash owner's "no" onto the contract's HTTP answers.
func cashRejection(res core.Result) *apiError {
	switch res.Reason {
	case core.ReasonInsufficientCash:
		return &apiError{http.StatusForbidden, errorResponse{Error: "insufficient_cash",
			Shortfall: validate.FormatMinor(res.ShortfallMinor)}}
	case core.ReasonInsufficientHoldings:
		return &apiError{http.StatusForbidden, errorResponse{Error: "insufficient_holdings"}}
	case core.ReasonUnknownAccount:
		return &apiError{http.StatusForbidden, errorResponse{Error: "account_not_found"}}
	case core.ReasonExchangeBusy:
		return &apiError{http.StatusServiceUnavailable, errorResponse{Error: "exchange_busy", Message: notPlaced}}
	case core.ReasonExchangeUnavailable:
		return &apiError{http.StatusServiceUnavailable, errorResponse{Error: "engine_unavailable", Message: notPlaced}}
	case core.ReasonUnknownSymbol:
		return invalid("symbol", "unknown symbol")
	case core.ReasonNotSupported:
		return notSupported("type", "this order type or time_in_force is not built yet")
	default: // includes ReasonUnknown: never silently call it a rejection
		log.Printf("orders: unmapped cash owner reject reason %d", res.Reason)
		return &apiError{http.StatusInternalServerError, errorResponse{Error: "internal"}}
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
	switch in.Type {
	case "limit":
	case "market", "stop", "stop_limit": // known, not built yet
		return none, notSupported("type", `only "limit" orders are supported yet`)
	default:
		return none, invalid("type", `must be "limit", "market", "stop" or "stop_limit"`)
	}
	switch in.TimeInForce {
	case "gtc":
	case "ioc", "day": // known, not built yet
		return none, notSupported("time_in_force", `only "gtc" is supported yet`)
	default:
		return none, invalid("time_in_force", `must be "gtc", "ioc" or "day"`)
	}
	if in.StopPrice != nil {
		return none, invalid("stop_price", `only for "stop" and "stop_limit" orders`)
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
