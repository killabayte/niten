package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestProbe attempts to break out of the verifier sandbox from inside a test
// binary and records what happened. It is a negative control: every attempt
// named deny_* must fail, every allow_* must succeed. The report is written to
// NITEN_PROBE_REPORT; without that variable the test is skipped.
func TestProbe(t *testing.T) {
	out := os.Getenv("NITEN_PROBE_REPORT")
	if out == "" {
		t.Skip("NITEN_PROBE_REPORT not set")
	}
	outside := os.Getenv("NITEN_PROBE_OUTSIDE")
	store := os.Getenv("NITEN_PROBE_STORE")
	gitdir := os.Getenv("NITEN_PROBE_GITDIR")
	home := os.Getenv("NITEN_PROBE_HOME")
	tmpfile := os.Getenv("NITEN_PROBE_TMPFILE")
	port := os.Getenv("NITEN_PROBE_PORT")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	type attempt struct {
		Allowed bool   `json:"allowed"`
		Err     string `json:"err,omitempty"`
	}
	m := map[string]attempt{}
	rec := func(name string, err error) {
		a := attempt{Allowed: err == nil}
		if err != nil {
			a.Err = err.Error()
		}
		m[name] = a
	}
	write := func(p string) error { return os.WriteFile(p, []byte("escape"), 0o644) }
	read := func(p string) error { _, err := os.ReadFile(p); return err }

	rec("allow_write_source", write(filepath.Join(wd, "probe_ok_source.txt")))
	rec("allow_write_scratch", write(filepath.Join(os.Getenv("TMPDIR"), "probe_ok_scratch.txt")))
	rec("allow_read_source", read(filepath.Join(wd, "go.mod")))
	rec("deny_write_outside", write(filepath.Join(outside, "escape.txt")))
	rec("deny_write_store", write(filepath.Join(store, "escape.txt")))
	rec("deny_write_gitdir", write(filepath.Join(gitdir, "escape")))
	rec("deny_write_shared_tmp", write(tmpfile))
	rec("deny_read_store_canary", read(filepath.Join(store, "canary.txt")))
	rec("deny_read_home_credential", read(filepath.Join(home, ".claude", "credentials.json")))

	link := filepath.Join(wd, "probe_escape_link")
	os.Remove(link)
	if err := os.Symlink(outside, link); err != nil {
		rec("deny_symlink_escape", err)
	} else {
		rec("deny_symlink_escape", write(filepath.Join(link, "via_symlink.txt")))
	}

	sh := exec.Command("/bin/sh", "-c", "echo x > "+filepath.Join(outside, "shell.txt"))
	rec("deny_child_shell_escape", sh.Run())

	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err == nil {
		c.Close()
	}
	rec("deny_network_loopback", err)
	_, err = net.LookupHost("example.com")
	rec("deny_network_dns", err)

	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
