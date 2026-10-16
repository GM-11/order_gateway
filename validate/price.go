package validate

import (
	"errors"
	"strconv"
	"strings"
)

var ErrBadNumber = errors.New("must be digits like 123 or 123.45")

func ParseMinor(s string) (int64, error) {
	if len(s) == 0 || len(s) > 15 { // 15 chars keeps us far below int64's limit
		return 0, ErrBadNumber
	}
	whole, frac, hasDot := strings.Cut(s, ".") // "100.5" → "100", "5", true
	if !allDigits(whole) {
		return 0, ErrBadNumber // catches "", "-5", "+5", " 5", "1e3", "1,000"
	}
	if hasDot && (len(frac) == 0 || len(frac) > 2 || !allDigits(frac)) {
		return 0, ErrBadNumber // catches "100.", "100.123", "100.5x"
	}
	for len(frac) < 2 {
		frac += "0" // "" → "00", "5" → "50"
	}
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, ErrBadNumber
	}
	return n, nil
}

func ParseQty(s string) (int64, error) {
	if len(s) == 0 || len(s) > 12 || !allDigits(s) {
		return 0, ErrBadNumber
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, ErrBadNumber
	}
	return n, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
