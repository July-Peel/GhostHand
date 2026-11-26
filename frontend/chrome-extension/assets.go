package chromeextension

import "embed"

// Files is the unpacked extension distributed inside Windows Core.
//
//go:embed manifest.json background.js content-script.js icons js
var Files embed.FS
