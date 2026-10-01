package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Render produces the Seatbelt profile (SBPL) for a normalized policy. The
// output is deterministic: identical policies render identical text, and the
// text's digest is part of every check's evidence key.
//
// Rules are ordered allow-then-deny because a later matching rule wins.
func Render(p Policy) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w("(version 1)")
	w("(deny default)")
	w("(allow process-fork)")
	w("(allow process-exec)")
	// A descendant that starts a session or a process group of its own would
	// escape the group kill. Both calls are denied; see survivors_darwin.go for
	// the posix_spawn attributes that the syscall filter does not cover.
	w("(deny syscall-unix (syscall-number SYS_setsid SYS_setpgid))")
	w("(allow process-info* (target same-sandbox))")
	w("(allow signal (target same-sandbox))")
	w("(allow sysctl-read)")
	if len(machServices) > 0 {
		b.WriteString("(allow mach-lookup")
		for _, s := range machServices {
			b.WriteString(` (global-name "` + s + `")`)
		}
		w(")")
	}
	w("(allow file-read-metadata)")
	w("(allow file-read* file-map-executable")
	w(`  (literal "/")`)
	for _, r := range systemReadRoots {
		w(`  (subpath "` + r + `")`)
	}
	for _, r := range p.Toolchains {
		w(`  (subpath "` + r + `")`)
	}
	for _, r := range p.ReadOnly {
		w(`  (subpath "` + r + `")`)
	}
	w(`  (subpath "` + p.SourceRoot + `")`)
	w(`  (subpath "` + p.ScratchRoot + `"))`)
	w("(deny file-read*")
	for _, r := range systemDenyRead {
		w(`  (subpath "` + r + `")`)
	}
	for _, r := range p.DenyRead {
		w(`  (subpath "` + r + `")`)
	}
	b.WriteString(")\n")
	w("(allow file-write*")
	w(`  (subpath "` + p.ScratchRoot + `")`)
	w(`  (subpath "` + p.SourceRoot + `")`)
	w(`  (literal "/dev/null"))`)
	w(`(allow file-ioctl (subpath "/dev"))`)
	w("(deny network*)")
	return b.String()
}

// Digest returns the hex SHA-256 of a rendered profile.
func Digest(profile string) string {
	sum := sha256.Sum256([]byte(profile))
	return hex.EncodeToString(sum[:])
}

// selfTestProfile is the minimal profile used to prove that the launcher can
// start a process at all. It is independent of any policy.
const selfTestProfile = `(version 1)
(deny default)
(allow process-fork)
(allow process-exec)
(allow sysctl-read)
(allow file-read-metadata)
(allow file-read* file-map-executable
  (literal "/")
  (subpath "/usr")
  (subpath "/System")
  (subpath "/Library")
  (subpath "/private/var/db")
  (subpath "/dev"))
(deny network*)
`
