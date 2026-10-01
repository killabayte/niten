//go:build !darwin

package sandbox

import "context"

// Seatbelt exists on every platform so callers compile; only macOS can run it.
type Seatbelt struct{}

// New always fails off macOS: there is no verifier backend and no fallback.
func New(profileDir string) (*Seatbelt, error) { return nil, ErrUnsupportedOS }

// OS reports nothing on unsupported platforms.
func (s *Seatbelt) OS() (version, build string) { return "", "" }

// SelfTest always fails off macOS.
func (s *Seatbelt) SelfTest(ctx context.Context) error { return ErrUnsupportedOS }

// Run always fails off macOS.
func (s *Seatbelt) Run(ctx context.Context, p Policy, cmd Command) (Result, error) {
	return Result{}, ErrUnsupportedOS
}

// Seal always fails off macOS.
func (s *Seatbelt) Seal(p Policy) (Sealed, error) { return Sealed{}, ErrUnsupportedOS }
