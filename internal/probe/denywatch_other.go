//go:build !darwin

package probe

import (
	"context"
	"errors"
)

type denyWatch struct{}

func startDenyWatch(ctx context.Context, dir, name string) (*denyWatch, error) {
	return nil, errors.New("the kernel sandbox log is observable only on macOS")
}

func (w *denyWatch) stop(ctx context.Context) ([]denyRecord, error) { return nil, nil }
