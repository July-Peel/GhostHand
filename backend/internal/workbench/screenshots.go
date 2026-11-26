package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"os"
	"time"

	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/storage"
	"ghosthand/proto"
)

var (
	ErrUploadDenied = errors.New("screenshot ticket is invalid")
	ErrUploadData   = errors.New("invalid JPEG upload")
)

const validTicket = ` FROM screenshot_tickets x JOIN tasks t ON t.id=x.task_id
	JOIN device_credentials c ON c.device_id=t.device_id
	WHERE x.task_id=? AND x.upload_id=? AND x.token_hash=? AND x.expires_at>? AND c.revoked_at IS NULL
	AND t.state IN ('running','paused')`

func checkScreenshotTicket(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, task, upload, hash string) error {
	var valid bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1"+validTicket+")", task, upload, hash, time.Now().UnixMilli()).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrUploadDenied
	}
	return nil
}

func (s *Service) SaveScreenshot(ctx context.Context, task, upload, token string, body io.Reader) (proto.ScreenshotResult, error) {
	result := proto.ScreenshotResult{}
	hash, err := proto.TokenHash(token)
	if err != nil || !proto.TaskIDPattern.MatchString(task) || (upload != "" && len(upload) != 26) {
		return result, ErrUploadDenied
	}
	if err := checkScreenshotTicket(ctx, s.store, task, upload, hash); err != nil {
		return result, err
	}
	data, err := io.ReadAll(io.LimitReader(body, proto.MaxScreenshotSize+1))
	if err != nil {
		return result, err
	}
	if len(data) > proto.MaxScreenshotSize {
		return result, ErrUploadData
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return result, ErrUploadData
	}
	full, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return result, ErrUploadData
	}
	var thumb bytes.Buffer
	if err := jpeg.Encode(&thumb, thumbnail(full), &jpeg.Options{Quality: 76}); err != nil {
		return result, err
	}
	id, err := proto.NewID()
	if err != nil {
		return result, err
	}
	configs, err := s.config.Read(ctx)
	if err != nil {
		return result, err
	}
	now := time.Now()
	kind := "local"
	key, thumbKey := id+".jpg", id+".thumb.jpg"
	var ref []byte
	if remote := configs[settings.WebDAV]; remote.Endpoint != "" {
		kind = "webdav"
		key = storage.MonthlyKey("ghosthand-"+key, now)
		thumbKey = storage.MonthlyKey("ghosthand-"+thumbKey, now)
		ref, err = s.config.SealWebDAV(id, remote)
		if err != nil {
			return result, err
		}
	}
	backend, err := s.screenshotBackend(id, kind, ref)
	if err != nil {
		return result, err
	}
	defer func() {
		if result.AttachmentID == "" {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
			defer cancel()
			_ = backend.Delete(cleanup, key)
			_ = backend.Delete(cleanup, thumbKey)
		}
	}()
	if err := backend.Put(ctx, key, bytes.NewReader(data)); err != nil {
		return result, err
	}
	if err := backend.Put(ctx, thumbKey, bytes.NewReader(thumb.Bytes())); err != nil {
		return result, err
	}
	sum := sha256.Sum256(data)
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		if err := checkScreenshotTicket(ctx, tx, task, upload, hash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments
			(id,task_id,upload_id,name,mime,size,sha256,created_at,storage_backend,storage_key,thumb_key,storage_ref)
			VALUES (?,?,?,?,'image/jpeg',?,?,?,?,?,?,?)`, id, task, upload, "GhostHand-"+id+".jpg",
			len(data), hex.EncodeToString(sum[:]), time.Now().UnixMilli(), kind, key, thumbKey, ref); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM screenshot_tickets WHERE task_id=? AND upload_id=?", task, upload)
		return err
	})
	if err == nil {
		result.AttachmentID = id
	}
	return result, err
}

func (s *Service) Attachment(ctx context.Context, id string, thumb bool) (io.ReadCloser, string, error) {
	var name, kind, key, thumbKey string
	var ref []byte
	err := s.store.QueryRowContext(ctx, `SELECT a.name,a.storage_backend,a.storage_key,a.thumb_key,a.storage_ref
		FROM attachments a JOIN tasks t ON t.id=a.task_id WHERE a.id=? AND
		(t.state='succeeded' OR EXISTS(SELECT 1 FROM brain_calls b WHERE b.id=a.upload_id AND b.task_id=t.id
			AND json_extract(b.result_json,'$.attachment_id')=a.id))`, id).Scan(&name, &kind, &key, &thumbKey, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrTaskNotFound
	}
	if err != nil {
		return nil, "", err
	}
	backend, err := s.screenshotBackend(id, kind, ref)
	if err != nil {
		return nil, "", err
	}
	if thumb {
		key = thumbKey
	}
	body, err := backend.Get(ctx, key)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrTaskNotFound
	}
	return body, name, err
}

func (s *Service) screenshotBackend(id, kind string, ref []byte) (storage.Backend, error) {
	switch kind {
	case "local":
		return storage.Local{Directory: s.fileDir}, nil
	case "webdav":
		config, err := s.config.OpenWebDAV(id, ref)
		if err != nil {
			return nil, err
		}
		return storage.WebDAV{Endpoint: config.Endpoint, Username: config.Label, Password: config.Secret}, nil
	default:
		return nil, errors.New("unsupported screenshot storage")
	}
}

func thumbnail(source image.Image) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if largest := max(width, height); largest > 480 {
		width, height = max(1, width*480/largest), max(1, height*480/largest)
	}
	result := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			result.Set(x, y, source.At(bounds.Min.X+x*bounds.Dx()/width, bounds.Min.Y+y*bounds.Dy()/height))
		}
	}
	return result
}
