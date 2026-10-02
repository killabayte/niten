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

const sentinelWait = 20 * time.Second

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
// until the stream delivers it.
func (w *denyWatch) mark(ctx context.Context, name string) error {
	p := filepath.Join(w.sentinel, name)
	profile := fmt.Sprintf(`(version 1)(allow default)(deny file-write* (subpath %q))`, w.sentinel)
	_ = exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, "/usr/bin/touch", p).Run()
	deadline := time.Now().Add(sentinelWait)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		for _, r := range w.recs {
			if r.Target == p {
				w.mu.Unlock()
				return nil
			}
		}
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("the kernel sandbox log is not observable: the sentinel denial " + name + " never arrived")
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
