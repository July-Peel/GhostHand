package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"ghosthand/proto"
)

type taskWorker struct {
	cancel   context.CancelFunc
	wake     chan struct{}
	stopping bool
	lane     proto.Lane
}

type taskCompletion struct {
	id  string
	err error
}

func (a *agent) runTasks(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-a.welcomed:
	}
	ctx, stop := context.WithCancel(ctx)
	var group sync.WaitGroup
	defer func() {
		stop()
		group.Wait()
	}()
	active := make(map[string]*taskWorker)
	done := make(chan taskCompletion, 5)
	for ctx.Err() == nil {
		tasks, err := a.local.tasks()
		if err != nil {
			slog.Error("read task journal", "error", err)
			return
		}
		if a.local.takeHelperForce() {
			a.platform.forceHelper()
		}
		ui, bg := 0, 0
		for _, worker := range active {
			if worker.lane == proto.UILane {
				ui++
			} else {
				bg++
			}
		}
		for _, task := range tasks {
			if worker := active[task.TaskID]; worker != nil {
				stopping := task.Desired == "cancel" || task.Desired == "force"
				if stopping || (task.Finish != nil && task.CallStarted) {
					worker.cancel()
				}
				if stopping && !worker.stopping {
					worker.stopping = true
					if worker.lane == proto.UILane {
						a.platform.markOverlayCancelling(task.TaskID)
					}
				}
				signal(worker.wake)
			}
		}
		for _, task := range tasks {
			if active[task.TaskID] != nil || (task.Lane == proto.UILane && ui >= 1) || (task.Lane == proto.BackgroundLane && bg >= 4) {
				continue
			}
			if task.Lane == proto.UILane {
				ui++
			} else {
				bg++
			}
			taskCtx, cancel := context.WithCancel(ctx)
			worker := &taskWorker{cancel: cancel, wake: make(chan struct{}, 1), lane: task.Lane}
			active[task.TaskID] = worker
			group.Add(1)
			go func() {
				defer group.Done()
				defer cancel()
				err := a.executeTask(taskCtx, ctx, task, worker.wake)
				select {
				case done <- taskCompletion{task.TaskID, err}:
				case <-ctx.Done():
				}
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-a.taskChanges:
		case control := <-a.platform.controls:
			if active[control.TaskID] != nil {
				err := a.local.changeTask(control.TaskID, func(task *localTask) error {
					if task.Lane == proto.UILane {
						if control.Action == "cancel" && task.Desired != "cancel" && task.Desired != "force" {
							if err := a.local.enqueueInLock(proto.TaskHeartbeat, proto.TaskStatus{
								TaskID: task.TaskID, State: proto.Cancelling, StepIndex: task.Step,
							}); err != nil {
								return err
							}
						}
						setTaskDesired(task, control.Action)
					}
					return nil
				})
				if err != nil && !errors.Is(err, errLocalTaskMissing) {
					slog.Error("save Helper task control", "error", err)
					return
				}
			}
		case result := <-done:
			delete(active, result.id)
			if result.err != nil && ctx.Err() == nil {
				slog.Error("save task result", "task", result.id, "error", result.err)
				return
			}
		}
	}
}

func (a *agent) executeTask(ctx, process context.Context, task localTask, wake <-chan struct{}) error {
	result := proto.TaskResultPayload{State: proto.Succeeded}
	if task.Lane == proto.BackgroundLane {
		release, err := a.occupyBackground(ctx)
		if err != nil {
			if process.Err() != nil {
				return nil
			}
			result.State, result.ErrorCode = proto.Cancelled, proto.ErrCancelled
			return a.local.finishTask(task.TaskID, result)
		}
		defer release()
	}
	if task.State == proto.Queued && task.ExpiresAt <= time.Now().UnixMilli() {
		result.State = proto.Expired
		return a.local.finishTask(task.TaskID, result)
	}
	if (task.Capability != proto.BrainTask && task.Step != 0) || task.CallStarted {
		// A Core restart can interrupt a step. The server can issue a new task after this result.
		result.State, result.ErrorCode = proto.Failed, proto.ErrInternal
		return a.local.finishTask(task.TaskID, result)
	}
	if err := a.local.changeTask(task.TaskID, func(record *localTask) error {
		if record.Desired == "cancel" || record.Desired == "force" {
			return context.Canceled
		}
		if record.State == proto.Queued {
			record.State = proto.Running
			return a.local.enqueueInLock(proto.TaskAccepted, proto.TaskRef{TaskID: task.TaskID})
		}
		return nil
	}); err != nil {
		if errors.Is(err, context.Canceled) {
			return a.local.finishTask(task.TaskID, proto.TaskResultPayload{State: proto.Cancelled, ErrorCode: proto.ErrCancelled})
		}
		return err
	}
	if task.Capability == proto.BrainTask {
		return a.executeBrainTask(ctx, process, task, wake)
	}

	started := time.Now()
	checkpoint := func(step int, key, name string) error {
		return a.taskCheckpoint(ctx, task, wake, step, key, name, started)
	}
	var raw any
	var err error
	if task.Capability == proto.ScreenCapture {
		release := keepAwake()
		defer release()
		var media proto.ScreenshotResult
		media, err = a.runScreenshot(ctx, task.TaskID, wake, checkpoint)
		raw = media
	} else {
		name := "读取设备信息"
		if task.Capability == proto.DeviceBattery {
			name = "读取电源状态"
		}
		err = checkpoint(1, "read", name)
		if err == nil {
			raw = a.platform.snapshot()
		}
	}
	if err == nil {
		err = checkpoint(0, "", "")
	}
	if task.Lane == proto.UILane {
		a.platform.overlay(taskOverlay{TaskDisplay: proto.TaskDisplay{TaskID: task.TaskID}})
	}
	if process.Err() != nil {
		return nil
	}
	if err == nil {
		result.Result, err = json.Marshal(raw)
	}
	if err != nil {
		var failure taskFailure
		switch {
		case errors.Is(err, context.Canceled):
			result.State, result.ErrorCode = proto.Cancelled, proto.ErrCancelled
		case errors.As(err, &failure):
			result.State, result.ErrorCode = proto.Failed, failure.code
		case errors.Is(err, context.DeadlineExceeded):
			result.State, result.ErrorCode = proto.Failed, proto.ErrTimeout
		default:
			result.State, result.ErrorCode = proto.Failed, proto.ErrInternal
		}
		slog.Info("task interrupted", "task", task.TaskID, "error", err)
	}
	return a.local.finishTask(task.TaskID, result)
}

type taskFailure struct {
	code proto.ErrorCode
	err  error
}

func (e taskFailure) Error() string { return e.err.Error() }

func (a *agent) taskCheckpoint(ctx context.Context, task localTask, wake <-chan struct{}, step int, key, name string, started time.Time) error {
	lastOverlayState := proto.TaskState("")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var state proto.TaskState
		var current int
		err := a.local.changeTask(task.TaskID, func(record *localTask) error {
			if err := a.local.applyLocalControl(record); err != nil {
				return err
			}
			state, current = record.State, record.Step
			if state == proto.Paused || step == 0 {
				return nil
			}
			total := 1
			if task.Lane == proto.UILane {
				total = 2
			}
			record.Step, current = step, step
			return a.local.enqueueInLock(proto.TaskProgress, proto.TaskProgressPayload{
				TaskID: record.TaskID, StepIndex: step, StepTotal: total, StepKey: key,
				StepName: name, Status: "running", Attempt: 1, ElapsedMS: time.Since(started).Milliseconds(),
			})
		})
		if err != nil {
			return err
		}
		if task.Lane == proto.UILane && lastOverlayState != state &&
			(step > 0 || state == proto.Paused || lastOverlayState == proto.Paused) {
			displayName := name
			if displayName == "" && current == 2 {
				displayName = "上传截图"
			}
			a.platform.overlay(taskOverlay{Visible: true, TaskDisplay: proto.TaskDisplay{
				TaskID: task.TaskID, Title: "截图", State: state, StepIndex: current, StepTotal: 2, StepName: displayName,
			}})
			lastOverlayState = state
		}
		if state != proto.Paused {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

func (a *agent) runScreenshot(ctx context.Context, id string, wake <-chan struct{},
	checkpoint func(int, string, string) error) (proto.ScreenshotResult, error) {
	if err := checkpoint(1, "capture", "截取主显示器"); err != nil {
		return proto.ScreenshotResult{}, err
	}
	captureCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	data, err := a.platform.capture(captureCtx, id)
	cancel()
	if err != nil {
		return proto.ScreenshotResult{}, err
	}
	if err := checkpoint(2, "upload", "上传截图"); err != nil {
		return proto.ScreenshotResult{}, err
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	result, err := a.uploadScreenshot(uploadCtx, id, "", data, wake)
	if err != nil && ctx.Err() == nil {
		err = taskFailure{proto.ErrUpload, err}
	}
	return result, err
}

func (j *journal) applyLocalControl(record *localTask) error {
	switch record.Desired {
	case "cancel", "force":
		return context.Canceled
	case "pause", "resume":
		next := proto.Running
		if record.Desired == "pause" {
			next = proto.Paused
		}
		record.Desired = ""
		record.State = next
		return j.enqueueInLock(proto.TaskHeartbeat, proto.TaskStatus{TaskID: record.TaskID, State: next, StepIndex: record.Step})
	}
	return nil
}
