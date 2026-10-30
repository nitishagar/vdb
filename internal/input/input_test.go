package input

import (
	"context"
	"errors"
	"testing"

	"github.com/nitishagar/vdb/internal/faketoolkit"
	"github.com/nitishagar/vdb/internal/toolkit"
)

func TestParseSequence(t *testing.T) {
	seq, err := ParseSequence("DOWN, DOWN ,ENTER")
	if err != nil {
		t.Fatalf("ParseSequence: %v", err)
	}
	if len(seq) != 3 || seq[0].Key != "DOWN" || seq[2].Key != "ENTER" {
		t.Errorf("seq = %+v", seq)
	}
}

func TestParseSequenceBoundaries(t *testing.T) {
	for _, bad := range []string{"", "   ", "DOWN,,ENTER", "DOWN,BOGUS", "up"} {
		if _, err := ParseSequence(bad); err == nil {
			t.Errorf("ParseSequence(%q) must error", bad)
		}
	}
	// Unknown keys must error before any press happens (invariant 9).
	if _, err := ParseSequence("WARP"); err == nil {
		t.Error("unknown key must error")
	}
}

func TestRepeatPress(t *testing.T) {
	seq, err := RepeatPress("RIGHT", 3, 500)
	if err != nil {
		t.Fatalf("RepeatPress: %v", err)
	}
	if len(seq) != 3 || seq[0].Key != "RIGHT" || seq[0].HoldMS != 500 {
		t.Errorf("seq = %+v", seq)
	}
	if _, err := RepeatPress("RIGHT", 0, 0); err == nil {
		t.Error("n=0 must error")
	}
	if _, err := RepeatPress("NOPE", 2, 0); err == nil {
		t.Error("unknown key must error")
	}
}

// The toolkit backend maps names to the documented codes and carries hold.
func TestToolkitBackendSendsDocumentedCodes(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("injectInputKeyEvent", faketoolkit.ObjectStep(nil))
	backend := NewToolkitBackend(toolkit.NewClient(srv.URL()))
	if err := backend.InjectKey(context.Background(), "SERIAL", "HOME", 750); err != nil {
		t.Fatalf("InjectKey: %v", err)
	}
	calls := srv.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	params := string(calls[0].Params)
	if !contains(params, `"keyCode":170`) || !contains(params, `"holdDuration":750`) {
		t.Errorf("params = %s (HOME must be 170 per the documented table)", params)
	}
}

func TestToolkitBackendUnknownKeyErrorsBeforeWire(t *testing.T) {
	srv := faketoolkit.New(t)
	backend := NewToolkitBackend(toolkit.NewClient(srv.URL()))
	if err := backend.InjectKey(context.Background(), "S", "WARP", 0); err == nil {
		t.Fatal("unknown key must error")
	}
	if n := srv.CallsFor("injectInputKeyEvent"); n != 0 {
		t.Errorf("no RPC may be sent for an invalid key (sent %d)", n)
	}
}

func TestToolkitBackendPropagatesRPCError(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("injectInputKeyEvent", faketoolkit.ErrorStep(-32000, "input rejected"))
	backend := NewToolkitBackend(toolkit.NewClient(srv.URL()))
	err := backend.InjectKey(context.Background(), "S", "ENTER", 0)
	if err == nil || !contains(err.Error(), "input rejected") {
		t.Fatalf("want propagated rpc error, got %v", err)
	}
	var rpcErr *toolkit.RPCError
	if !errors.As(err, &rpcErr) {
		t.Errorf("error should unwrap to RPCError, got %T", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
