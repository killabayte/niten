//go:build darwin

package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// denyWatch streams the kernel's sandbox denials for the window of one model
// call. The window opens and closes with a sentinel denial the host causes
// itself: seeing the opening one proves the log is observable, and seeing the
// closing one proves every earlier denial was delivered.
type denyWatch struct {
	cmd      *exec.Cmd
	mu       sync.Mutex
	recs     []denyRecord
	sentinel string
	done     chan struct{}
}

// sentinelWait bounds the wait for a sentinel; under load the stream can take
// seconds to attach and to deliver.
const sentinelWait = 60 * time.Second

func startDenyWatch(ctx context.Context, dir, name string) (*denyWatch, error) {
	sentinel := filepath.Join(dir, "sentinel-"+name)
	if err := os.MkdirAll(sentinel, 0o700); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/log", "stream", "--style", "ndjson", "--predicate", `sender == "Sandbox"`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the kernel log stream: %w", err)
	}
	w := &denyWatch{cmd: cmd, sentinel: sentinel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			var e struct {
				Process string `json:"processImagePath"`
				Sender  string `json:"senderImagePath"`
				Message string `json:"eventMessage"`
			}
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if r, ok := parseDeny(e.Process, e.Sender, e.Message); ok {
				w.mu.Lock()
				w.recs = append(w.recs, r)
				w.mu.Unlock()
			}
		}
	}()
	if err := w.mark(ctx, "open"); err != nil {
		w.cmd.Process.Kill()
		<-w.done
		return nil, err
	}
	return w, nil
}

// mark causes a denial of a sentinel path under a minimal profile and waits
// until the stream delivers it. A stream that has not attached yet misses a
// denial, so a fresh sentinel is caused every second until one arrives: the
// first to arrive proves the stream is live, and, the kernel's records being
// delivered in order, that every earlier denial has arrived too.
func (w *denyWatch) mark(ctx context.Context, name string) error {
	profile := fmt.Sprintf(`(version 1)(allow default)(deny file-write* (subpath %q))`, w.sentinel)
	deadline := time.Now().Add(sentinelWait)
	var caused []string
	for i := 0; time.Now().Before(deadline); i++ {
		p := filepath.Join(w.sentinel, fmt.Sprintf("%s-%d", name, i))
		caused = append(caused, p)
		_ = exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, "/usr/bin/touch", p).Run()
		for until := time.Now().Add(time.Second); time.Now().Before(until); {
			if w.seenAny(caused) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return errors.New("the kernel sandbox log is not observable: no " + name + " sentinel denial arrived")
}

func (w *denyWatch) seenAny(paths []string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.recs {
		for _, p := range paths {
			if r.Target == p {
				return true
			}
		}
	}
	return false
}

// stop closes the window and returns every denial seen in it, sentinels excluded.
func (w *denyWatch) stop(ctx context.Context) ([]denyRecord, error) {
	err := w.mark(ctx, "close")
	w.cmd.Process.Kill()
	<-w.done
	w.cmd.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []denyRecord
	for _, r := range w.recs {
		if filepath.Dir(r.Target) != w.sentinel {
			out = append(out, r)
		}
	}
	return out, err
}
