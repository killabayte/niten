package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/killabayte/niten/internal/contract"
)

func exec(args ...string) (contract.ExitCode, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		if code, out, _ := exec(args...); code != contract.ExitOK || !strings.Contains(out, "Usage: niten") {
			t.Fatalf("%v: code %d, out %q", args, code, out)
		}
	}
	code, out, _ := exec("version")
	if code != contract.ExitOK || !strings.HasPrefix(out, "niten "+version+" "+runtime.GOOS) {
		t.Fatalf("version: code %d, out %q", code, out)
	}
}

func TestNoCommandAndUnknownCommand(t *testing.T) {
	if code, _, errb := exec(); code != contract.ExitFormat || !strings.Contains(errb, "Usage: niten") {
		t.Fatalf("no command: code %d, err %q", code, errb)
	}
	if code, _, errb := exec("frobnicate"); code != contract.ExitFormat || !strings.Contains(errb, "unknown command") {
		t.Fatalf("unknown: code %d, err %q", code, errb)
	}
}

func TestUnimplementedCommandsRefuseExplicitly(t *testing.T) {
	for cmd, stage := range plannedStage {
		code, out, errb := exec(cmd, "whatever")
		if code != contract.ExitFormat || out != "" || !strings.Contains(errb, "not implemented") || !strings.Contains(errb, stage) {
			t.Errorf("%s: code %d, out %q, err %q", cmd, code, out, errb)
		}
	}
}

func TestDoctorOffline(t *testing.T) {
	code, out, _ := exec("doctor")
	if !strings.Contains(out, "contract schemas: ") || !strings.Contains(out, "never calls Claude or Codex") {
		t.Fatalf("doctor output incomplete:\n%s", out)
	}
	if runtime.GOOS == "darwin" {
		if code != contract.ExitOK || !strings.Contains(out, "verifier sandbox self-test: ok") {
			t.Fatalf("doctor on macOS: code %d\n%s", code, out)
		}
	} else if code != contract.ExitRejected || !strings.Contains(out, "unavailable") {
		t.Fatalf("doctor off macOS must fail closed: code %d\n%s", code, out)
	}
}

func TestDoctorLiveIsRefused(t *testing.T) {
	code, out, errb := exec("doctor", "--live")
	if code != contract.ExitFormat || out != "" || !strings.Contains(errb, "separately authorized") {
		t.Fatalf("doctor --live: code %d, out %q, err %q", code, out, errb)
	}
	if code, _, _ := exec("doctor", "extra"); code != contract.ExitFormat {
		t.Fatalf("doctor with a positional argument must be rejected, got %d", code)
	}
}
