package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ProjectDirName is the name of the folder under ~/.claude/projects that
// holds the transcripts of conversations started in dir: Claude Code turns
// every character but ASCII letters and digits into "-", so "/" and "."
// both become "-".
func ProjectDirName(dir string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, dir)
}

// LaunchDir returns the directory a conversation was started in, which
// claude --resume needs and which its process keeps as its working
// directory. The agent's cwd moves during a session (into a subdirectory,
// even into another repository), but the transcript stays in the folder
// named for the launch directory.
//
// It walks up from cwd to the first directory whose folder name is the
// transcript's. Failing that (the agent left the launch directory, or the
// name was shortened), it takes the cwd of the transcript's first entry
// that has one, and then cwd itself.
func LaunchDir(transcriptPath, cwd string) string {
	if transcriptPath == "" {
		return cwd
	}
	if cwd != "" {
		want := filepath.Base(filepath.Dir(transcriptPath))
		for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
			if ProjectDirName(d) == want {
				return d
			}
			if parent := filepath.Dir(d); parent == d {
				break
			}
		}
	}
	if first := FirstCwd(transcriptPath); first != "" {
		return first
	}
	return cwd
}

// firstCwdLines is how many lines FirstCwd reads. The first entries are
// bookkeeping without a cwd; the first prompt comes within a few lines.
const firstCwdLines = 100

// FirstCwd returns the cwd of the first entry near the start of the
// transcript at path that has one, or "" when there is none or the file
// can't be read.
func FirstCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for i := 0; i < firstCwdLines && sc.Scan(); i++ {
		var e struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Cwd != "" {
			return e.Cwd
		}
	}
	return ""
}
