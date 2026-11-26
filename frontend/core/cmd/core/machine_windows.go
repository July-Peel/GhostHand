//go:build windows

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

	"golang.org/x/sys/windows/registry"
)

func machineFingerprint() (string, error) {
	raw := readRegistryString(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, "MachineGuid")
	if raw == "" {
		raw = readRegistryString(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\IDConfigDB\Hardware Profiles\0001`, "HwProfileGuid")
	}
	if raw == "" {
		raw = fallbackMachineString()
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return "", errors.New("unable to determine machine identifier")
	}
	sum := sha256.Sum256([]byte("ghosthand-machine-v1:" + runtime.GOOS + ":" + raw))
	return hex.EncodeToString(sum[:]), nil
}

func readRegistryString(root registry.Key, path, valueName string) string {
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer key.Close()
	val, _, err := key.GetStringValue(valueName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

func fallbackMachineString() string {
	host, _ := os.Hostname()
	ifaces, err := net.Interfaces()
	if err != nil {
		return host
	}
	var macs []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) == 0 {
			continue
		}
		macs = append(macs, iface.HardwareAddr.String())
	}
	sort.Strings(macs)
	return host + "|" + strings.Join(macs, ",")
}
