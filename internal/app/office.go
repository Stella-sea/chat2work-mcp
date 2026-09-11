package app

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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
