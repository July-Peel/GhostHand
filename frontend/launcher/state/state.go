package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const (
	Base       = "base"
	CoreName   = "ghosthand-core.exe"
	HelperName = "GhostHand.Helper.exe"
	PipeName   = "ghosthand-core"
)

var IDPattern = regexp.MustCompile("^[0-7][0-9A-HJKMNP-TV-Z]{25}$")

type Layout struct {
	Root      string
	Bootstrap string
}

func (l Layout) Runtime(name string) string { return filepath.Join(l.Root, "runtime", name) }
func (l Layout) Version(name string) string {
	if name == Base {
		return l.Bootstrap
	}
	return filepath.Join(l.Root, "versions", name)
}
func (l Layout) Health(id string) string { return l.Runtime("healthy-" + id) }

type Request struct {
	ID           string `json:"id"`
	Candidate    string `json:"candidate,omitempty"`
	Rollback     bool   `json:"rollback,omitempty"`
	VerifyHelper bool   `json:"verify_helper,omitempty"`
}

type Trial struct {
	Request
	Fallback string    `json:"fallback"`
	Deadline time.Time `json:"deadline"`
	Failures int       `json:"failures"`
}

type Outcome struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
	Detail  string `json:"detail"`
}

type Versions struct {
	Active   string    `json:"active"`
	Previous string    `json:"previous,omitempty"`
	Trial    *Trial    `json:"trial,omitempty"`
	Outcomes []Outcome `json:"outcomes,omitempty"`
}

func ReadVersions(layout Layout) (Versions, error) {
	v := Versions{Active: Base}
	err := ReadJSON(layout.Runtime("versions.json"), &v)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	validVersion := func(value string) bool { return value == Base || IDPattern.MatchString(value) }
	if !validVersion(v.Active) || (v.Previous != "" && !validVersion(v.Previous)) ||
		(v.Trial != nil && (!IDPattern.MatchString(v.Trial.ID) || !validVersion(v.Trial.Fallback))) {
		return v, errors.New("invalid launcher state")
	}
	return v, nil
}

func (v *Versions) Record(outcome Outcome) {
	v.Outcomes = append(v.Outcomes, outcome)
	if len(v.Outcomes) > 32 {
		v.Outcomes = v.Outcomes[len(v.Outcomes)-32:]
	}
}

func ReadJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func WriteJSON(path string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return WriteFile(path, data)
}

func WriteFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ghosthand-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err != nil {
		file.Close()
		return err
	}
	err = errors.Join(file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return replaceFile(file.Name(), path)
}

type ServiceEvent struct {
	Kind string `json:"kind"`
	Code uint32 `json:"code"`
}
