package agents

import (
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
)

const MaxWindowsInstallerFileSize = 256 << 20

var windowsInstallerNames = []string{"GhostHand.Helper.exe", "ghosthand-core.exe", "ghosthand-launcher.exe"}

type WindowsInstallerFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

func (s *Service) SaveWindowsInstallerFile(name string, body io.Reader) (WindowsInstallerFile, error) {
	if !slices.Contains(windowsInstallerNames, name) {
		return WindowsInstallerFile{}, ErrMissing
	}
	dir := filepath.Join(s.dir, "windows-install")
	file, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return WindowsInstallerFile{}, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	size, err := io.Copy(file, io.LimitReader(body, MaxWindowsInstallerFileSize+1))
	if err != nil {
		return WindowsInstallerFile{}, err
	}
	if size > MaxWindowsInstallerFileSize || !isWindowsAMD64Executable(file) {
		return WindowsInstallerFile{}, ErrInvalid
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return WindowsInstallerFile{}, err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, name)); err != nil {
		return WindowsInstallerFile{}, err
	}
	return WindowsInstallerFile{Name: name, Size: size}, nil
}

func isWindowsAMD64Executable(file io.ReaderAt) bool {
	var dos [64]byte
	if _, err := file.ReadAt(dos[:], 0); err != nil || string(dos[:2]) != "MZ" {
		return false
	}
	offset := int64(binary.LittleEndian.Uint32(dos[60:]))
	var header [26]byte
	if _, err := file.ReadAt(header[:], offset); err != nil || string(header[:4]) != "PE\x00\x00" {
		return false
	}
	flags := binary.LittleEndian.Uint16(header[22:24])
	return binary.LittleEndian.Uint16(header[4:6]) == pe.IMAGE_FILE_MACHINE_AMD64 &&
		binary.LittleEndian.Uint16(header[24:26]) == 0x20b &&
		flags&pe.IMAGE_FILE_EXECUTABLE_IMAGE != 0 && flags&pe.IMAGE_FILE_DLL == 0
}

func (s *Service) OpenWindowsInstallerFile(name string) (*os.File, os.FileInfo, error) {
	if !slices.Contains(windowsInstallerNames, name) {
		return nil, nil, ErrMissing
	}
	file, err := os.Open(filepath.Join(s.dir, "windows-install", name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrMissing
	}
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err == nil {
			err = ErrMissing
		}
		return nil, nil, err
	}
	return file, info, nil
}

func (s *Service) WindowsInstallerFiles(withSHA256 bool) ([]WindowsInstallerFile, error) {
	files := make([]WindowsInstallerFile, 0, len(windowsInstallerNames))
	for _, name := range windowsInstallerNames {
		file, info, err := s.OpenWindowsInstallerFile(name)
		if errors.Is(err, ErrMissing) && !withSHA256 {
			continue
		}
		if err != nil {
			return nil, err
		}
		entry := WindowsInstallerFile{Name: name, Size: info.Size()}
		if withSHA256 {
			hash := sha256.New()
			_, err = io.Copy(hash, file)
			entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
		}
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		files = append(files, entry)
	}
	return files, nil
}
