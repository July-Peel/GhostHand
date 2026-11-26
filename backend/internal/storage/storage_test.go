package storage

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWebDAVMonthlyFolderAndContentTypes(t *testing.T) {
	var mu sync.Mutex
	collections := map[string]bool{"/dav/": true}
	files := map[string]struct {
		contentType string
		body        string
	}{}
	var mkcols []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PROPFIND":
			p := r.URL.Path
			if !strings.HasSuffix(p, "/") {
				p += "/"
			}
			if !collections[p] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>
				<multistatus xmlns="DAV:"><response><propstat><prop><resourcetype><collection/></resourcetype></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
		case "MKCOL":
			p := strings.TrimRight(r.URL.Path, "/")
			parent := p[:strings.LastIndex(p, "/")+1]
			if !collections[parent] {
				w.WriteHeader(http.StatusConflict)
				return
			}
			collections[p+"/"] = true
			mkcols = append(mkcols, p)
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			p := r.URL.Path
			parent := p[:strings.LastIndex(p, "/")+1]
			if !collections[parent] {
				w.WriteHeader(http.StatusConflict)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			files[p] = struct {
				contentType string
				body        string
			}{
				contentType: r.Header.Get("Content-Type"),
				body:        string(raw),
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			item, ok := files[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, item.body)
		case http.MethodDelete:
			delete(files, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	dav := WebDAV{Endpoint: srv.URL + "/dav"}
	ctx := t.Context()

	month := time.Now().Format("200601")
	imgKey, err := dav.PutMonthly(ctx, "ghosthand-shot.jpg", strings.NewReader("jpeg-bytes"))
	if err != nil {
		t.Fatalf("PutMonthly image: %v", err)
	}
	if imgKey != month+"/ghosthand-shot.jpg" {
		t.Fatalf("imgKey=%q, want %q", imgKey, month+"/ghosthand-shot.jpg")
	}

	txtKey, err := dav.PutMonthly(ctx, "note.txt", strings.NewReader("hello text"))
	if err != nil {
		t.Fatalf("PutMonthly text: %v", err)
	}
	if txtKey != month+"/note.txt" {
		t.Fatalf("txtKey=%q, want %q", txtKey, month+"/note.txt")
	}

	mu.Lock()
	if len(mkcols) != 1 || mkcols[0] != "/dav/"+month {
		t.Fatalf("mkcols=%v, want [/dav/%s]", mkcols, month)
	}
	if got := files["/dav/"+imgKey].contentType; got != "image/jpeg" {
		t.Fatalf("image contentType=%q, want image/jpeg", got)
	}
	if got := files["/dav/"+txtKey].contentType; got != "text/plain; charset=utf-8" {
		t.Fatalf("text contentType=%q, want text/plain; charset=utf-8", got)
	}
	mu.Unlock()

	rc, err := dav.Get(ctx, imgKey)
	if err != nil {
		t.Fatalf("Get image: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "jpeg-bytes" {
		t.Fatalf("Get body=%q, want jpeg-bytes", string(got))
	}

	if err := dav.Delete(ctx, "../escape"); err == nil {
		t.Fatal("expected invalid key error for traversal")
	}
}
