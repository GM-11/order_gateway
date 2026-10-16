package refdata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"order_gateway/core"
	"os"
)

type Instrument struct {
	ID        core.SymbolID // the number used inside the system (MOOG = 1)
	Ticker    string        // what people type: "MOOG"
	Partition uint32        // which sequencer lane owns it (the gateway doesn't use it, but checks it)
	BandBps   int64         // circuit-breaker band, 1000 = 10%
	TickMinor int64         // minimum price increment in minor units
}

type Table struct {
	Version  uint64
	byTicker map[string]Instrument
}

func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var file struct {
		Version     uint64 `json:"version"`
		Partitions  uint32 `json:"partitions"`
		Instruments []struct {
			ID        core.SymbolID `json:"id"`
			Ticker    string        `json:"ticker"`
			Partition uint32        `json:"partition"`
			BandBps   int64         `json:"band_bps"`
			TickMinor int64         `json:"tick_minor"`
		} `json:"instruments"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected data after JSON document")
		}
		return nil, err
	}

	t := &Table{
		Version:  file.Version,
		byTicker: make(map[string]Instrument, len(file.Instruments)),
	}
	seenIDs := make(map[core.SymbolID]struct{}, len(file.Instruments))
	for _, input := range file.Instruments {
		if input.ID == 0 {
			return nil, fmt.Errorf("instrument id must not be 0")
		}
		if _, exists := seenIDs[input.ID]; exists {
			return nil, fmt.Errorf("duplicate instrument id %d", input.ID)
		}
		seenIDs[input.ID] = struct{}{}

		if len(input.Ticker) < 1 || len(input.Ticker) > 12 || input.Ticker[0] < 'A' || input.Ticker[0] > 'Z' {
			return nil, fmt.Errorf("invalid ticker %q", input.Ticker)
		}
		for i := 0; i < len(input.Ticker); i++ {
			c := input.Ticker[i]
			if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				return nil, fmt.Errorf("invalid ticker %q", input.Ticker)
			}
		}
		if _, exists := t.byTicker[input.Ticker]; exists {
			return nil, fmt.Errorf("duplicate ticker %q", input.Ticker)
		}
		if input.Partition >= file.Partitions {
			return nil, fmt.Errorf("partition %d is not less than partitions %d", input.Partition, file.Partitions)
		}
		if input.BandBps < 1 || input.BandBps > 9999 {
			return nil, fmt.Errorf("band_bps must be between 1 and 9999")
		}
		if input.TickMinor < 1 {
			return nil, fmt.Errorf("tick_minor must be at least 1")
		}
		inst := Instrument{ID: input.ID, Ticker: input.Ticker, Partition: input.Partition, BandBps: input.BandBps, TickMinor: input.TickMinor}
		t.byTicker[inst.Ticker] = inst
	}
	return t, nil
}
func (t *Table) Lookup(ticker string) (Instrument, bool) {
	inst, ok := t.byTicker[ticker]
	return inst, ok
}
