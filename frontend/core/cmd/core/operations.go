package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"ghosthand/proto"
)

func (a *agent) runOperations(ctx context.Context, managed bool) {
	select {
	case <-ctx.Done():
		return
	case <-a.welcomed:
	}
	for ctx.Err() == nil {
		operations, err := a.local.pending()
		if err != nil {
			slog.Error("read operations", "error", err)
			return
		}
		waiting := false
		for _, op := range operations {
			done, restart, err := a.runOperation(ctx, op, managed)
			if ctx.Err() != nil {
				return
			}
			if restart {
				slog.Info("restarting Core", "operation", op.Operation.ID, "kind", op.Kind)
				return
			}
			if !done {
				waiting = true
				continue
			}
			outcome := proto.OperationOutcome{ID: op.Operation.ID, Success: err == nil, Detail: "completed"}
			if err != nil {
				outcome.Detail = err.Error()
				if len(outcome.Detail) > 2048 {
					outcome.Detail = outcome.Detail[:2048]
				}
			}
			if err := a.local.finish(op, outcome); err != nil {
				slog.Error("persist operation result", "error", err)
				return
			}
			slog.Info("operation finished", "operation", op.Operation.ID, "kind", op.Kind, "success", outcome.Success, "detail", outcome.Detail)
		}
		var timer *time.Timer
		var next <-chan time.Time
		if waiting {
			timer = time.NewTimer(time.Second)
			next = timer.C
		}
		select {
		case <-ctx.Done():
		case <-a.operations:
		case <-next:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (a *agent) runOperation(ctx context.Context, op pendingOperation, managed bool) (done, restart bool, err error) {
	switch op.Kind {
	case proto.AgentRestart:
		if !managed {
			return true, false, errors.New("Core is not managed by the launcher")
		}
		if op.Phase == "restarting" {
			return true, false, nil
		}
		err := a.local.phase(op, "restarting")
		return err != nil, err == nil, err
	case proto.HelperRestart:
		helperCtx, cancel := context.WithTimeout(ctx, 18*time.Second)
		defer cancel()
		return true, false, a.platform.restartHelper(helperCtx)
	case proto.LogCollect:
		release, err := a.occupyBackground(ctx)
		if err != nil {
			return true, false, err
		}
		defer release()
		return true, false, a.collectThenUpload(ctx, op.Operation)
	case proto.AgentUpdate, proto.AgentRollback:
		return true, false, errors.New("节点为无文件模式，请直接替换程序并用固定密钥重新启动接入")
	case proto.AgentUninstall:
		return true, false, a.uninstall(ctx, op)
	default:
		return true, false, errors.New("unsupported operation")
	}
}

type uninstallWorker struct {
	input io.WriteCloser
	done  <-chan error
}

func (a *agent) uninstall(ctx context.Context, op pendingOperation) error {
	if !a.canUninstall {
		return errors.New("remote uninstall requires a managed Windows installation")
	}
	worker, err := startUninstallWorker(ctx)
	if err != nil {
		return err
	}
	defer worker.input.Close()
	seq, err := a.local.reportUninstall(op)
	if err != nil {
		return err
	}
	// Give the controller time to persist the handoff. Cleanup still proceeds if the link drops.
	if err := a.waitForUninstallAck(ctx, seq); err != nil {
		return err
	}
	if err := a.cleanupBrowserExtension(ctx); err != nil {
		slog.Warn("browser extension cleanup during uninstall", "error", err)
	}
	if _, err := io.WriteString(worker.input, "uninstall\n"); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-worker.done:
		return err
	}
}

func (a *agent) waitForUninstallAck(ctx context.Context, seq uint64) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for {
		ack, err := a.local.resultAcknowledged(seq)
		if err != nil || ack {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-poll.C:
		}
	}
}

type permanentError struct{ error }

func retry(ctx context.Context, action func() error) error {
	var last error
	for i := 0; i < 5; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = action()
		var permanent permanentError
		var status statusError
		if last == nil || errors.As(last, &permanent) ||
			(errors.As(last, &status) && status >= 400 && status < 500 && status != 429 && status != 408) {
			return last
		}
		if i == 4 {
			break
		}
		timer := time.NewTimer(time.Duration(1<<i) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
}
