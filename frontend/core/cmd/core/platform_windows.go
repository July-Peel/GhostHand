package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"ghosthand/launcher/state"
)

var (
	kernel                  = windows.NewLazySystemDLL("kernel32.dll")
	getPower                = kernel.NewProc("GetSystemPowerStatus")
	getMemory               = kernel.NewProc("GlobalMemoryStatusEx")
	setExecutionState       = kernel.NewProc("SetThreadExecutionState")
	expandEnvStrings        = kernel.NewProc("ExpandEnvironmentStringsW")
	wts                     = windows.NewLazySystemDLL("wtsapi32.dll")
	querySession            = wts.NewProc("WTSQuerySessionInformationW")
	netapi                  = windows.NewLazySystemDLL("iphlpapi.dll")
	cancelNetworkChanges    = netapi.NewProc("CancelMibChangeNotify2")
	userenv                 = windows.NewLazySystemDLL("userenv.dll")
	expandEnvStringsForUser = userenv.NewProc("ExpandEnvironmentStringsForUserW")
)

func osVersion() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("Windows %d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}

func currentProcessSessionID() uint32 {
	var id uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &id); err != nil {
		return 0
	}
	return id
}

func powerStatus() (*int, *bool) {
	var info struct {
		ACLineStatus        byte
		BatteryFlag         byte
		BatteryLifePercent  byte
		SystemStatusFlag    byte
		BatteryLifeTime     uint32
		BatteryFullLifeTime uint32
	}
	ok, _, _ := getPower.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 || info.BatteryFlag == 128 || info.BatteryLifePercent > 100 {
		return nil, nil
	}
	pct := int(info.BatteryLifePercent)
	if info.ACLineStatus == 255 {
		return &pct, nil
	}
	charging := info.BatteryFlag != 255 && info.BatteryFlag&8 != 0
	return &pct, &charging
}

func startPlatform(ctx context.Context, stop context.CancelFunc, helper string, service bool) (*host, <-chan struct{}) {
	desktop := "no_session"
	if helper != "" {
		if currentProcessSessionID() > 0 {
			desktop = desktopState(currentProcessSessionID())
		} else if id, token, ok := interactiveSession(); ok {
			desktop = desktopState(id)
			token.Close()
		}
	}
	helperState := "none"
	if helper != "" {
		helperState = "unavailable"
	}
	p := newHost(desktop, helperState)
	done := make(chan struct{})
	sessionEvents := make(chan struct{}, 1)
	if service {
		go func() {
			reader := json.NewDecoder(os.Stdin)
			for {
				var event state.ServiceEvent
				if reader.Decode(&event) != nil {
					stop()
					return
				}
				switch event.Kind {
				case "session":
					signal(sessionEvents)
				case "power":
					switch event.Code {
					case 4: // PBT_APMSUSPEND
						p.sleeping.Store(true)
						signal(p.changed)
					case 7, 18: // PBT_APMRESUMESUSPEND, PBT_APMRESUMEAUTOMATIC
						p.sleeping.Store(false)
						p.powerChanged()
						signal(p.wake)
						signal(sessionEvents)
					default:
						p.powerChanged()
					}
				}
			}
		}()
	} else if helper != "" {
		go func() {
			tick := time.NewTicker(2 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					signal(sessionEvents)
				}
			}
		}()
	}
	closeNetwork := monitorNetwork(p.wake)
	go func() {
		defer close(done)
		defer closeNetwork()
		if helper != "" {
			p.superviseHelper(ctx, helper, sessionEvents)
			return
		}
		<-ctx.Done()
	}()
	return p, done
}

func deviceMemory() *memoryStatus {
	var info struct {
		Length, Load                     uint32
		TotalPhysical, AvailablePhysical uint64
		TotalPageFile, AvailablePageFile uint64
		TotalVirtual, AvailableVirtual   uint64
		AvailableExtendedVirtual         uint64
	}
	info.Length = uint32(unsafe.Sizeof(info))
	ok, _, _ := getMemory.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return nil
	}
	return &memoryStatus{Total: info.TotalPhysical, Available: info.AvailablePhysical}
}

func monitorNetwork(ch chan struct{}) func() {
	callback := windows.NewCallback(func(_, _, _ uintptr) uintptr {
		signal(ch)
		return 0
	})
	var handles []windows.Handle
	for _, name := range []string{"NotifyIpInterfaceChange", "NotifyUnicastIpAddressChange", "NotifyRouteChange2"} {
		var h windows.Handle
		code, _, _ := netapi.NewProc(name).Call(0, callback, 0, 0, uintptr(unsafe.Pointer(&h)))
		if code == 0 {
			handles = append(handles, h)
		} else {
			slog.Warn("network notification unavailable", "event", name, "error", syscall.Errno(code))
		}
	}
	return func() {
		for _, h := range handles {
			cancelNetworkChanges.Call(uintptr(h))
		}
	}
}

func keepAwake() func() {
	const continuous = 0x80000000
	const systemRequired = 1
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		ok, _, _ := setExecutionState.Call(continuous | systemRequired)
		close(ready)
		<-release
		if ok != 0 {
			setExecutionState.Call(continuous)
		}
	}()
	<-ready
	return func() {
		close(release)
		<-done
	}
}

func interactiveSession() (uint32, windows.Token, bool) {
	id := windows.WTSGetActiveConsoleSessionId()
	var token windows.Token
	if id != 0xffffffff && windows.WTSQueryUserToken(id, &token) == nil {
		return id, token, true
	}
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count) == nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
		for _, entry := range unsafe.Slice(sessions, int(count)) {
			if entry.SessionID != 0 && entry.State == windows.WTSActive && windows.WTSQueryUserToken(entry.SessionID, &token) == nil {
				return entry.SessionID, token, true
			}
		}
	}
	return 0, 0, false
}

// elevateUserToken extracts the linked elevated primary token for UAC-filtered admin sessions
// and optionally enables TokenUIAccess so the interactive helper can control elevated windows.
func elevateUserToken(userToken windows.Token, uiAccess bool) (windows.Token, bool) {
	const tokenElevationTypeLimited = 3
	var elevationType uint32
	var returned uint32
	source := userToken
	if err := windows.GetTokenInformation(userToken, windows.TokenElevationType, (*byte)(unsafe.Pointer(&elevationType)), 4, &returned); err == nil && elevationType == tokenElevationTypeLimited {
		var linked windows.Token
		if err := windows.GetTokenInformation(userToken, windows.TokenLinkedToken, (*byte)(unsafe.Pointer(&linked)), uint32(unsafe.Sizeof(linked)), &returned); err == nil && linked != 0 {
			source = linked
			defer linked.Close()
		}
	}
	var primary windows.Token
	if err := windows.DuplicateTokenEx(source, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return userToken, false
	}
	if uiAccess {
		var enabled uint32 = 1
		if err := windows.SetTokenInformation(primary, windows.TokenUIAccess, (*byte)(unsafe.Pointer(&enabled)), 4); err != nil {
			slog.Warn("set TokenUIAccess failed, continuing with elevated token", "error", err)
		}
	}
	return primary, true
}

func desktopState(id uint32) string {
	var buffer uintptr
	var size uint32
	const wtsSessionInfoEx = 25
	ok, _, _ := querySession.Call(0, uintptr(id), wtsSessionInfoEx, uintptr(unsafe.Pointer(&buffer)), uintptr(unsafe.Pointer(&size)))
	if ok != 0 && buffer != 0 {
		defer windows.WTSFreeMemory(buffer)
		// WTSINFOEXW pads the union to an eight-byte boundary on supported Windows x64.
		if size >= 20 && *(*uint32)(unsafe.Pointer(buffer)) == 1 {
			if flag := *(*int32)(unsafe.Pointer(buffer + 16)); flag == 1 {
				return "unlocked"
			}
		}
	}
	return "locked"
}
