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
type OfficeCLI struct{ path string }

func (o OfficeCLI) Execute(ctx context.Context, root string, args []string) (string, error) {
	output, err := o.run(ctx, root, args)
	if ctx.Err() != nil {
		return "", fmt.Errorf("office operation timed out: %w", ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("officecli failed: %s", strings.TrimSpace(string(output)))
	}
	// create and batch may start a resident process. Close it before returning so
	// files are durably flushed and no tenant document remains open between calls.
	if len(args) > 1 && (args[0] == "create" || args[0] == "batch") {
		if _, err := o.run(ctx, root, []string{"close", args[1]}); err != nil {
			return "", fmt.Errorf("officecli could not flush document: %w", err)
		}
	}
	return string(output), nil
}

func (o OfficeCLI) run(ctx context.Context, root string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, o.path, args...)
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(), "OFFICECLI_SKIP_UPDATE=1", "OFFICECLI_RESIDENT_FLUSH=each")
	return cmd.CombinedOutput()
}
