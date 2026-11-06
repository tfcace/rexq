// Package rexenv takes a Rex pane's identity out of an environment, for
// the processes rexq starts.
package rexenv

import "strings"

// Without returns env without REX_SESSION and REX_BLOCK, the variables that
// tell a process which Rex session and pane it runs in.
func Without(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "REX_SESSION=") || strings.HasPrefix(kv, "REX_BLOCK=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
