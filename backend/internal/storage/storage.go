package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Backend interface {
	Put(context.Context, string, io.Reader) error
	Get(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

var objectKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

type Local struct{ Directory string }

func (l Local) Put(_ context.Context, key string, body io.Reader) error {
	if !objectKey.MatchString(key) {
		return errors.New("invalid local storage key")
	}
	file, err := os.CreateTemp(l.Directory, ".screenshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := io.Copy(file, body)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(l.Directory, key))
}

func (l Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if !objectKey.MatchString(key) {
		return nil, errors.New("invalid local storage key")
	}
	return os.Open(filepath.Join(l.Directory, key))
}

func (l Local) Delete(_ context.Context, key string) error {
	if !objectKey.MatchString(key) {
		return errors.New("invalid local storage key")
	}
	err := os.Remove(filepath.Join(l.Directory, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type WebDAV struct {
	Endpoint string
	Username string
	Password string
}

var webDAVClient = &http.Client{
	Timeout:       12 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (w WebDAV) endpoint() (*url.URL, error) {
	base, err := url.Parse(w.Endpoint)
	if err != nil || base.Hostname() == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, errors.New("invalid WebDAV endpoint")
	}
	return base, nil
}

func validWebDAVKey(key string) bool {
	if key == "" || len(key) > 256 || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return false
	}
	parts := strings.Split(key, "/")
	if len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		if !objectKey.MatchString(part) {
			return false
		}
	}
	return true
}

// MonthlyKey prefixes a filename with a YYYYMM directory for monthly WebDAV organization.
func MonthlyKey(name string, now time.Time) string {
	return now.Format("200601") + "/" + name
}

func contentTypeForKey(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	default:
		if ct := mime.TypeByExtension(path.Ext(key)); ct != "" {
			return ct
		}
		return "application/octet-stream"
	}
}

func (w WebDAV) send(ctx context.Context, method, address string, data io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, data)
	if err != nil {
		return nil, err
	}
	if w.Username != "" {
		req.SetBasicAuth(w.Username, w.Password)
	}
	switch method {
	case http.MethodPut:
		req.Header.Set("Content-Type", contentTypeForKey(address))
	case "PROPFIND":
		req.Header.Set("Depth", "0")
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	}
	res, err := webDAVClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Keep the configured address and credentials out of operation logs.
		return nil, errors.New("无法连接 WebDAV 服务。")
	}
	return res, nil
}

func (w WebDAV) request(ctx context.Context, method, key string, data io.Reader) (*http.Response, error) {
	if !validWebDAVKey(key) {
		return nil, errors.New("invalid WebDAV storage key")
	}
	base, err := w.endpoint()
	if err != nil {
		return nil, err
	}
	address := strings.TrimRight(base.String(), "/") + "/" + key
	res, err := w.send(ctx, method, address, data)
	if err != nil {
		return nil, err
	}
	if method == http.MethodDelete && res.StatusCode == http.StatusNotFound {
		return res, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		if res.StatusCode == http.StatusNotFound {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("WebDAV returned HTTP %d", res.StatusCode)
	}
	return res, nil
}

// EnsureCollection prepares the upload directory, creating parents when needed.
func (w WebDAV) EnsureCollection(ctx context.Context) error {
	base, err := w.endpoint()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return w.ensureCollection(ctx, *base)
}

// EnsureDir prepares a relative subdirectory under the WebDAV endpoint, creating parents when needed.
func (w WebDAV) EnsureDir(ctx context.Context, relDir string) error {
	if relDir == "" || relDir == "." {
		return w.EnsureCollection(ctx)
	}
	if !validWebDAVKey(relDir) {
		return errors.New("invalid WebDAV directory")
	}
	base, err := w.endpoint()
	if err != nil {
		return err
	}
	prefix := strings.TrimRight(base.EscapedPath(), "/")
	base.RawPath = prefix + "/" + relDir + "/"
	base.Path, _ = url.PathUnescape(base.RawPath)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return w.ensureCollection(ctx, *base)
}

func (w WebDAV) ensureCollection(ctx context.Context, base url.URL) error {
	path := strings.TrimRight(base.EscapedPath(), "/")
	base.RawPath = path + "/"
	// EscapedPath and its slash-delimited prefixes always have valid URL escapes.
	base.Path, _ = url.PathUnescape(base.RawPath)
	address := base.String()
	exists, err := w.collectionExists(ctx, address)
	if err != nil || exists {
		return err
	}
	if path == "" {
		return errors.New("WebDAV 服务的根目录不可用。")
	}

	status, err := w.makeCollection(ctx, address)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		parent := base
		parent.RawPath = path[:strings.LastIndex(path, "/")+1]
		parent.Path, _ = url.PathUnescape(parent.RawPath)
		if err := w.ensureCollection(ctx, parent); err != nil {
			return err
		}
		status, err = w.makeCollection(ctx, address)
		if err != nil {
			return err
		}
	}
	switch status {
	case http.StatusCreated:
		return nil
	case http.StatusMethodNotAllowed:
		// Another save or upload may have created the directory concurrently.
		if exists, err := w.collectionExists(ctx, address); exists || err != nil {
			return err
		}
	}
	return collectionError("创建", status)
}

func (w WebDAV) makeCollection(ctx context.Context, address string) (int, error) {
	res, err := w.send(ctx, "MKCOL", strings.TrimSuffix(address, "/"), nil)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return res.StatusCode, nil
}

func (w WebDAV) collectionExists(ctx context.Context, address string) (bool, error) {
	const query = `<propfind xmlns="DAV:"><prop><resourcetype/></prop></propfind>`
	res, err := w.send(ctx, "PROPFIND", address, strings.NewReader(query))
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if res.StatusCode != http.StatusMultiStatus {
		return false, collectionError("检查", res.StatusCode)
	}
	var result struct {
		XMLName   xml.Name `xml:"DAV: multistatus"`
		Responses []struct {
			Status    string `xml:"DAV: status"`
			PropStats []struct {
				Status string `xml:"DAV: status"`
				Prop   struct {
					ResourceType *struct {
						Collection *struct{} `xml:"DAV: collection"`
					} `xml:"DAV: resourcetype"`
				} `xml:"DAV: prop"`
			} `xml:"DAV: propstat"`
		} `xml:"DAV: response"`
	}
	if err := xml.NewDecoder(io.LimitReader(res.Body, 128<<10)).Decode(&result); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, errors.New("WebDAV 返回了无效的目录属性。")
	}
	for _, response := range result.Responses {
		if status := strings.Fields(response.Status); len(status) >= 2 && status[1] == "404" {
			return false, nil
		}
		for _, prop := range response.PropStats {
			status := strings.Fields(prop.Status)
			if len(status) < 2 || status[1] != "200" || prop.Prop.ResourceType == nil {
				continue
			}
			if prop.Prop.ResourceType.Collection != nil {
				return true, nil
			}
			return false, errors.New("WebDAV 目标地址指向文件。请选择目录地址。")
		}
	}
	return false, errors.New("WebDAV 服务未能确认该地址的目录属性。")
}

func collectionError(action string, status int) error {
	switch status {
	case http.StatusUnauthorized:
		return errors.New("WebDAV 账号或密码验证失败。")
	case http.StatusForbidden:
		return fmt.Errorf("没有权限%s WebDAV 目录。", action)
	default:
		return fmt.Errorf("无法%s WebDAV 目录（HTTP %d）。", action, status)
	}
}

func (w WebDAV) Put(ctx context.Context, key string, body io.Reader) error {
	if dir := path.Dir(key); dir != "." {
		if err := w.EnsureDir(ctx, dir); err != nil {
			return err
		}
	}
	res, err := w.request(ctx, http.MethodPut, key, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return nil
}

// PutMonthly uploads an image or text object into the current YYYYMM folder under the WebDAV endpoint.
func (w WebDAV) PutMonthly(ctx context.Context, name string, body io.Reader) (string, error) {
	key := MonthlyKey(name, time.Now())
	if err := w.Put(ctx, key, body); err != nil {
		return "", err
	}
	return key, nil
}

func (w WebDAV) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	res, err := w.request(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

func (w WebDAV) Delete(ctx context.Context, key string) error {
	res, err := w.request(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	return res.Body.Close()
}
