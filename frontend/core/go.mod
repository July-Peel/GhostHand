module ghosthand/core

go 1.23.0

require (
	ghosthand/chrome-extension v0.0.0
	ghosthand/launcher v0.0.0
	ghosthand/proto v0.0.0
	github.com/Microsoft/go-winio v0.6.2
	github.com/coder/websocket v1.8.14
	go.etcd.io/bbolt v1.3.11
	golang.org/x/sys v0.28.0
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
)

require golang.org/x/crypto v0.31.0 // indirect

replace ghosthand/launcher => ../launcher
replace ghosthand/chrome-extension => ../chrome-extension
replace ghosthand/proto => ../../proto
