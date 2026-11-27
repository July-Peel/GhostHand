//go:build !windows && !linux

package main

import (
	"context"

	"ghosthand/proto"
)

func runTerminal(_ context.Context, _ proto.TerminalFrame, _ <-chan terminalEvent, _ func(string)) proto.TerminalFrame {
	return proto.TerminalFrame{Error: "该设备不支持远程终端。"}
}
