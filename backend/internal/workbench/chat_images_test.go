package workbench

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

func TestChatImageTaskAndRetention(t *testing.T) {
	s := newImageTestWorkbench(t)
	ctx := t.Context()
	var wantJPEG []byte
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Content []struct {
					Type, Text string
					URL        string `json:"image_url"`
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
			len(request.Input) != 1 || len(request.Input[0].Content) != 2 {
			t.Error("expected text and image in first model request", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		content := request.Input[0].Content
		if content[0].Text != imageAnalysisPrompt ||
			content[1].URL != "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(wantJPEG) {
			t.Error("unexpected initial prompt or image")
		}
		_, _ = io.WriteString(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"已读取图片"}]}]}`)
	}))
	defer model.Close()
	if err := s.config.Save(ctx, 1, settings.LLM, settings.Update{Endpoint: model.URL, Label: "vision"}); err != nil {
		t.Fatal(err)
	}
	image := testPNG(t)
	if err := s.Send(ctx, 1, "win1", "", []UploadImage{image}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Messages(ctx, "win1", 0)
	if err != nil || len(page.Messages) != 2 || page.Messages[0].Text != "" || page.Messages[0].Sender != "user" {
		t.Fatalf("chat messages: %+v, %v", page, err)
	}
	var media userMediaPayload
	if err := json.Unmarshal(page.Messages[0].Payload, &media); err != nil || len(media.Attachments) != 1 {
		t.Fatalf("media payload: %+v, %v", media, err)
	}
	var task TaskCard
	if err := json.Unmarshal(page.Messages[1].Payload, &task); err != nil || task.Title != "图片分析任务" || task.State != proto.Queued {
		t.Fatalf("task: %+v, %v", task, err)
	}
	card := media.Attachments[0]
	if card.Name != "粘贴图片.jpg" {
		t.Fatalf("attachment name = %q", card.Name)
	}
	wantJPEG, err = s.loadModelImage(ctx, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := jpeg.Decode(bytes.NewReader(wantJPEG))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := original.At(0, 0).RGBA()
	if r < 65000 || g < 65000 || b < 65000 {
		t.Fatal("transparent pixels should be white in stored JPEG")
	}
	body, _, err := s.Attachment(ctx, card.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	thumb, decodeErr := jpeg.DecodeConfig(body)
	body.Close()
	if decodeErr != nil || thumb.Width != 480 {
		t.Fatalf("thumbnail: %+v, %v", thumb, decodeErr)
	}
	if err := s.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='running' WHERE id=?`, task.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO device_sessions(device_id,conn_gen,connected_at,remote_ip,agent_ver)
			VALUES ('win1',0,1,'127.0.0.1','test')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	modelCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if result, err := s.runLoop(modelCtx, brainTask{id: task.ID, device: "win1", title: task.Title}, nil); err != nil || result != "已读取图片" {
		t.Fatalf("runLoop = %q, %v", result, err)
	}
	if err := s.store.Write(ctx, func(tx *sql.Tx) error {
		if err := finishTask(ctx, tx, task.ID, proto.Failed, "", 1); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE attachments SET created_at=1 WHERE id=?", card.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Control(ctx, 1, "win1", task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	if err := s.prune(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadModelImage(ctx, card.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("reading pruned image: %v", err)
	}
	if err := s.Control(ctx, 1, "win1", task.ID, "retry"); !errors.Is(err, ErrChatImagesExpired) {
		t.Fatalf("retrying after retention: %v", err)
	}
	if err := s.ClearMessages(ctx, 1, "win1"); err != nil {
		t.Fatal(err)
	}
	if files, err := os.ReadDir(s.fileDir); err != nil || len(files) != 0 {
		t.Fatalf("files after clear: %v, %v", files, err)
	}
}

func TestChatImageWebDAVAndRollback(t *testing.T) {
	s := newImageTestWorkbench(t)
	ctx := t.Context()
	var mu sync.Mutex
	month := time.Now().Format("200601")
	created := false
	rejectThumb := true
	files := map[string][]byte{}
	dav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.URL.Path
		switch r.Method {
		case "PROPFIND":
			if key != "/dav/" && !created {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, `<multistatus xmlns="DAV:"><response><propstat><prop><resourcetype><collection/></resourcetype></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
		case "MKCOL":
			created = strings.TrimSuffix(key, "/") == "/dav/"+month
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			if !created || !strings.HasPrefix(key, "/dav/"+month+"/ghosthand-") {
				t.Errorf("unexpected WebDAV key %q", key)
			}
			if rejectThumb && strings.HasSuffix(key, ".thumb.jpg") {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			files[key], _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			delete(files, key)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			_, _ = w.Write(files[key])
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer dav.Close()
	if err := s.config.Save(ctx, 1, settings.WebDAV, settings.Update{Endpoint: dav.URL + "/dav"}); err != nil {
		t.Fatal(err)
	}
	uploads := []UploadImage{testPNG(t)}
	checkFiles := func(want int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(files) != want {
			t.Fatalf("remote file count = %d, want %d", len(files), want)
		}
	}
	if err := s.Send(ctx, 1, "win1", "", uploads); err == nil {
		t.Fatal("expected WebDAV upload failure")
	}
	checkFiles(0)
	mu.Lock()
	rejectThumb = false
	mu.Unlock()
	if err := s.Send(ctx, 99, "win1", "", uploads); err == nil {
		t.Fatal("expected database write failure")
	}
	checkFiles(0)
	if err := s.Send(ctx, 1, "win1", "分析", uploads); err != nil {
		t.Fatal(err)
	}
	checkFiles(2)
	if files, err := os.ReadDir(s.fileDir); err != nil || len(files) != 0 {
		t.Fatalf("unexpected local files: %v, %v", files, err)
	}
	var id, kind string
	if err := s.store.QueryRowContext(ctx, "SELECT id,storage_backend FROM attachments").Scan(&id, &kind); err != nil || kind != "webdav" {
		t.Fatalf("stored attachment: %s, %v", kind, err)
	}
	if err := s.config.Save(ctx, 1, settings.WebDAV, settings.Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadModelImage(ctx, id); err != nil {
		t.Fatal(err)
	}
	page, err := s.Messages(ctx, "win1", 0)
	if err != nil || len(page.Messages) != 2 || !strings.Contains(page.Messages[1].Text, "尚未配置 AI 模型接口") {
		t.Fatalf("missing-model messages: %+v, %v", page, err)
	}
	if err := s.ClearMessages(ctx, 1, "win1"); err != nil {
		t.Fatal(err)
	}
	checkFiles(0)
}

func newImageTestWorkbench(t *testing.T) *Service {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	store, err := db.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg, err := settings.New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(store, cfg, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,created_at) VALUES (1,'admin','hash',1)"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO devices
			(id,name,platform,arch,os_version,group_name,conn_state,helper_state,desktop_state,last_seen_at,task_protocol)
			VALUES ('win1','WinNode','windows','amd64','Windows 11','default','online','ready','unlocked',1,2)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func testPNG(t *testing.T) UploadImage {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 600, 8))); err != nil {
		t.Fatal(err)
	}
	return UploadImage{Name: "粘贴图片.png", Data: data.Bytes()}
}
