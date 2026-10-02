package probe

import (
	"regexp"
	"strconv"
	"strings"
)

// denyRecord is one kernel sandbox denial: which process attempted which
// operation on which target. Only the kernel's own Sandbox records count; a
// user process can write log messages, but not as the kernel's sandbox.
type denyRecord struct {
	Proc   string
	PID    int
	Op     string
	Target string
}

var reDeny = regexp.MustCompile(`^Sandbox: (.+)\((\d+)\) deny\(\d+\) (\S+) (.*)$`)

// kernelSandbox are the record fields only the kernel's sandbox produces.
const (
	kernelImage  = "/kernel"
	sandboxImage = "/System/Library/Extensions/Sandbox.kext/Contents/MacOS/Sandbox"
)

// parseDeny reads one unified-log record; ok is false for anything that is
// not a kernel sandbox denial.
func parseDeny(processImage, senderImage, message string) (denyRecord, bool) {
	if processImage != kernelImage || senderImage != sandboxImage {
		return denyRecord{}, false
	}
	m := reDeny.FindStringSubmatch(strings.TrimSpace(message))
	if m == nil {
		return denyRecord{}, false
	}
	pid, _ := strconv.Atoi(m[2])
	return denyRecord{Proc: m[1], PID: pid, Op: m[3], Target: strings.TrimSpace(m[4])}, true
}

// helperProcess is the kernel's name of the helper test binary go test runs.
const helperProcess = "probe.test"

// deniedWrite reports whether the helper's write to path was denied by the kernel.
func deniedWrite(recs []denyRecord, path string) bool {
	for _, r := range recs {
		if r.Proc == helperProcess && strings.HasPrefix(r.Op, "file-write") && r.Target == path {
			return true
		}
	}
	return false
}

// deniedConnect reports whether the helper's connection to port was denied by the kernel.
func deniedConnect(recs []denyRecord, port string) bool {
	for _, r := range recs {
		if r.Proc == helperProcess && r.Op == "network-outbound" && strings.HasSuffix(r.Target, ":"+port) {
			return true
		}
	}
	return false
}
