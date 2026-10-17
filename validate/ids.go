package validate

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrBadClientOrderID = errors.New("client_order_id must be 1-48 chars of A-Z a-z 0-9 _ -")

func ClientOrderID(s string) error {
	if len(s) < 1 || len(s) > 48 { // len counts bytes, not letters
		return ErrBadClientOrderID
	}
	for i := 0; i < len(s); i++ { // byte loop: any non-ASCII byte (≥ 0x80) fails, which we want
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return ErrBadClientOrderID
		}
	}
	return nil
}

func FormatMinor(n int64) string {
	sign := ""
	u := uint64(n)
	if n < 0 {
		sign = "-"
		u = uint64(-n) // for MinInt64, -n wraps back to MinInt64, and uint64 of that is exactly 2^63, which is correct
	}
	return fmt.Sprintf("%s%d.%02d", sign, u/100, u%100) // 1250 → "12.50", 5 → "0.05"
}

var ErrBadOrderID = errors.New("order_id must look like <symbol_id>-<seq>")

// FormatOrderID is the public order id: "<symbol_id>-<seq>", e.g. "1-812".
// seq is unique only per sequencer partition, so the symbol is part of the id.
// Clients treat it as opaque.
func FormatOrderID(symbol uint32, seq uint64) string {
	return strconv.FormatUint(uint64(symbol), 10) + "-" + strconv.FormatUint(seq, 10)
}

// ParseOrderID is the inverse of FormatOrderID. Only the exact form
// FormatOrderID writes is accepted (no signs, spaces or leading zeros), so one
// order has one public spelling.
func ParseOrderID(s string) (symbol uint32, seq uint64, err error) {
	left, right, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, ErrBadOrderID
	}
	sym, err1 := strconv.ParseUint(left, 10, 32)
	n, err2 := strconv.ParseUint(right, 10, 64)
	if err1 != nil || err2 != nil || sym == 0 || n == 0 {
		return 0, 0, ErrBadOrderID
	}
	if FormatOrderID(uint32(sym), n) != s {
		return 0, 0, ErrBadOrderID
	}
	return uint32(sym), n, nil
}
