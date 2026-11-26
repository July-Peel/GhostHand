package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"ghosthand/proto"
	"golang.org/x/sys/windows"
)

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

type processMemoryCounters struct {
	Size, PageFaults               uint32
	PeakWorkingSet, WorkingSet     uintptr
	PeakPagedPool, PagedPool       uintptr
	PeakNonPagedPool, NonPagedPool uintptr
	Pagefile, PeakPagefile         uintptr
}

type processInfo struct {
	PID     uint32   `json:"pid"`
	Name    string   `json:"name"`
	Memory  *float64 `json:"memory_mb"`
	CPU     *float64 `json:"cpu_pct,omitempty"`
	Session *uint32  `json:"session_id,omitempty"`
}

type cpuSample struct {
	index  int
	handle windows.Handle
	used   uint64
	at     time.Time
}

func listProcesses(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	needle := strings.ToLower(strings.TrimSpace(args.Filter))
	processes := make([]processInfo, 0)
	samples := make([]cpuSample, 0)
	defer func() {
		for _, sample := range samples {
			_ = windows.CloseHandle(sample.handle)
		}
	}()
	entry := windows.ProcessEntry32{}
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		info := processInfo{PID: entry.ProcessID, Name: windows.UTF16ToString(entry.ExeFile[:])}
		if !strings.Contains(strings.ToLower(info.Name), needle) && strconv.FormatUint(uint64(info.PID), 10) != needle {
			continue
		}
		var session uint32
		if windows.ProcessIdToSessionId(info.PID, &session) == nil {
			info.Session = &session
		}
		handle, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, info.PID)
		if openErr == nil {
			var memory processMemoryCounters
			memory.Size = uint32(unsafe.Sizeof(memory))
			ok, _, _ := getProcessMemoryInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&memory)), uintptr(memory.Size))
			if ok != 0 {
				mb := float64(memory.WorkingSet) / (1 << 20)
				info.Memory = &mb
			}
			used, measured := uint64(0), false
			if args.SortBy == "cpu" {
				used, measured = processCPUTime(handle)
			}
			if measured {
				samples = append(samples, cpuSample{len(processes), handle, used, time.Now()})
			} else {
				_ = windows.CloseHandle(handle)
			}
		}
		processes = append(processes, info)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	if len(samples) > 0 {
		timer := time.NewTimer(150 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		for _, sample := range samples {
			used, ok := processCPUTime(sample.handle)
			if ok && used >= sample.used {
				pct := float64(used-sample.used) * 10000 / float64(time.Since(sample.at).Nanoseconds()) / float64(runtime.NumCPU())
				processes[sample.index].CPU = &pct
			}
		}
	}
	slices.SortFunc(processes, func(a, b processInfo) int {
		var order int
		switch args.SortBy {
		case "name":
			order = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		case "cpu":
			order = compareProcessMetric(a.CPU, b.CPU)
		case "pid":
			return cmp.Compare(a.PID, b.PID)
		default:
			order = compareProcessMetric(a.Memory, b.Memory)
		}
		if order == 0 {
			return cmp.Compare(a.PID, b.PID)
		}
		return order
	})
	limit := args.Limit
	if limit == 0 {
		limit = 50
	}
	return json.Marshal(struct {
		Total     int           `json:"total_count"`
		Processes []processInfo `json:"processes"`
	}{len(processes), processes[:min(limit, len(processes))]})
}

func processCPUTime(handle windows.Handle) (uint64, bool) {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil {
		return 0, false
	}
	return fileTimeTicks(kernel) + fileTimeTicks(user), true
}

func compareProcessMetric(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return cmp.Compare(*b, *a)
	}
}
