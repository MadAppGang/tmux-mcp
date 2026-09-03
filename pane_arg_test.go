package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// slotRequest builds a tool call carrying the given slot argument. A nil value
// means the argument was sent as JSON null; use omitSlot for "absent".
func slotRequest(value any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "test",
			Arguments: map[string]any{"slot": value},
		},
	}
}

// TestParseSlotArg walks the whole accepted grammar, and the rejections matter
// more than the acceptances.
//
// The dangerous rows are the non-numeric strings and the out-of-range numbers.
// mcp-go's GetInt would coerce any of them to 0 via strconv.Atoi's failure path,
// and 0 is indistinguishable from "no slot given", so a caller's typo would land
// silently in the shared default pane instead of being refused. Everything here
// exists to make that impossible to reintroduce.
func TestParseSlotArg(t *testing.T) {
	tests := []struct {
		name        string
		req         mcp.CallToolRequest
		wantSlot    int
		wantPresent bool
		wantErr     bool
	}{
		{
			name: "absent",
			req: mcp.CallToolRequest{Params: mcp.CallToolParams{
				Name: "test", Arguments: map[string]any{},
			}},
		},
		{name: "json null", req: slotRequest(nil)},
		{name: "number", req: slotRequest(float64(2)), wantSlot: 2, wantPresent: true},
		{name: "number 1", req: slotRequest(float64(1)), wantSlot: 1, wantPresent: true},
		{name: "max slot", req: slotRequest(float64(maxSlot)), wantSlot: maxSlot, wantPresent: true},
		{name: "stringified number", req: slotRequest("2"), wantSlot: 2, wantPresent: true},
		{name: "padded stringified number", req: slotRequest(" 3 "), wantSlot: 3, wantPresent: true},
		{name: "json.Number", req: slotRequest(json.Number("4")), wantSlot: 4, wantPresent: true},
		{name: "go int", req: slotRequest(5), wantSlot: 5, wantPresent: true},

		{name: "zero", req: slotRequest(float64(0)), wantErr: true},
		{name: "negative", req: slotRequest(float64(-1)), wantErr: true},
		// "new" used to be an accepted value. It is gone, and a caller still
		// sending it must be told so rather than quietly landing in slot 1.
		{name: "new is no longer a slot", req: slotRequest("new"), wantErr: true},
		{name: "NEW uppercased", req: slotRequest("NEW"), wantErr: true},
		{name: "above max", req: slotRequest(float64(maxSlot + 1)), wantErr: true},
		{name: "wildly above max", req: slotRequest(float64(99999)), wantErr: true},
		{name: "non-integral", req: slotRequest(1.5), wantErr: true},
		{name: "bool", req: slotRequest(true), wantErr: true},
		{name: "word", req: slotRequest("banana"), wantErr: true},
		{name: "empty string", req: slotRequest(""), wantErr: true},
		{name: "array", req: slotRequest([]any{1}), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			slot, present, err := parseSlotArg(tc.req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got slot=%d present=%v", slot, present)
				}
				// The message has to name what IS accepted. An error that only
				// says "invalid" is what sends an agent hunting for another
				// route into the terminal.
				if !strings.Contains(err.Error(), fmt.Sprintf("1 to %d", maxSlot)) {
					t.Errorf("error message does not name the accepted range: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if slot != tc.wantSlot || present != tc.wantPresent {
				t.Errorf("got slot=%d present=%v, want slot=%d present=%v",
					slot, present, tc.wantSlot, tc.wantPresent)
			}
		})
	}
}

// TestSlotEmbeddingFlattens pins a Go rule the responses depend on: the
// exported fields of an embedded UNEXPORTED struct type are promoted into the
// enclosing JSON object rather than nested under a sub-object.
//
// That promotion is what lets pane-state answer {"panePid":…,"slot":1} and
// run-in-repl answer {"slot":1,"created":false,"output":…} without either
// response growing a nested object no client expects. It is a rule about the
// library rather than about this code, so it is checked rather than assumed.
//
// It also pins the two always-present fields. created is a POINTER so that
// "absent" and "false" are different answers — a reading tool omits the key
// entirely — while exited and timedOut have no omitempty at all, because an
// absent key makes `result.exited === false` unsatisfiable for the caller and
// gives a model reading the response no way to learn the field exists.
func TestSlotEmbeddingFlattens(t *testing.T) {
	t.Run("pane-state", func(t *testing.T) {
		out := marshalMap(t, paneStateResult{
			PaneState: &PaneState{PanePID: 42, IsAlive: true},
			slotRef:   slotRef{Slot: 2},
		})
		assertFlat(t, out, map[string]any{
			"slot": float64(2), "panePid": float64(42), "isAlive": true,
		})
		if _, ok := out["created"]; ok {
			t.Error("pane-state reads; created must be absent, not false")
		}
	})

	t.Run("run-in-repl", func(t *testing.T) {
		out := marshalMap(t, replResult{
			slotResolution: slotResolution{Slot: 2, Created: creating(true)},
			Output:         "hello",
		})
		assertFlat(t, out, map[string]any{
			"slot": float64(2), "created": true, "output": "hello", "exited": false,
		})
	})

	t.Run("execute-command", func(t *testing.T) {
		out := marshalMap(t, execResult{
			slotResolution: slotResolution{Slot: 1, Created: creating(false)},
			Output:         "hi",
		})
		assertFlat(t, out, map[string]any{
			"slot": float64(1), "created": false, "output": "hi",
			"exitCode": float64(0), "timedOut": false,
		})
	})

	t.Run("a reading tool omits created entirely", func(t *testing.T) {
		out := marshalMap(t, WatchResult{Slot: 3, Event: "timeout"})
		if _, ok := out["created"]; ok {
			t.Errorf("created present on a reading WatchResult: %v", out)
		}
		if out["slot"] != float64(3) {
			t.Errorf("slot is %v, want 3", out["slot"])
		}
	})
}

func marshalMap(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Read the raw bytes as well as the decoded map: json.Unmarshal into a map
	// silently keeps only the last of two identical keys, so a duplicated key —
	// the exact bug an embedded struct can cause — would be invisible in the map.
	// And no response type may carry a pane id at all.
	if strings.Contains(string(data), `"paneId"`) {
		t.Fatalf("a response type carries a paneId: %s", data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func assertFlat(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	for key, wantVal := range want {
		gotVal, ok := got[key]
		if !ok {
			t.Errorf("key %q missing — embedded fields must be promoted to the top level, got %v", key, got)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("key %q = %v, want %v", key, gotVal, wantVal)
		}
	}
}
