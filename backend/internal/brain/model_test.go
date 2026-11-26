package brain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"ghosthand/proto"
)

func TestToolLimitAndHistoryCompaction(t *testing.T) {
	s := &Session{names: map[string]string{"device_get_status": proto.ToolStatus}}
	output := make([]json.RawMessage, 100)
	for i := range output {
		output[i] = json.RawMessage(fmt.Sprintf(`{"type":"function_call","call_id":"call_%d","name":"device_get_status","arguments":"{}"}`, i))
	}
	turn, err := s.readTurn(output[:99])
	if err != nil || len(turn.Calls) != 99 {
		t.Fatalf("99 calls: got %d, err=%v", len(turn.Calls), err)
	}
	if _, err := s.readTurn(output); err == nil {
		t.Fatal("100 parallel calls accepted")
	}
	s.input = nil
	full := "head:" + strings.Repeat("示例", 1000) + ":tail"
	for i := 0; i < 26; i++ {
		s.Return([]Call{{ID: fmt.Sprint(i)}}, []Observation{{Text: full}})
		if i == 23 && s.input[0].(map[string]any)["output"] != full {
			t.Fatal("history was compacted before the 25th round")
		}
	}
	for i, item := range s.input {
		text := item.(map[string]any)["output"].(string)
		if i >= 11 {
			if text != full {
				t.Fatalf("recent round %d was compacted", i)
			}
			continue
		}
		if !strings.HasPrefix(text, "head:") || !strings.HasSuffix(text, ":tail") ||
			!strings.Contains(text, "历史长输出已截断") || len(text) > 2100 || !utf8.ValidString(text) {
			t.Fatalf("invalid compacted output at round %d", i)
		}
	}
}

func TestOpenAIResponsesLoop(t *testing.T) {
	ctx := t.Context()
	tools := []proto.MCPTool{proto.DeviceTools[0], proto.DeviceTools[2]}
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		round++
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "completed",
				"output": []any{
					map[string]any{
						"type":      "function_call",
						"call_id":   "call_1",
						"name":      "device_get_status",
						"arguments": "{}",
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{"type": "output_text", "text": "设备在线"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	s, err := New(srv.URL+"/v1", "test-key", "gpt-5.4", "查状态", tools)
	if err != nil {
		t.Fatal(err)
	}
	turn1, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(turn1.Calls) != 1 || turn1.Calls[0].Name != proto.ToolStatus {
		t.Fatalf("unexpected turn1 calls: %+v", turn1.Calls)
	}
	s.Return(turn1.Calls, []Observation{{Text: `{"ok":true}`, JPEG: []byte{0xFF, 0xD8, 0xFF}}})
	turn2, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if turn2.Text != "设备在线" || len(turn2.Calls) != 0 {
		t.Fatalf("unexpected turn2: %+v", turn2)
	}
}
