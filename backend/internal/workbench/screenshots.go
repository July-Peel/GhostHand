package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"
	"unicode"

	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/storage"
	"ghosthand/proto"
)

var (
	ErrUploadDenied      = errors.New("screenshot ticket is invalid")
	ErrUploadData        = errors.New("invalid JPEG upload")
	ErrChatImageData     = errors.New("图片无法读取，或分辨率超过 1600 万像素。")
	ErrChatImageSize     = errors.New("单张图片不能超过 5 MB。")
	ErrChatImageCount    = errors.New("单条消息最多上传 4 张图片。")
	ErrCommandImages     = errors.New("快捷指令不支持携带图片，请直接发送文字指令或移除图片。")
	ErrChatImagesExpired = errors.New("原消息的图片已过期，请重新添加图片。")
)

const (
	MaxChatImageSize = 5 << 20
	MaxChatImages    = 4
	maxImagePixels   = 16_000_000
	userUploadIDs    = " GLOB 'user_*'"
)

type UploadImage struct {
	Name string
	Data []byte
}

type AttachmentFiles struct {
	ID, Kind, Key, Thumb string
	Ref                  []byte
}

type imageCard struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int    `json:"size"`
}

type userMediaPayload struct {
	Attachments []imageCard `json:"attachments"`
}

type storedImage struct {
	AttachmentFiles
	name string
	size int
	sum  [sha256.Size]byte
}

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
	if err != nil || !validImageSize(config) {
		return result, ErrUploadData
	}
	full, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return result, ErrUploadData
	}
	configs, err := s.config.Read(ctx)
	if err != nil {
		return result, err
	}
	now := time.Now()
	stored, err := s.storeAttachmentImage(ctx, "", data, full, configs[settings.WebDAV], now)
	if err != nil {
		return result, err
	}
	defer func() {
		if result.AttachmentID == "" {
			s.deleteAttachmentFiles(ctx, stored.AttachmentFiles)
		}
	}()
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		if err := checkScreenshotTicket(ctx, tx, task, upload, hash); err != nil {
			return err
		}
		if err := stored.insert(ctx, tx, task, upload, time.Now()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM screenshot_tickets WHERE task_id=? AND upload_id=?", task, upload)
		return err
	})
	if err == nil {
		result.AttachmentID = stored.ID
	}
	return result, err
}

func validImageSize(config image.Config) bool {
	return config.Width > 0 && config.Height > 0 && config.Width <= maxImagePixels/config.Height
}

func (s *Service) saveUserAttachment(ctx context.Context, imageUpload UploadImage, remote settings.Configuration, now time.Time) (storedImage, error) {
	data := imageUpload.Data
	if len(data) > MaxChatImageSize {
		return storedImage{}, ErrChatImageSize
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || !validImageSize(config) {
		return storedImage{}, ErrChatImageData
	}
	full, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return storedImage{}, ErrChatImageData
	}
	if format != "jpeg" {
		opaque := image.NewRGBA(full.Bounds())
		draw.Draw(opaque, opaque.Bounds(), image.White, image.Point{}, draw.Src)
		draw.Draw(opaque, opaque.Bounds(), full, full.Bounds().Min, draw.Over)
		full = opaque
		var encoded bytes.Buffer
		// Keep the original dimensions and fit the stored JPEG within the same download and model budget.
		for quality := 88; quality > 0; quality -= 12 {
			encoded.Reset()
			if err := jpeg.Encode(&encoded, full, &jpeg.Options{Quality: quality}); err != nil {
				return storedImage{}, err
			}
			if encoded.Len() <= MaxChatImageSize {
				break
			}
		}
		if encoded.Len() > MaxChatImageSize {
			return storedImage{}, ErrChatImageSize
		}
		data = encoded.Bytes()
	}
	name := path.Base(strings.ReplaceAll(imageUpload.Name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = limitText(strings.TrimSuffix(name, path.Ext(name)), 120)
	if name == "" || name == "." || name == ".." || name == "/" {
		name = "图片"
	}
	return s.storeAttachmentImage(ctx, name+".jpg", data, full, remote, now)
}

func (s *Service) storeAttachmentImage(ctx context.Context, name string, data []byte, full image.Image, remote settings.Configuration, now time.Time) (storedImage, error) {
	var thumb bytes.Buffer
	if err := jpeg.Encode(&thumb, thumbnail(full), &jpeg.Options{Quality: 76}); err != nil {
		return storedImage{}, err
	}
	id, err := proto.NewID()
	if err != nil {
		return storedImage{}, err
	}
	files := AttachmentFiles{ID: id, Kind: "local", Key: id + ".jpg", Thumb: id + ".thumb.jpg"}
	if name == "" {
		name = "GhostHand-" + id + ".jpg"
	}
	if remote.Endpoint != "" {
		files.Kind = "webdav"
		files.Key = storage.MonthlyKey("ghosthand-"+files.Key, now)
		files.Thumb = storage.MonthlyKey("ghosthand-"+files.Thumb, now)
		files.Ref, err = s.config.SealWebDAV(id, remote)
		if err != nil {
			return storedImage{}, err
		}
	}
	backend, err := s.screenshotBackend(id, files.Kind, files.Ref)
	if err != nil {
		return storedImage{}, err
	}
	if err := backend.Put(ctx, files.Key, bytes.NewReader(data)); err != nil {
		s.deleteAttachmentFiles(ctx, files)
		return storedImage{}, err
	}
	if err := backend.Put(ctx, files.Thumb, bytes.NewReader(thumb.Bytes())); err != nil {
		s.deleteAttachmentFiles(ctx, files)
		return storedImage{}, err
	}
	return storedImage{AttachmentFiles: files, name: name, size: len(data), sum: sha256.Sum256(data)}, nil
}

func (image storedImage) insert(ctx context.Context, tx *sql.Tx, task, upload string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO attachments
		(id,task_id,upload_id,name,mime,size,sha256,created_at,storage_backend,storage_key,thumb_key,storage_ref)
		VALUES (?,?,?,?,'image/jpeg',?,?,?,?,?,?,?)`, image.ID, task, upload, image.name, image.size,
		hex.EncodeToString(image.sum[:]), now.UnixMilli(), image.Kind, image.Key, image.Thumb, image.Ref)
	return err
}

func DeviceAttachmentFiles(ctx context.Context, tx *sql.Tx, device string) ([]AttachmentFiles, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.storage_backend,a.storage_key,a.thumb_key,a.storage_ref
		FROM attachments a JOIN tasks t ON t.id=a.task_id WHERE t.device_id=?`, device)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []AttachmentFiles
	for rows.Next() {
		var file AttachmentFiles
		if err := rows.Scan(&file.ID, &file.Kind, &file.Key, &file.Thumb, &file.Ref); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (s *Service) removeAttachmentFiles(ctx context.Context, files AttachmentFiles) error {
	backend, err := s.screenshotBackend(files.ID, files.Kind, files.Ref)
	if err != nil {
		return err
	}
	return errors.Join(backend.Delete(ctx, files.Key), backend.Delete(ctx, files.Thumb))
}

func (s *Service) deleteAttachmentFiles(ctx context.Context, files AttachmentFiles) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	if err := s.removeAttachmentFiles(cleanup, files); err != nil {
		slog.Error("delete attachment files", "id", files.ID, "error", err)
	}
}

func (s *Service) deletePendingChatAttachment(ctx context.Context, files AttachmentFiles) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	if err := s.removeAttachmentFiles(cleanup, files); err != nil {
		return err
	}
	return s.store.Write(cleanup, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(cleanup, "DELETE FROM chat_attachment_deletions WHERE id=?", files.ID)
		return err
	})
}

func (s *Service) Attachment(ctx context.Context, id string, thumb bool) (io.ReadCloser, string, error) {
	var name, kind, key, thumbKey string
	var ref []byte
	err := s.store.QueryRowContext(ctx, `SELECT a.name,a.storage_backend,a.storage_key,a.thumb_key,a.storage_ref
		FROM attachments a JOIN tasks t ON t.id=a.task_id WHERE a.id=? AND
		(a.upload_id`+userUploadIDs+` OR (t.source IN ('chat','brain') AND a.upload_id GLOB 'chat_*')
		OR t.state='succeeded' OR EXISTS(SELECT 1 FROM brain_calls b WHERE b.id=a.upload_id AND b.task_id=t.id
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
