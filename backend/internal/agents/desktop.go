package agents

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"ghosthand/proto"
)

var ErrDesktopOffline = errors.New("device is offline")
var ErrDesktopUnsupported = errors.New("device does not support desktop operations")
var ErrBrowserStatus = errors.New("device could not read browser extension state")

type desktopRequest struct {
	kind     proto.MessageType
	payload  proto.DesktopRequest
	results  chan proto.DesktopResult
	progress func(string) bool
}

func (s *Service) desktopPeer(device string) (*peer, error) {
	s.connectionMu.Lock()
	p := s.connections[device]
	s.connectionMu.Unlock()
	if p == nil {
		return nil, ErrDesktopOffline
	}
	if !p.desktopOperations {
		return nil, ErrDesktopUnsupported
	}
	return p, nil
}

func requestDesktop(ctx context.Context, p *peer, kind proto.MessageType, payload proto.DesktopRequest, progress func(string) bool) (proto.DesktopResult, error) {
	req := desktopRequest{kind, payload, make(chan proto.DesktopResult, 1), progress}
	p.desktopPending.Store(payload.OperationID, req)
	defer p.desktopPending.Delete(payload.OperationID)
	select {
	case p.desktopRequests <- req:
	case <-p.desktopDone:
		return proto.DesktopResult{}, ErrDesktopOffline
	case <-ctx.Done():
		return proto.DesktopResult{}, ctx.Err()
	}
	select {
	case result := <-req.results:
		return result, nil
	case <-p.desktopDone:
		return proto.DesktopResult{}, ErrDesktopOffline
	case <-ctx.Done():
		return proto.DesktopResult{}, ctx.Err()
	}
}

func deliverDesktopResult(p *peer, result proto.DesktopResult) bool {
	if value, ok := p.desktopPending.Load(result.OperationID); ok {
		req := value.(desktopRequest)
		if req.kind == result.Kind {
			if result.Stage != "" {
				return req.progress != nil && req.progress(result.Stage)
			}
			select {
			case req.results <- result:
			default:
			}
		}
	}
	return false
}

func (s *Service) BrowserExtensionStatus(ctx context.Context, device string) (proto.BrowserExtensionStatus, error) {
	var status proto.BrowserExtensionStatus
	p, err := s.desktopPeer(device)
	if err != nil {
		return status, err
	}
	id, err := proto.NewID()
	if err != nil {
		return status, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := requestDesktop(ctx, p, proto.BrowserExtensionStatusRequest, proto.DesktopRequest{
		OperationID: id, ExpiresAt: time.Now().Add(10 * time.Second).UnixMilli(),
	}, nil)
	if err != nil {
		return status, err
	}
	if !result.OK || json.Unmarshal(result.Detail, &status) != nil || status.Browser != "chrome" {
		return status, ErrBrowserStatus
	}
	return status, nil
}
