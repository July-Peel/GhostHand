package main

import (
	"encoding/json"
	"errors"
	"sync"

	"ghosthand/proto"
)

var errSequence = errors.New("invalid reliable sequence")

type pendingOperation struct {
	Operation proto.Operation   `json:"operation"`
	Kind      proto.MessageType `json:"kind"`
	Phase     string            `json:"phase,omitempty"`
	Seq       uint64            `json:"seq"`
	ResultSeq uint64            `json:"result_seq,omitempty"`
}

type journal struct {
	mu          sync.Mutex
	fresh       bool
	inbound     uint64
	outboundAck uint64
	outboxSeq   uint64
	outbox      []proto.Envelope
	inbox       map[string]struct{}
	inboxOrder  []inboxEntry
	operations  map[uint64]pendingOperation
	opOrder     []uint64
	tasksMap    map[string]localTask
	taskResults map[string]json.RawMessage
	resultOrder []string
	changed     chan struct{}
}

type inboxEntry struct {
	seq uint64
	id  string
}

func openMemoryJournal() *journal {
	return &journal{
		fresh:       true,
		inbox:       make(map[string]struct{}),
		operations:  make(map[uint64]pendingOperation),
		tasksMap:    make(map[string]localTask),
		taskResults: make(map[string]json.RawMessage),
		changed:     make(chan struct{}, 1),
	}
}

func (j *journal) resetForFreshSession() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.fresh = true
	j.inbound = 0
	j.outboundAck = 0
	j.outboxSeq = 0
	j.outbox = nil
	j.inbox = make(map[string]struct{})
	j.inboxOrder = nil
	j.operations = make(map[uint64]pendingOperation)
	j.opOrder = nil
	j.tasksMap = make(map[string]localTask)
	j.taskResults = make(map[string]json.RawMessage)
	j.resultOrder = nil
}

func (j *journal) isFresh() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.fresh
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (j *journal) acknowledgeLocked(ack uint64) error {
	if ack > j.outboxSeq {
		return errSequence
	}
	if ack <= j.outboundAck {
		return nil
	}
	idx := 0
	for idx < len(j.outbox) && j.outbox[idx].Seq <= ack {
		idx++
	}
	if idx > 0 {
		j.outbox = append([]proto.Envelope(nil), j.outbox[idx:]...)
	}
	j.outboundAck = ack
	return nil
}

func (j *journal) acceptWelcome(ack uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ack < j.outboundAck {
		return errSequence
	}
	if err := j.acknowledgeLocked(ack); err != nil {
		return err
	}
	j.fresh = false
	return nil
}

func (j *journal) enqueueLocked(kind proto.MessageType, payload any) (uint64, error) {
	message, err := proto.NewEnvelope(kind, payload)
	if err != nil {
		return 0, err
	}
	j.outboxSeq++
	message.Seq = j.outboxSeq
	j.outbox = append(j.outbox, message)
	return message.Seq, nil
}

func (j *journal) enqueueCaps(payload proto.CapabilitiesReport) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, frame := range j.outbox {
		if frame.Type == proto.CapsReport {
			var existing proto.CapabilitiesReport
			if json.Unmarshal(frame.Payload, &existing) == nil && existing.Hash == payload.Hash {
				return nil
			}
		}
	}
	if _, err := j.enqueueLocked(proto.CapsReport, payload); err != nil {
		return err
	}
	signal(j.changed)
	return nil
}

func (j *journal) framesAfter(seq uint64) ([]proto.Envelope, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var frames []proto.Envelope
	for _, frame := range j.outbox {
		if frame.Seq > seq {
			frames = append(frames, frame)
			if len(frames) >= 64 {
				break
			}
		}
	}
	return frames, nil
}

func (j *journal) receive(frame proto.Envelope) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.acknowledgeLocked(frame.Ack); err != nil {
		return 0, err
	}
	if frame.Seq <= j.inbound {
		return j.inbound, nil
	}
	if frame.Seq != j.inbound+1 {
		return 0, errSequence
	}
	if _, seen := j.inbox[frame.ID]; !seen {
		if isTaskFrame(frame.Type) {
			if err := j.receiveTaskLocked(frame); err != nil {
				return 0, err
			}
		} else {
			var data proto.Operation
			if err := json.Unmarshal(frame.Payload, &data); err != nil {
				return 0, err
			}
			operation := pendingOperation{Operation: data, Kind: frame.Type, Seq: frame.Seq}
			j.operations[operation.Seq] = operation
			j.opOrder = append(j.opOrder, operation.Seq)
		}
		j.inbox[frame.ID] = struct{}{}
		j.inboxOrder = append(j.inboxOrder, inboxEntry{seq: frame.Seq, id: frame.ID})
	}
	j.inbound = frame.Seq
	if j.inbound > 200 {
		cutoff := j.inbound - 200
		idx := 0
		for idx < len(j.inboxOrder) && j.inboxOrder[idx].seq <= cutoff {
			delete(j.inbox, j.inboxOrder[idx].id)
			idx++
		}
		if idx > 0 {
			j.inboxOrder = append([]inboxEntry(nil), j.inboxOrder[idx:]...)
		}
	}
	return j.inbound, nil
}

func (j *journal) pending() ([]pendingOperation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	result := make([]pendingOperation, 0, len(j.operations))
	for _, seq := range j.opOrder {
		if op, ok := j.operations[seq]; ok {
			result = append(result, op)
		}
	}
	return result, nil
}

func (j *journal) phase(op pendingOperation, phase string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	op.Phase = phase
	j.operations[op.Seq] = op
	return nil
}

func (j *journal) reportUninstall(op pendingOperation) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if op.ResultSeq != 0 {
		return op.ResultSeq, nil
	}
	seq, err := j.enqueueLocked(proto.OperationResult, proto.OperationOutcome{
		ID: op.Operation.ID, Success: true, Detail: "Windows 服务清理已启动",
	})
	if err != nil {
		return 0, err
	}
	op.ResultSeq = seq
	j.operations[op.Seq] = op
	signal(j.changed)
	return op.ResultSeq, nil
}

func (j *journal) resultAcknowledged(seq uint64) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.outboundAck >= seq, nil
}

func (j *journal) finish(op pendingOperation, outcome proto.OperationOutcome) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	kind := proto.OperationResult
	switch op.Kind {
	case proto.AgentUpdate, proto.AgentRollback:
		kind = proto.AgentUpdateResult
	case proto.LogCollect:
		kind = proto.LogReady
	}
	if _, err := j.enqueueLocked(kind, outcome); err != nil {
		return err
	}
	delete(j.operations, op.Seq)
	signal(j.changed)
	return nil
}
