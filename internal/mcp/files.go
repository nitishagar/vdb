package mcp

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
)

// openPkg opens a .vpkg for streaming install, deriving the file name.
func openPkg(path string) (io.ReadCloser, *vdbv1.InstallMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: open package: %w", err)
	}
	return f, &vdbv1.InstallMeta{FileName: filepath.Base(path)}, nil
}

// writeAll writes data to path (creating/truncating), with parent dirs.
func writeAll(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mcp: create dir: %w", err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("mcp: write %s: %w", path, err)
	}
	return nil
}
