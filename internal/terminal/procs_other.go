//go:build !darwin

package terminal

import "errors"

// members is only implemented on macOS, where Terminal runs; elsewhere Close
// reaches the program's process group alone.
func (s *Session) members() ([]int, error) { return nil, errors.ErrUnsupported }

func (s *Session) foreground() string { return "" }
