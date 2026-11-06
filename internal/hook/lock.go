package hook

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Locker serializes the work for one Claude session across processes.
type Locker interface {
	// Lock waits for the session's lock until ctx ends.
	Lock(ctx context.Context, claudeSession string) (unlock func(), err error)
	// TryLock takes the session's lock if it is free; ok is false if not.
	TryLock(claudeSession string) (unlock func(), ok bool, err error)
}

// LockShards is how many lock files FileLock uses. Sessions that share one
// wait for each other now and then, which costs time but nothing else.
const LockShards = 64

// lockPoll is how often Lock tries again. flock(2) can't wait with a
// deadline, so Lock polls a non-blocking flock instead.
const lockPoll = 50 * time.Millisecond

// FileLock returns a Locker that uses flock(2) on files in dir. flock locks
// belong to an open file, so it also orders goroutines within one process.
func FileLock(dir string) Locker { return fileLock{dir} }

type fileLock struct{ dir string }

func (l fileLock) Lock(ctx context.Context, claudeSession string) (func(), error) {
	t := time.NewTicker(lockPoll)
	defer t.Stop()
	for {
		unlock, ok, err := l.TryLock(claudeSession)
		if err != nil || ok {
			return unlock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (l fileLock) TryLock(claudeSession string) (func(), bool, error) {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, false, err
	}
	h := fnv.New32a()
	h.Write([]byte(claudeSession))
	name := filepath.Join(l.dir, fmt.Sprintf("%02d.lock", h.Sum32()%LockShards))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}
