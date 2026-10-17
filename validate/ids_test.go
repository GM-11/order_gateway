package validate

import "testing"

func TestOrderIDRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		symbol uint32
		seq    uint64
	}{{1, 812}, {2, 1}, {4294967295, 18446744073709551615}} {
		s := FormatOrderID(tc.symbol, tc.seq)
		gotSym, gotSeq, err := ParseOrderID(s)
		if err != nil || gotSym != tc.symbol || gotSeq != tc.seq {
			t.Errorf("%q: got (%d, %d, %v)", s, gotSym, gotSeq, err)
		}
	}
	if got := FormatOrderID(1, 812); got != "1-812" {
		t.Errorf("FormatOrderID = %q, want 1-812", got)
	}
}

func TestParseOrderIDRejectsEverythingElse(t *testing.T) {
	for _, s := range []string{
		"", "812", "1-", "-812", "1-812-3", "a-1", "1-a", " 1-812", "1-812 ", "+1-812", "1-+812",
		"01-812", "1-0812", "0-812", "1-0", "4294967296-1", "1-18446744073709551616", "1_812",
	} {
		if _, _, err := ParseOrderID(s); err == nil {
			t.Errorf("ParseOrderID(%q) accepted", s)
		}
	}
}
