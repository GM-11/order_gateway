package validate

import "testing"

func TestParseMinor(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "whole price", input: "100", want: 10000},
		{name: "one fractional digit", input: "100.5", want: 10050},
		{name: "two fractional digits", input: "100.50", want: 10050},
		{name: "fraction below one unit", input: "0.05", want: 5},
		{name: "leading zeroes", input: "007", want: 700},
		{name: "zero", input: "0", want: 0},
		{name: "empty", input: "", wantErr: true},
		{name: "missing whole part", input: ".5", wantErr: true},
		{name: "missing fractional part", input: "100.", wantErr: true},
		{name: "too many fractional digits", input: "100.123", wantErr: true},
		{name: "negative", input: "-5", wantErr: true},
		{name: "positive sign", input: "+5", wantErr: true},
		{name: "exponent", input: "1e3", wantErr: true},
		{name: "leading whitespace", input: " 100", wantErr: true},
		{name: "thousands separator", input: "1,000", wantErr: true},
		{name: "letters", input: "abc", wantErr: true},
		{name: "full-width digits", input: "１００", wantErr: true},
		{name: "too many characters", input: "1234567890123456", wantErr: true},
		{name: "exactly 15 chars is allowed", input: "123456789012.34", want: 12345678901234},
		{name: "two dots", input: "1.2.3", wantErr: true},
		{name: "junk after fraction", input: "100.5x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMinor(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseMinor(%q) = %d, want an error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMinor(%q) returned unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseMinor(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseQty(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{input: "10", want: 10},
		{input: "0.5", wantErr: true},
		{input: "+3", wantErr: true},
		{input: "-3", wantErr: true},
		{input: "", wantErr: true},
		{input: "999999999999", want: 999999999999}, // 12 digits: the limit
		{input: "1000000000000", wantErr: true},     // 13 digits: one over
		{input: "0", want: 0},                       // a valid number; "must be ≥ 1" is checked later
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseQty(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseQty(%q) = %d, want an error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseQty(%q) returned unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseQty(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
