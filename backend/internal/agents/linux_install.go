package agents

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
)

const MaxLinuxInstallerFileSize = 256 << 20

var linuxInstallerNames = []string{"ghosthand-core"}

type LinuxInstallerFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

func (s *Service) SaveLinuxInstallerFile(name string, body io.Reader) (LinuxInstallerFile, error) {
	if !slices.Contains(linuxInstallerNames, name) {
		return LinuxInstallerFile{}, ErrMissing
	}
	dir := filepath.Join(s.dir, "linux-install")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return LinuxInstallerFile{}, err
	}
	file, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return LinuxInstallerFile{}, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	size, err := io.Copy(file, io.LimitReader(body, MaxLinuxInstallerFileSize+1))
	if err != nil {
		return LinuxInstallerFile{}, err
	}
	if size > MaxLinuxInstallerFileSize || !isLinuxAMD64Executable(file) {
		return LinuxInstallerFile{}, ErrInvalid
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return LinuxInstallerFile{}, err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, name)); err != nil {
		return LinuxInstallerFile{}, err
	}
	return LinuxInstallerFile{Name: name, Size: size}, nil
}

func isLinuxAMD64Executable(file io.ReaderAt) bool {
	f, err := elf.NewFile(file)
	if err != nil {
		return false
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 {
		return false
	}
	if f.Machine != elf.EM_X86_64 && f.Machine != elf.EM_AARCH64 {
		return false
	}
	return f.Type == elf.ET_EXEC || f.Type == elf.ET_DYN
}

func (s *Service) OpenLinuxInstallerFile(name string) (*os.File, os.FileInfo, error) {
	if !slices.Contains(linuxInstallerNames, name) {
		return nil, nil, ErrMissing
	}
	file, err := os.Open(filepath.Join(s.dir, "linux-install", name))
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

func (s *Service) LinuxInstallerFiles(withSHA256 bool) ([]LinuxInstallerFile, error) {
	files := make([]LinuxInstallerFile, 0, len(linuxInstallerNames))
	for _, name := range linuxInstallerNames {
		file, info, err := s.OpenLinuxInstallerFile(name)
		if errors.Is(err, ErrMissing) && !withSHA256 {
			continue
		}
		if err != nil {
			return nil, err
		}
		entry := LinuxInstallerFile{Name: name, Size: info.Size()}
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
