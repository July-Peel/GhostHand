package main

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"time"
	"unsafe"

	"ghosthand/proto"
	"golang.org/x/sys/windows"
)

var getSystemTimes = kernel.NewProc("GetSystemTimes")

func readSystemMetrics(ctx context.Context, args proto.GUIArgs) systemMetrics {
	metrics := systemMetrics{CPU: systemCPU(ctx)}
	token := windows.GetCurrentProcessToken()
	if user, err := token.GetTokenUser(); err == nil {
		switch {
		case user.User.Sid.IsWellKnown(windows.WinLocalSystemSid):
			metrics.CoreAccount = "system"
		case token.IsElevated():
			metrics.CoreAccount = "elevated"
		default:
			metrics.CoreAccount = "standard"
		}
	}
	if args.IncludeDisks == nil || *args.IncludeDisks {
		metrics.Disks = fixedDisks()
	}
	if args.IncludeNetwork == nil || *args.IncludeNetwork {
		metrics.Addresses = localAddresses()
	}
	return metrics
}

func localAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var local []string
	for _, address := range addrs {
		prefix, err := netip.ParsePrefix(address.String())
		if err != nil {
			continue
		}
		ip := prefix.Addr().Unmap()
		if ip.IsGlobalUnicast() {
			local = append(local, ip.String())
		}
	}
	slices.Sort(local)
	return slices.Compact(local)
}

func fixedDisks() []diskStatus {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var disks []diskStatus
	for i := range 26 {
		if mask&(1<<i) == 0 {
			continue
		}
		root := []uint16{uint16('A' + i), ':', '\\', 0}
		if windows.GetDriveType(&root[0]) != windows.DRIVE_FIXED {
			continue
		}
		disk := diskStatus{Drive: windows.UTF16ToString(root)}
		if err := windows.GetDiskFreeSpaceEx(&root[0], nil, &disk.Total, &disk.Free); err == nil {
			disks = append(disks, disk)
		}
	}
	return disks
}

func systemCPU(ctx context.Context) *float64 {
	idle, total, ok := sampleSystemCPU()
	if !ok {
		return nil
	}
	timer := time.NewTimer(120 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
	}
	nextIdle, nextTotal, ok := sampleSystemCPU()
	if !ok || nextTotal <= total || nextIdle < idle || nextIdle-idle > nextTotal-total {
		return nil
	}
	pct := 100 * (1 - float64(nextIdle-idle)/float64(nextTotal-total))
	return &pct
}

func sampleSystemCPU() (idle, total uint64, ok bool) {
	var idleTime, kernelTime, userTime windows.Filetime
	success, _, _ := getSystemTimes.Call(uintptr(unsafe.Pointer(&idleTime)), uintptr(unsafe.Pointer(&kernelTime)), uintptr(unsafe.Pointer(&userTime)))
	return fileTimeTicks(idleTime), fileTimeTicks(kernelTime) + fileTimeTicks(userTime), success != 0
}

func fileTimeTicks(value windows.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}
