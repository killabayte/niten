//go:build darwin

package probe

import "syscall"

func osIdentity() (string, string) {
	v, _ := syscall.Sysctl("kern.osproductversion")
	b, _ := syscall.Sysctl("kern.osversion")
	return v, b
}
