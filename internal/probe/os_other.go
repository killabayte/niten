//go:build !darwin

package probe

import "runtime"

func osIdentity() (string, string) { return runtime.GOOS, "unknown" }
