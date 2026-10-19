// Package workspace owns the directory where generated code lives and knows
// how to compile and test it. Agents never touch the filesystem or run tools
// directly; they propose files, and the workspace applies them. That boundary
// is what makes the pipeline auditable: every file on disk came through here.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// File is a path relative to the workspace root plus its full content.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Toolchain is what the workspace needs to know about the target language.
// The lang package builds one from a profile and the spec.
type Toolchain struct {
	// SourceExt selects the files ReadTree returns.
	SourceExt string
	// SkipDir, given a directory below the root, reports whether ReadTree
	// leaves it out (directories the toolchain itself ignores). May be nil.
	SkipDir func(dir string) bool
	// Protected are manifest basenames agents may not write (go.mod, Package.swift).
	Protected []string
	// Init writes the manifest once when the workspace is created.
	Init func(root string) error
	// Sync updates the manifest for the modules present so far.
	Sync func(root string, modules []string) error
	// Steps are the commands, run in root, that build and test one module.
	Steps func(module string) [][]string
	// Env is added to the process environment of every step.
	Env []string
}

// Workspace is one generated project on disk.
type Workspace struct {
	Root string
	// TestTimeout bounds one build-and-test run. Generated code can loop
	// forever; the pipeline must not.
	TestTimeout time.Duration
	tc          Toolchain
	// tools serialises manifest syncs and toolchain runs: modules may be
	// generated in parallel, but `go mod tidy` or `swift build` running
	// twice at once in one tree corrupt each other.
	tools sync.Mutex
}

// Result of a build-and-test run.
type Result struct {
	OK       bool          `json:"ok"`
	Output   string        `json:"output"`
	Duration time.Duration `json:"duration"`
}

// New creates the root directory and the manifest.
func New(root string, tc Toolchain) (*Workspace, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if tc.Init != nil {
		if err := tc.Init(root); err != nil {
			return nil, err
		}
	}
	return &Workspace{Root: root, TestTimeout: 3 * time.Minute, tc: tc}, nil
}

// Sync regenerates the manifest for the given modules (no-op for languages
// whose manifest does not list modules).
func (w *Workspace) Sync(modules []string) error {
	if w.tc.Sync == nil {
		return nil
	}
	w.tools.Lock()
	defer w.tools.Unlock()
	return w.tc.Sync(w.Root, modules)
}

// WriteFiles applies a set of files. Paths must stay inside the root; an agent
// proposing "../../etc/passwd" is rejected rather than trusted.
func (w *Workspace) WriteFiles(files []File) error {
	for _, f := range files {
		abs, err := w.safePath(f.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// DeleteFiles removes files previously written (used when a repair attempt
// renames a file and the old one would otherwise keep failing the build).
func (w *Workspace) DeleteFiles(paths []string) error {
	for _, p := range paths {
		abs, err := w.safePath(p)
		if err != nil {
			return err
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (w *Workspace) safePath(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", fmt.Errorf("workspace: invalid path %q", p)
	}
	clean := filepath.Clean(p)
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("workspace: path %q escapes the workspace", p)
	}
	base := filepath.Base(clean)
	for _, protected := range w.tc.Protected {
		if base == protected {
			return "", fmt.Errorf("workspace: agents may not rewrite %s", clean)
		}
	}
	return filepath.Join(w.Root, clean), nil
}

// ReadTree returns every source file under the root, sorted by path, so agents
// can be shown the current state of the project. A root that is a symlink
// (an -out directory linking to a checkout) is resolved first, because
// WalkDir does not descend into a symlinked root; symlinks below the root
// are not followed.
func (w *Workspace) ReadTree() ([]File, error) {
	root := w.Root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	var files []File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Hidden directories hold build products (.build), history and
			// tool state (.git, .claude), never the project's code.
			if path != root && (strings.HasPrefix(d.Name(), ".") || (w.tc.SkipDir != nil && w.tc.SkipDir(path))) {
				return filepath.SkipDir
			}
			return nil
		}
		if w.tc.SourceExt != "" && !strings.HasSuffix(path, w.tc.SourceExt) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, File{Path: filepath.ToSlash(rel), Content: string(b)})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

// Test runs the toolchain's steps for one module and returns the combined
// output. A non-nil error means the tooling itself could not run; a failing
// build or test is reported through Result.OK.
func (w *Workspace) Test(ctx context.Context, module string) (Result, error) {
	w.tools.Lock()
	defer w.tools.Unlock()
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, w.TestTimeout)
	defer cancel()

	var out bytes.Buffer
	for _, args := range w.tc.Steps(module) {
		fmt.Fprintf(&out, "$ %s\n", strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = w.Root
		cmd.Env = append(os.Environ(), w.tc.Env...)
		b, err := cmd.CombinedOutput()
		out.Write(b)
		if ctx.Err() != nil {
			fmt.Fprintf(&out, "\n[aspect] aborted: %v\n", ctx.Err())
			return Result{OK: false, Output: out.String(), Duration: time.Since(start)}, nil
		}
		if err != nil {
			var exitErr *exec.ExitError
			if !asExitError(err, &exitErr) {
				return Result{}, fmt.Errorf("run %v: %w", args, err)
			}
			fmt.Fprintf(&out, "\n[aspect] %s exited with %v\n", strings.Join(args[:2], " "), exitErr)
			return Result{OK: false, Output: out.String(), Duration: time.Since(start)}, nil
		}
	}
	return Result{OK: true, Output: out.String(), Duration: time.Since(start)}, nil
}
