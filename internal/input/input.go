// Package input defines the input-injection seam: one interface, several
// backends. The toolkit backend (injectInputKeyEvent) is implemented here;
// inputd-cli and VVD emulator-gRPC are future adapters behind the same
// interface (plan: interface day one, one implemented backend).
package input

import (
	"context"
	"fmt"
	"strings"

	"github.com/nitishagar/vdb/internal/toolkit"
)

// Backend injects one key press on a target. Implementations must be safe
// for concurrent use; serialization per target is the caller's duty
// (companion per-target mutex, invariant 13).
type Backend interface {
	// InjectKey presses the named key (UP/DOWN/LEFT/RIGHT/ENTER/BACK/HOME)
	// with an optional hold duration in milliseconds.
	InjectKey(ctx context.Context, serial, key string, holdMS int) error
}

// Sequence is an ordered series of key presses with per-key hold.
type Sequence []Press

// Press is one key event.
type Press struct {
	Key    string
	HoldMS int
}

// ParseSequence converts "DOWN,DOWN,ENTER" into a Sequence. Unknown key
// names error (invariant 9: never code 0).
func ParseSequence(spec string) (Sequence, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("input: empty key sequence")
	}
	var seq Sequence
	for _, part := range strings.Split(spec, ",") {
		name := strings.TrimSpace(part)
		if _, err := toolkit.KeyCode(name); err != nil {
			return nil, err
		}
		seq = append(seq, Press{Key: name})
	}
	return seq, nil
}

// RepeatPress builds a Sequence of one key pressed n times.
func RepeatPress(key string, n, holdMS int) (Sequence, error) {
	if n <= 0 {
		return nil, fmt.Errorf("input: repeat count must be > 0")
	}
	if _, err := toolkit.KeyCode(key); err != nil {
		return nil, err
	}
	seq := make(Sequence, n)
	for i := range seq {
		seq[i] = Press{Key: key, HoldMS: holdMS}
	}
	return seq, nil
}

// ToolkitBackend injects keys via the automation toolkit.
type ToolkitBackend struct {
	Client *toolkit.Client
}

// NewToolkitBackend builds a backend bound to a toolkit client.
func NewToolkitBackend(client *toolkit.Client) *ToolkitBackend {
	return &ToolkitBackend{Client: client}
}

// InjectKey implements Backend.
func (b *ToolkitBackend) InjectKey(ctx context.Context, serial, key string, holdMS int) error {
	code, err := toolkit.KeyCode(key)
	if err != nil {
		return err
	}
	return b.Client.InjectKeyEvent(ctx, code, holdMS)
}
