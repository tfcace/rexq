package pi

import _ "embed"

// Extension is rexq.ts as it is in the repository, before the binary's
// path is written into it. rexq install pi writes it where Pi loads
// extensions from.
//
//go:embed rexq.ts
var Extension string
