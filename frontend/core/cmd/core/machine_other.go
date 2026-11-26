//go:build !windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
)

func machineFingerprint() (string, error) {
	var raw string
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		data, err := os.ReadFile(path)
		if err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				raw = s
				break
			}
		}
	}
	if raw == "" {
		host, _ := os.Hostname()
		var macs []string
		if ifaces, err := net.Interfaces(); err == nil {
			for _, iface := range ifaces {
				if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) == 0 {
					continue
				}
				macs = append(macs, iface.HardwareAddr.String())
			}
			sort.Strings(macs)
		}
		raw = host + "|" + strings.Join(macs, ",")
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return "", errors.New("unable to determine machine identifier")
	}
	sum := sha256.Sum256([]byte("ghosthand-machine-v1:" + runtime.GOOS + ":" + raw))
	return hex.EncodeToString(sum[:]), nil
}
