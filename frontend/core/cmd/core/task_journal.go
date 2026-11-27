package main

import (
	"encoding/json"
	"errors"
	"runtime"

	"ghosthand/launcher/state"
	"ghosthand/proto"
)

type localTask struct {
	proto.TaskAssignment
	State         proto.TaskState          `json:"state"`
	Step          int                      `json:"step"`
	Desired       string                   `json:"desired,omitempty"`
	TicketRequest string                   `json:"ticket_request,omitempty"`
	Ticket        proto.ScreenshotTicket   `json:"ticket"`
	Display       proto.TaskDisplay        `json:"display"`
	Call          *proto.MCPCall           `json:"call,omitempty"`
	CallStarted   bool                     `json:"call_started,omitempty"`
	LastCall      string                   `json:"last_call,omitempty"`
	Finish        *proto.TaskResultPayload `json:"finish,omitempty"`
}

func validTaskFrame(frame proto.Envelope) bool {
	switch frame.Type {
	case proto.TaskAssign:
		var task proto.TaskAssignment
		if json.Unmarshal(frame.Payload, &task) != nil || !proto.TaskIDPattern.MatchString(task.TaskID) || task.ExpiresAt <= 0 {
			return false
		}
		for _, cap := range proto.TaskCapabilities(proto.Platform(runtime.GOOS)) {
			if cap.ID == task.Capability && cap.Lane == task.Lane {
				return true
			}
		}
		return false
	case proto.TaskPause, proto.TaskResume, proto.TaskCancel:
		var control proto.TaskControl
		return json.Unmarshal(frame.Payload, &control) == nil && proto.TaskIDPattern.MatchString(control.TaskID) &&
			(!control.Force || frame.Type == proto.TaskCancel)
	case proto.MCPToolCall:
		var call proto.MCPCall
		if json.Unmarshal(frame.Payload, &call) != nil || !state.IDPattern.MatchString(call.CallID) ||
			call.TaskID != call.Display.TaskID || !validDisplay(call.Display) || call.Tool == proto.ToolConfirm || len(call.Window) > 24 {
			return false
		}
		_, err := proto.ValidateTool(call.Tool, call.Arguments)
		return err == nil
	case proto.MCPTaskDisplay:
		var display proto.TaskDisplay
		return json.Unmarshal(frame.Payload, &display) == nil && validDisplay(display)
	case proto.MCPTaskFinish:
		var result proto.TaskResultPayload
		return json.Unmarshal(frame.Payload, &result) == nil && proto.TaskIDPattern.MatchString(result.TaskID) &&
			(result.State == proto.Succeeded || result.State == proto.Failed || result.State == proto.Cancelled)
	case proto.UploadTicket:
		var ticket proto.ScreenshotTicket
		if json.Unmarshal(frame.Payload, &ticket) != nil || !proto.TaskIDPattern.MatchString(ticket.TaskID) ||
			!state.IDPattern.MatchString(ticket.RequestID) {
			return false
		}
		if ticket.AttachmentID != "" {
			return state.IDPattern.MatchString(ticket.AttachmentID)
		}
		_, err := proto.TokenHash(ticket.Token)
		return err == nil && ticket.ExpiresAt > 0
	default:
		return false
	}
}

func validDisplay(display proto.TaskDisplay) bool {
	return proto.TaskIDPattern.MatchString(display.TaskID) && display.StepIndex >= 0 &&
		display.StepTotal >= display.StepIndex && display.StepTotal <= 1000 && len(display.Title) <= 480 && len(display.StepName) <= 480
}

func isTaskFrame(kind proto.MessageType) bool {
	switch kind {
	case proto.TaskAssign, proto.TaskPause, proto.TaskResume, proto.TaskCancel, proto.UploadTicket,
		proto.MCPToolCall, proto.MCPTaskDisplay, proto.MCPTaskFinish:
		return true
	}
	return false
}

func (j *journal) receiveTaskLocked(frame proto.Envelope) error {
	var ref proto.TaskRef
	if err := json.Unmarshal(frame.Payload, &ref); err != nil {
		return err
	}
	task, exists := j.tasksMap[ref.TaskID]
	if frame.Type == proto.TaskAssign {
		if exists {
			return nil
		}
		if result, ok := j.taskResults[ref.TaskID]; ok {
			_, err := j.enqueueLocked(proto.TaskResult, result)
			return err
		}
		var assigned localTask
		if err := json.Unmarshal(frame.Payload, &assigned.TaskAssignment); err != nil {
			return err
		}
		assigned.State = proto.Queued
		j.tasksMap[assigned.TaskID] = assigned
		return nil
	}
	if !exists {
		return nil
	}
	switch frame.Type {
	case proto.TaskPause:
		setTaskDesired(&task, "pause")
	case proto.TaskResume:
		setTaskDesired(&task, "resume")
	case proto.TaskCancel:
		var control proto.TaskControl
		if err := json.Unmarshal(frame.Payload, &control); err != nil {
			return err
		}
		action := "cancel"
		if control.Force && task.Lane == proto.UILane {
			action = "force"
			if task.Desired != "force" {
				j.forceHelperPending = true
			}
		}
		setTaskDesired(&task, action)
	case proto.UploadTicket:
		var ticket proto.ScreenshotTicket
		if err := json.Unmarshal(frame.Payload, &ticket); err != nil {
			return err
		}
		if ticket.RequestID == task.TicketRequest {
			task.Ticket = ticket
		}
	case proto.MCPToolCall:
		var call proto.MCPCall
		if err := json.Unmarshal(frame.Payload, &call); err != nil {
			return err
		}
		if task.Capability == proto.BrainTask && task.Call == nil && task.LastCall != call.CallID && task.Finish == nil {
			task.Call = &call
			task.Display = call.Display
		}
	case proto.MCPTaskDisplay:
		if task.Capability == proto.BrainTask {
			if err := json.Unmarshal(frame.Payload, &task.Display); err != nil {
				return err
			}
		}
	case proto.MCPTaskFinish:
		if task.Capability == proto.BrainTask {
			var result proto.TaskResultPayload
			if err := json.Unmarshal(frame.Payload, &result); err != nil {
				return err
			}
			task.Finish = &result
		}
	}
	j.tasksMap[task.TaskID] = task
	return nil
}

func setTaskDesired(task *localTask, next string) {
	if task.Desired == "force" || (task.Desired == "cancel" && next != "force") {
		return
	}
	task.Desired = next
}

func (j *journal) tasks() ([]localTask, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	tasks := make([]localTask, 0, len(j.tasksMap))
	for _, task := range j.tasksMap {
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (j *journal) takeHelperForce() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	pending := j.forceHelperPending
	j.forceHelperPending = false
	return pending
}

var errLocalTaskMissing = errors.New("local task no longer active")

func (j *journal) changeTask(id string, update func(*localTask) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	task, ok := j.tasksMap[id]
	if !ok {
		return errLocalTaskMissing
	}
	if err := update(&task); err != nil {
		return err
	}
	if _, stillExists := j.tasksMap[id]; stillExists {
		j.tasksMap[id] = task
	}
	signal(j.changed)
	return nil
}

func (j *journal) enqueueInLock(kind proto.MessageType, payload any) error {
	_, err := j.enqueueLocked(kind, payload)
	return err
}

func (j *journal) finishTask(id string, result proto.TaskResultPayload) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	task, ok := j.tasksMap[id]
	if !ok {
		return errLocalTaskMissing
	}
	if task.Desired == "cancel" || task.Desired == "force" {
		result.State, result.ErrorCode, result.Result = proto.Cancelled, proto.ErrCancelled, nil
	}
	result.TaskID = id
	if _, err := j.enqueueLocked(proto.TaskResult, result); err != nil {
		return err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	j.taskResults[id] = raw
	j.resultOrder = append(j.resultOrder, id)
	for len(j.resultOrder) > 100 {
		oldest := j.resultOrder[0]
		j.resultOrder = j.resultOrder[1:]
		delete(j.taskResults, oldest)
	}
	delete(j.tasksMap, id)
	signal(j.changed)
	return nil
}

func (j *journal) taskInventory() (ack uint64, live []proto.RunningTask, pending []string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	ack = j.inbound
	for _, task := range j.tasksMap {
		live = append(live, proto.RunningTask{TaskID: task.TaskID, State: task.State, StepIndex: task.Step})
	}
	seen := make(map[string]bool)
	for _, frame := range j.outbox {
		if frame.Type == proto.TaskResult {
			var result proto.TaskResultPayload
			if json.Unmarshal(frame.Payload, &result) == nil && !seen[result.TaskID] && len(pending) < 1200 {
				seen[result.TaskID] = true
				pending = append(pending, result.TaskID)
			}
		}
	}
	return ack, live, pending, nil
}
