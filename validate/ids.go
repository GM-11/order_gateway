package validate

import (
	"errors"
	"fmt"
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
