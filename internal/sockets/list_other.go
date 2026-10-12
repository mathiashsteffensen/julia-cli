//go:build !darwin

package sockets

import "context"

// List is only needed beside the macOS sandbox; elsewhere it is unsupported.
func List(context.Context, ...string) ([]Socket, error) { return nil, ErrUnsupported }
