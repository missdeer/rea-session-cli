// Package textutil holds small text helpers shared by the CLI and daemon.
package textutil

import (
	"errors"
	"io"
	"os"
	"strings"
)

// TailLines returns the last count lines of a file, reading at most the final
// 64 KiB. A missing file is not an error.
func TailLines(path string, count int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	const window = 64 << 10
	if size := info.Size(); size > window {
		if _, err := f.Seek(size-window, io.SeekStart); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n"), nil
}
