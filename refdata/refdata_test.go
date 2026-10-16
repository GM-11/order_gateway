package refdata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	const valid = `{"version":1,"partitions":2,"instruments":[{"id":1,"ticker":"MOOG","partition":0,"band_bps":1000}]}`

	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "valid file", content: valid},
		{name: "unknown field", content: `{"version":1,"partitions":2,"unknown":true,"instruments":[]}`, wantErr: true},
		{name: "version zero", content: `{"version":0,"partitions":2,"instruments":[]}`, wantErr: true},
		{name: "partitions zero", content: `{"version":1,"partitions":0,"instruments":[]}`, wantErr: true},
		{name: "duplicate id", content: `{"version":1,"partitions":2,"instruments":[{"id":1,"ticker":"MOOG","partition":0,"band_bps":1000},{"id":1,"ticker":"GOOG","partition":1,"band_bps":1000}]}`, wantErr: true},
		{name: "duplicate ticker", content: `{"version":1,"partitions":2,"instruments":[{"id":1,"ticker":"MOOG","partition":0,"band_bps":1000},{"id":2,"ticker":"MOOG","partition":1,"band_bps":1000}]}`, wantErr: true},
		{name: "lowercase ticker", content: `{"version":1,"partitions":2,"instruments":[{"id":1,"ticker":"moog","partition":0,"band_bps":1000}]}`, wantErr: true},
		{name: "partition out of range", content: `{"version":1,"partitions":2,"instruments":[{"id":1,"ticker":"MOOG","partition":2,"band_bps":1000}]}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "refdata.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write test data: %v", err)
			}

			table, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Load() succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() returned unexpected error: %v", err)
			}

			inst, ok := table.Lookup("MOOG")
			if !ok {
				t.Fatal(`Lookup("MOOG") returned false, want true`)
			}
			if inst.ID != 1 {
				t.Errorf(`Lookup("MOOG").ID = %d, want 1`, inst.ID)
			}
			if _, ok := table.Lookup("GOOGL"); ok {
				t.Error(`Lookup("GOOGL") returned true, want false`)
			}
		})
	}
}
