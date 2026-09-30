//go:build !linux

package ble

import (
	"context"

	"aether/backend/internal/tuyable"
)

// NewSystemRadio is a radio that is never available: Aether Edge reaches the host's Bluetooth only through BlueZ on
// Linux. Development machines use a fake radio in tests.
func NewSystemRadio(id string) Radio { return unavailable{id: id} }

type unavailable struct{ id string }

func (u unavailable) Adapter() string { return u.id }

func (unavailable) Scan(ctx context.Context, _ func(Advertisement)) error { return ErrNoAdapter }

func (unavailable) Connect(context.Context, string, bool) (tuyable.Link, error) {
	return nil, ErrNoAdapter
}
