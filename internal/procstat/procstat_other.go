//go:build !linux && !(darwin && cgo)

package procstat

func read(int) (Process, error) { return Process{}, ErrUnsupported }
