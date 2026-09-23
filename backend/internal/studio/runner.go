package studio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

var slots = make(chan struct{}, 2)

func Run(ctx context.Context, code, fn string, input json.RawMessage) (json.RawMessage, error) {
	if len(code) > 16000 || len(input) > 65536 || !json.Valid(input) || (fn != "decodeUplink" && fn != "render" && fn != "decodeHistory") {
		return nil, fmt.Errorf("invalid javascript request")
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	path := os.Getenv("AETHER_JS_RUNNER")
	if path == "" {
		path = "/app/sandbox/run.py"
	}
	b, _ := json.Marshal(map[string]any{"code": code, "function": fn, "input": input})
	cmd := exec.CommandContext(ctx, "python3", path)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.Stdin = bytes.NewReader(b)
	out, e := cmd.Output()
	if e != nil || len(out) > 70000 {
		return nil, fmt.Errorf("javascript failed or resource limit exceeded")
	}
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(out, &r) != nil || len(r.Result) == 0 {
		return nil, fmt.Errorf("invalid javascript output")
	}
	return r.Result, nil
}
