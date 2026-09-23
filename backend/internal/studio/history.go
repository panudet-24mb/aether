package studio

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type Observation struct {
	Raw        string    `json:"raw"`
	Source     string    `json:"source"`
	ReceivedAt time.Time `json:"received_at"`
}
type DecodedHistory struct {
	Data       map[string]any
	History    []map[string]any
	Source     string
	ReceivedAt time.Time
	Skipped    int
	Count      int
}

// Observations arrive newest-first. Bound total input, and use the same decoder for every point.
func DecodeHistory(ctx context.Context, code string, observations []Observation) (DecodedHistory, error) {
	out := DecodedHistory{History: []map[string]any{}}
	frames := []string{}
	size := 2
	for _, o := range observations {
		if len(frames) >= 200 || size+len(o.Raw)+3 > 48000 {
			break
		}
		frames = append(frames, o.Raw)
		size += len(o.Raw) + 3
	}
	if len(frames) == 0 {
		return out, fmt.Errorf("no archived frames in this range")
	}
	payload, _ := json.Marshal(frames)
	result, e := Run(ctx, code, "decodeHistory", payload)
	if e != nil {
		return out, e
	}
	var decoded struct {
		Samples []json.RawMessage `json:"samples"`
	}
	if json.Unmarshal(result, &decoded) != nil || len(decoded.Samples) != len(frames) {
		return out, fmt.Errorf("invalid history result")
	}
	out.Count = len(frames)
	for n, sample := range decoded.Samples {
		var value struct {
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(sample, &value) != nil || value.Data == nil {
			out.Skipped++
			continue
		}
		o := observations[n]
		if out.Data == nil {
			out.Data = value.Data
			out.Source = o.Source
			out.ReceivedAt = o.ReceivedAt
		}
		point := map[string]any{}
		for k, v := range value.Data {
			point[k] = v
		}
		point["received_at"] = o.ReceivedAt
		point["source"] = o.Source
		out.History = append(out.History, point)
	}
	if out.Data == nil {
		return out, fmt.Errorf("decoder did not produce data for any frame")
	}
	for i, j := 0, len(out.History)-1; i < j; i, j = i+1, j-1 {
		out.History[i], out.History[j] = out.History[j], out.History[i]
	}
	return out, nil
}
