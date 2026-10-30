package mcp

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// EnvVarScreenshotDir confines MCP screenshot writes to a caller-chosen
// directory (default: the server's working directory). Writes outside the
// root are rejected — the MCP peer must not gain arbitrary-path writes.
const EnvVarScreenshotDir = "VDB_SCREENSHOT_DIR"

// writeAll writes data to path (creating/truncating) constrained to root.
func writeAll(root, path string, data []byte) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("mcp: resolve path: %w", err)
	}
	if root == "" {
		root = "."
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("mcp: resolve root: %w", err)
	}
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return fmt.Errorf("mcp: path %s is outside the screenshot root %s", path, root)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return fmt.Errorf("mcp: write %s: %w", abs, err)
	}
	return nil
}
