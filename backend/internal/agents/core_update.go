package agents

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"ghosthand/proto"
)

var (
	ErrCoreUpdateUnsupported = errors.New("device does not support remote Core updates")
	ErrCoreUpdateOffline     = errors.New("device must be online for a Core update")
	ErrCoreUpdateMissing     = errors.New("no Core installer has been uploaded for this platform")
	ErrCoreUpdateArch        = errors.New("uploaded Core does not match the device architecture")
)

func coreUpdateTarget(ctx context.Context, q rowQuerier, id string) (proto.Platform, string, error) {
	var platform proto.Platform
	var arch, caps string
	var state proto.ConnState
	err := q.QueryRowContext(ctx, `SELECT d.platform,d.arch,d.caps_hash,d.conn_state
		FROM devices d JOIN device_credentials c ON c.device_id=d.id
		WHERE d.id=? AND c.revoked_at IS NULL`, id).Scan(&platform, &arch, &caps, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrMissing
	}
	if err != nil {
		return "", "", err
	}
	if !proto.SupportsCoreUpdate(platform, caps) {
		return "", "", ErrCoreUpdateUnsupported
	}
	if state != proto.Online {
		return "", "", ErrCoreUpdateOffline
	}
	return platform, arch, nil
}

func (s *Service) queueCoreUpdate(ctx context.Context, user int64, device string) (string, error) {
	platform, arch, err := coreUpdateTarget(ctx, s.store, device)
	if err != nil {
		return "", err
	}
	staged, release, err := s.stageCoreRelease(platform, arch)
	if err != nil {
		return "", err
	}
	defer os.Remove(staged)
	var operationID string
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		currentPlatform, currentArch, err := coreUpdateTarget(ctx, tx, device)
		if err != nil {
			return err
		}
		if currentPlatform != platform || currentArch != arch {
			return ErrCoreUpdateArch
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_releases(id,version,platform,arch,component,size,sha256,signature,created_at)
			VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(platform,arch,component,version) DO NOTHING`,
			release.ID, release.Version, platform, arch, release.Component, release.Size, release.SHA256,
			release.Signature, time.Now().UnixMilli()); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT id,signature FROM agent_releases
			WHERE platform=? AND arch=? AND component=? AND version=?`, platform, arch, release.Component, release.Version).
			Scan(&release.ID, &release.Signature); err != nil {
			return err
		}
		operationID, err = queue(ctx, tx, user, device, proto.AgentUpdate, 0, &release)
		if err != nil {
			return err
		}
		path := filepath.Join(s.dir, "core-updates", release.SHA256)
		_, err = os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return os.Rename(staged, path)
		}
		return err
	})
	return operationID, err
}

func (s *Service) stageCoreRelease(platform proto.Platform, arch string) (string, proto.Release, error) {
	var file *os.File
	var info os.FileInfo
	var err error
	switch platform {
	case proto.Windows:
		file, info, err = s.OpenWindowsInstallerFile("ghosthand-core.exe")
	case proto.Linux:
		file, info, err = s.OpenLinuxInstallerFile("ghosthand-core")
	default:
		return "", proto.Release{}, ErrCoreUpdateUnsupported
	}
	if errors.Is(err, ErrMissing) {
		return "", proto.Release{}, ErrCoreUpdateMissing
	}
	if err != nil {
		return "", proto.Release{}, err
	}
	defer file.Close()
	if (platform == proto.Windows && (arch != "amd64" || !isWindowsAMD64Executable(file))) ||
		(platform == proto.Linux && linuxExecutableArch(file) != arch) {
		return "", proto.Release{}, ErrCoreUpdateArch
	}
	id, err := proto.NewID()
	if err != nil {
		return "", proto.Release{}, err
	}
	hash := sha256.New()
	staged, err := storeFile(filepath.Join(s.dir, "core-updates"), io.TeeReader(file, hash), proto.MaxCoreUpdateSize)
	if err != nil {
		return "", proto.Release{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	release := proto.Release{ID: id, Version: digest, Platform: platform, Arch: arch,
		Component: proto.CoreUpdateComponent, Size: info.Size(), SHA256: digest}
	release.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, release.SignedBytes()))
	return staged, release, nil
}

func (s *Service) OpenCoreUpdate(ctx context.Context, device, token, release string) (*os.File, error) {
	if _, _, err := authenticate(ctx, s.store, device, token); err != nil {
		return nil, err
	}
	var digest string
	err := s.store.QueryRowContext(ctx, `SELECT r.sha256 FROM agent_releases r
		JOIN device_operations o ON o.release_id=r.id
		WHERE r.id=? AND r.component=? AND o.device_id=? AND o.kind='agent.update' AND o.state='queued'`,
		release, proto.CoreUpdateComponent, device).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, err
	}
	if bytes, err := hex.DecodeString(digest); err != nil || len(bytes) != sha256.Size {
		return nil, ErrMissing
	}
	file, err := os.Open(filepath.Join(s.dir, "core-updates", digest))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrMissing
	}
	return file, err
}

func matchingCoreUpdate(ctx context.Context, tx *sql.Tx, device, digest string) (string, error) {
	var operation string
	err := tx.QueryRowContext(ctx, `SELECT o.id FROM device_operations o JOIN agent_releases r ON o.release_id=r.id
		WHERE o.device_id=? AND o.kind='agent.update' AND o.state='queued' AND r.sha256=? AND r.component=?`,
		device, digest, proto.CoreUpdateComponent).Scan(&operation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return operation, err
}
