//go:build !darwin && !linux

package browser

// processes is not available here; Close relies on the process group.
func processes() []proc { return nil }
