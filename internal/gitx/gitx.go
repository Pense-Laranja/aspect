// Package gitx runs git for the commands that read a repository: the review
// gates' plan and the review engine. It is the only place that turns git
// output into gate input, so `aspect gate plan` and `aspect review` plan a
// change the same way.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/eideroliveira/aspect/internal/gate"
)

// Run runs git in dir ("" is the working directory) and returns its output
// without the trailing newlines.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// Changes lists the files changed between base and head with their line
// counts. Renames are reported as a delete and an add so both paths are
// matched against the gates.
func Changes(ctx context.Context, dir, base, head string) ([]gate.Change, error) {
	out, err := Run(ctx, dir, "diff", "--numstat", "--no-renames", "-z", base, head)
	if err != nil {
		return nil, err
	}
	var changes []gate.Change
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected git diff --numstat record %q", rec)
		}
		added, _ := strconv.Atoi(parts[0]) // "-" for binary files counts as 0
		deleted, _ := strconv.Atoi(parts[1])
		changes = append(changes, gate.Change{Path: parts[2], Added: added, Deleted: deleted})
	}
	return changes, nil
}

// Plan resolves head and its merge base with base in the repository at dir
// and builds the gate plan for the change between them.
func Plan(ctx context.Context, dir string, c *gate.Config, base, head string) (*gate.Plan, error) {
	headSHA, err := Run(ctx, dir, "rev-parse", "--verify", head+"^{commit}")
	if err != nil {
		return nil, err
	}
	baseSHA, err := Run(ctx, dir, "merge-base", base, headSHA)
	if err != nil {
		return nil, err
	}
	changes, err := Changes(ctx, dir, baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	spec := c.ResolveSpec(func(path string) bool {
		_, err := Run(ctx, dir, "cat-file", "-e", headSHA+":"+path)
		return err == nil
	})
	return gate.BuildPlan(c, baseSHA, headSHA, changes, spec), nil
}
