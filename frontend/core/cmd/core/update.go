package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ghosthand/launcher/state"
	"ghosthand/proto"
)

const coreUpdateCheck = "--check-core-update"

func regularCoreFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func coreBinarySHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func coreUpdateDir(core, operation string) string {
	return filepath.Join(filepath.Dir(core), ".ghosthand-core-update-"+operation)
}

func (a *agent) runCoreUpdate(ctx context.Context, op proto.Operation) error {
	if a.coreSHA256 == "" {
		return errors.New("远程升级需要受支持的 Core 服务安装")
	}
	cfg := *a.cfg.Load()
	origin, err := url.Parse(cfg.Server)
	if err != nil || (origin.Scheme != "https" && !(origin.Scheme == "http" &&
		(strings.EqualFold(origin.Hostname(), "localhost") || net.ParseIP(origin.Hostname()).IsLoopback()))) {
		return errors.New("远程升级需要 HTTPS 控制端")
	}
	if err := verifyCoreRelease(cfg, op.Release); err != nil {
		return err
	}
	if op.Release.SHA256 == a.coreSHA256 {
		return nil
	}
	stage := coreUpdateDir(a.corePath, op.ID)
	if err := os.Mkdir(stage, 0700); err != nil {
		return fmt.Errorf("创建 Core 升级目录: %w", err)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = os.RemoveAll(stage)
		}
	}()
	candidate := filepath.Join(stage, filepath.Base(a.corePath))
	downloadCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	err = retry(downloadCtx, func() error {
		return a.downloadCoreUpdate(downloadCtx, *op.Release, candidate)
	})
	cancel()
	if err != nil {
		return fmt.Errorf("下载 Core 升级文件: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	check := exec.CommandContext(checkCtx, candidate, coreUpdateCheck)
	quietCoreUpdateCheck(check)
	output, err := check.Output()
	cancel()
	if err != nil || strings.TrimSpace(string(output)) != proto.CoreUpdateComponent {
		return errors.New("上传文件无法启动或未实现 Core 升级协议")
	}
	if err := os.WriteFile(filepath.Join(stage, "expected"), []byte(op.Release.SHA256), 0600); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	worker, err := coreUpdateWorker(a.corePath, stage)
	if err != nil {
		return err
	}
	if err := worker.Start(); err != nil {
		return fmt.Errorf("启动 Core 升级进程: %w", err)
	}
	handedOff = true
	done := make(chan error, 1)
	go func() { done <- worker.Wait() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		if _, backupErr := os.Stat(filepath.Join(stage, "previous")); errors.Is(backupErr, os.ErrNotExist) {
			handedOff = false
		}
		if err != nil {
			return fmt.Errorf("Core 升级进程退出: %w", err)
		}
		return errors.New("Core 升级进程在重启前退出")
	}
}

func verifyCoreRelease(cfg configuration, release *proto.Release) error {
	if release == nil || release.Component != proto.CoreUpdateComponent || release.Platform != proto.Platform(runtime.GOOS) ||
		release.Arch != runtime.GOARCH || release.Size <= 0 || release.Size > proto.MaxCoreUpdateSize {
		return errors.New("Core 升级清单与此设备不匹配")
	}
	digest, err := hex.DecodeString(release.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("Core 升级清单 SHA-256 无效")
	}
	key, keyErr := base64.RawURLEncoding.DecodeString(cfg.UpdateKey)
	signature, sigErr := base64.RawURLEncoding.DecodeString(release.Signature)
	if keyErr != nil || len(key) != ed25519.PublicKeySize || sigErr != nil || !ed25519.Verify(key, release.SignedBytes(), signature) {
		return errors.New("Core 升级清单签名校验失败")
	}
	return nil
}

func (a *agent) downloadCoreUpdate(ctx context.Context, release proto.Release, path string) error {
	cfg := *a.cfg.Load()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Server+proto.CoreUpdatePath+release.ID, nil)
	if err != nil {
		return permanentError{err}
	}
	authorized(req, cfg)
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return statusError(res.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return permanentError{err}
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(res.Body, release.Size+1))
	if err != nil {
		_ = file.Close()
		return err
	}
	if size != release.Size || hex.EncodeToString(hash.Sum(nil)) != release.SHA256 {
		_ = file.Close()
		return permanentError{errors.New("升级文件大小或 SHA-256 不匹配")}
	}
	if err := errors.Join(file.Chmod(0755), file.Sync(), file.Close()); err != nil {
		return permanentError{err}
	}
	return nil
}

func (a *agent) confirmCoreUpdate(operation string) error {
	if !state.IDPattern.MatchString(operation) || a.coreSHA256 == "" {
		return errors.New("invalid Core update acknowledgment")
	}
	stage := coreUpdateDir(a.corePath, operation)
	digest, err := os.ReadFile(filepath.Join(stage, "expected"))
	if err == nil {
		if string(digest) != a.coreSHA256 {
			return errors.New("running Core does not match the staged update")
		}
		if err := os.WriteFile(filepath.Join(stage, "ready"), nil, 0600); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return a.local.reportCoreUpdate(operation)
}
