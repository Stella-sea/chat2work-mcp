package app

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type OfficeEngine interface {
	Execute(context.Context, string, []string) (string, error)
}

// OfficeCLI runs OfficeCLI as one process per call. Auto-resident is disabled
// so no background process survives the call, matching the "no resident state"
// design and preventing cross-request leakage. A semaphore bounds how many
// OfficeCLI processes run at once.
type OfficeCLI struct {
	path string
	sem  chan struct{}
}

func NewOfficeCLI(path string, maxConcurrency int) OfficeCLI {
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	return OfficeCLI{path: path, sem: make(chan struct{}, maxConcurrency)}
}

func (o OfficeCLI) Execute(ctx context.Context, root string, args []string) (string, error) {
	if o.sem != nil {
		select {
		case o.sem <- struct{}{}:
			defer func() { <-o.sem }()
		case <-ctx.Done():
			return "", fmt.Errorf("office operation canceled: %w", ctx.Err())
		}
	}
	cmd := exec.CommandContext(ctx, o.path, args...)
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(),
		"OFFICECLI_SKIP_UPDATE=1",
		"OFFICECLI_NO_AUTO_RESIDENT=1",
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("office operation timed out: %w", ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("officecli failed: %s", strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// checkOfficeInput bounds the size and expansion of a document before it is
// handed to OfficeCLI. OOXML files are ZIP archives, so the uncompressed-size
// cap guards against decompression bombs.
func checkOfficeInput(path string, maxBytes, maxUncompressed int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return fmt.Errorf("document exceeds the %d byte limit", maxBytes)
	}
	if info.Size() == 0 {
		return nil
	}
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil // not a ZIP-based document; let OfficeCLI decide
	}
	defer reader.Close()
	var total uint64
	for _, entry := range reader.File {
		total += entry.UncompressedSize64
		if maxUncompressed > 0 && total > uint64(maxUncompressed) {
			return fmt.Errorf("document expands beyond the %d byte safety limit", maxUncompressed)
		}
	}
	return nil
}

// tempDocumentPath returns a sibling path with the same extension, so OfficeCLI
// still infers the format while the real document stays untouched until the
// operation succeeds.
func tempDocumentPath(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s.chat2work-%d%s", base, time.Now().UnixNano(), ext)
}
