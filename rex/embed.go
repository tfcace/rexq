// Package rex holds rexq's Rex module (rexq.lua), which rexq install
// writes next to Rex's init.lua.
package rex

import _ "embed"

// Module is rexq.lua as it is in the repository, before the binary's
// path is written into it.
//
//go:embed rexq.lua
var Module string
