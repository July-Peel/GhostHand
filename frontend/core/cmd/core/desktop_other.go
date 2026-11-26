//go:build !windows

package main

import (
	"context"

	"ghosthand/proto"
)

func (a *agent) desktopOperation(_ context.Context, kind proto.MessageType, req proto.DesktopRequest, _ func(string)) proto.DesktopResult {
	return proto.DesktopResult{OperationID: req.OperationID, Kind: kind, Code: proto.ErrCapUnsupported}
}
