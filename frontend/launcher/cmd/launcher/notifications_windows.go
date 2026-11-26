package main

import (
	"log/slog"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func configureNetworkTrigger(service windows.Handle) error {
	firstAddress, err := windows.GUIDFromString("{4F27F2DE-14E2-430B-A549-7CD48CBC8245}")
	if err != nil {
		return err
	}
	const ipAddressAvailability = 2
	const startService = 1
	trigger := struct {
		Type      uint32
		Action    uint32
		Subtype   *windows.GUID
		ItemCount uint32
		Items     unsafe.Pointer
	}{Type: ipAddressAvailability, Action: startService, Subtype: &firstAddress}
	info := struct {
		Count    uint32
		Triggers unsafe.Pointer
		Reserved unsafe.Pointer
	}{Count: 1, Triggers: unsafe.Pointer(&trigger)}
	err = windows.ChangeServiceConfig2(service, windows.SERVICE_CONFIG_TRIGGER_INFO, (*byte)(unsafe.Pointer(&info)))
	runtime.KeepAlive(info)
	return err
}

func subscribePower() func() {
	user := windows.NewLazySystemDLL("user32.dll")
	subscribe := user.NewProc("RegisterPowerSettingNotification")
	unsubscribe := user.NewProc("UnregisterPowerSettingNotification")
	const serviceRecipient = 1
	var subscriptions []uintptr
	for _, id := range []string{
		"{5D3E9A59-E9D5-4B00-A6BD-FF34FF516548}", // AC/DC power source
		"{A7AD8041-B45A-4CAE-87A3-EECBB468A9E1}", // Battery percentage
	} {
		guid, err := windows.GUIDFromString(id)
		if err != nil {
			continue
		}
		handle, _, err := subscribe.Call(uintptr(svc.StatusHandle()), uintptr(unsafe.Pointer(&guid)), serviceRecipient)
		if handle == 0 {
			slog.Warn("power notification unavailable", "setting", id, "error", err)
		} else {
			subscriptions = append(subscriptions, handle)
		}
	}
	return func() {
		for _, handle := range subscriptions {
			unsubscribe.Call(handle)
		}
	}
}
