package brain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestUserImagesSurviveToolRounds(t *testing.T) {
	const original = "data:image/jpeg;base64,AQID"
	const screenshot = "data:image/jpeg;base64,BAUG"
	requests := captureImageRequests(t, 0)
	s, err := NewWithImages(requests.URL, "", "vision", "查看图片", [][]byte{{1, 2, 3}}, proto.DeviceTools)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Clear()
	for i, want := range [][]string{{original}, {original, screenshot}, {original}} {
		turn, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		body := <-requests.bodies
		if got := modelImageURLs(t, body); !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d images = %v, want %v", i+1, got, want)
		}
		if i == 0 {
			var first struct {
				Role    string
				Content []map[string]string
			}
			if err := json.Unmarshal(body[0], &first); err != nil {
				t.Fatal(err)
			}
			if first.Role != "user" || len(first.Content) != 2 || first.Content[0]["type"] != "input_text" ||
				first.Content[0]["text"] != "查看图片" || first.Content[1]["detail"] != "auto" {
				t.Fatalf("initial input = %s", body[0])
			}
			s.Return(turn.Calls, []Observation{{Text: "屏幕内容", JPEG: []byte{4, 5, 6}}})
		} else {
			s.Return(turn.Calls, []Observation{{Text: "工具结果"}})
		}
	}
}

func TestUserImagesFallbackForTextOnlyModel(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests := captureImageRequests(t, status)
			s, err := NewWithImages(requests.URL, "", "text", "查看图片", [][]byte{{1, 2, 3}}, proto.DeviceTools)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Clear()
			if _, err := s.Next(t.Context()); err != nil {
				t.Fatal(err)
			}
			initial, fallback := <-requests.bodies, <-requests.bodies
			if len(modelImageURLs(t, initial)) != 1 || len(modelImageURLs(t, fallback)) != 0 ||
				!strings.Contains(string(fallback[0]), "用户上传了图片（当前模型不支持视觉输入）。") {
				t.Fatalf("fallback input = %s", fallback[0])
			}
		})
	}
}

type imageRequests struct {
	URL    string
	bodies chan []json.RawMessage
}

func captureImageRequests(t *testing.T, firstStatus int) imageRequests {
	t.Helper()
	bodies := make(chan []json.RawMessage, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Input []json.RawMessage }
		if r.URL.Path != "/v1/responses" || json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Input) == 0 {
			t.Error("invalid Responses request")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		bodies <- request.Input
		if firstStatus != 0 {
			w.WriteHeader(firstStatus)
			firstStatus = 0
			return
		}
		fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","call_id":"call_%d","name":"device_get_status","arguments":"{}"}]}`, len(request.Input))
	}))
	t.Cleanup(srv.Close)
	return imageRequests{URL: srv.URL, bodies: bodies}
}

func modelImageURLs(t *testing.T, input []json.RawMessage) []string {
	t.Helper()
	var urls []string
	for _, item := range input {
		var message struct {
			Role    string
			Content []map[string]string
		}
		if err := json.Unmarshal(item, &message); err != nil {
			t.Fatal(err)
		}
		if message.Role != "user" {
			continue
		}
		for _, block := range message.Content {
			if block["type"] == "input_image" {
				urls = append(urls, block["image_url"])
			}
		}
	}
	return urls
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
