package brain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ghosthand/proto"
)

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
