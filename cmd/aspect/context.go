package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/eideroliveira/aspect/internal/importer"
	"github.com/eideroliveira/aspect/internal/lang"
	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/speccontext"
)

// runContext prints what the spec says about the modules that own the
// given files or directories (or the modules named): the text `aspect
// review` puts in a reviewer's prompt, for a person or a coding assistant
// about to change that code. It is how a project keeps its package rules in
// the spec and still has them at hand when editing: a coding assistant's
// CLAUDE.md can say "run aspect context before changing a package" instead
// of carrying them.
func runContext(path string, args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	repo := fs.String("repo", "", "the repository the paths are in (default: the spec's repository)")
	budget := fs.Int("budget", 0, "the most tokens to print (0: no limit)")
	system := fs.Bool("system", true, "include the system's constraints")
	briefs := fs.Bool("briefs", true, "include the modules' briefs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("context: name at least one file, directory or module")
	}
	s, _, err := loadValidated(path)
	if err != nil {
		return err
	}
	root := *repo
	if root == "" {
		root = s.Root
	}
	names, files, unowned := modulesFor(s, root, fs.Args())
	for _, a := range unowned {
		fmt.Fprintf(os.Stderr, "aspect: no module of the spec owns %s\n", a)
	}
	if len(names) == 0 {
		return exitCode{code: 1, msg: "nothing to show"}
	}
	fmt.Print(speccontext.Render(s, names, speccontext.Options{Budget: *budget, OmitSystem: !*system, OmitBriefs: !*briefs, Files: files}))
	return nil
}

// modulesFor resolves each argument to the module that owns it, in order,
// without repeats: a path (absolute, or relative to the working directory)
// that exists is looked up by ownership (a directory as the files directly
// in it), anything else as a module name. A module one of whose topics
// covers a path is that path's too, as in a review's plan. files are the
// paths, relative to the repository (a directory's files directly in it),
// whose topics are shown; nil, which shows every topic, when an argument
// named a module.
func modulesFor(s *spec.Spec, root string, args []string) (names, files, unowned []string) {
	rootAbs, _ := filepath.Abs(root)
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	byName := false
	addCovering := func(file string) {
		files = append(files, file)
		for _, m := range s.AllModules() {
			for j := range m.Topics {
				if m.Topics[j].Covers(file) {
					add(m.Name)
					break
				}
			}
		}
	}
	for _, a := range args {
		info, statErr := os.Stat(a)
		if statErr != nil {
			if s.Module(a) != nil {
				add(a)
				byName = true
			} else {
				unowned = append(unowned, a)
			}
			continue
		}
		abs, err := filepath.Abs(a)
		if err != nil {
			unowned = append(unowned, a)
			continue
		}
		if !spec.Inside(rootAbs, abs) {
			unowned = append(unowned, a+" (outside the repository "+rootAbs+")")
			continue
		}
		rel, _ := filepath.Rel(rootAbs, abs)
		file := filepath.ToSlash(rel)
		var m *spec.Module
		before := len(names)
		if info.IsDir() {
			if file == "." {
				file = ""
			}
			_, m = lang.ModuleOfDir(s, file)
			if m != nil {
				add(m.Name)
			}
			entries, _ := os.ReadDir(abs)
			for _, e := range entries {
				if !e.IsDir() {
					addCovering(path.Join(file, e.Name()))
				}
			}
		} else {
			if _, m = lang.ModuleOf(s, file); m != nil {
				add(m.Name)
			}
			addCovering(file)
		}
		if m == nil && len(names) == before {
			unowned = append(unowned, a)
		}
	}
	if byName {
		files = nil
	} else if files == nil {
		files = []string{}
	}
	return names, files, unowned
}

// runBriefs points modules' briefs at their packages' own ASPECT.md, read
// in place, instead of the copies `aspect import` made beside the spec: one
// document that people, `aspect context` and the agents all read, rather
// than two that drift apart.
func runBriefs(path string, args []string) error {
	fs := flag.NewFlagSet("briefs", flag.ContinueOnError)
	write := fs.Bool("w", false, "write the new brief paths into the spec files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := spec.Load(path)
	if err != nil {
		return err
	}
	moves, notes := importer.InPlaceBriefs(s)
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "aspect:", n)
	}
	if len(moves) == 0 {
		fmt.Printf("no brief to move: no placed module has an %s it does not already read (or a brief of its own that it keeps)\n", importer.PackageBrief)
		return nil
	}
	set := map[string]string{}
	for _, mv := range moves {
		from := mv.From
		if from == "" {
			from = "(none)"
		}
		fmt.Printf("%-40s %s -> %s\n", mv.Module, from, mv.To)
		if mv.Drifted {
			fmt.Printf("%-40s   the copy no longer matches %s; the package's file wins\n", "", importer.PackageBrief)
		}
		if len(mv.Dropped) > 0 {
			fmt.Printf("%-40s   the copy also held %s, which the new brief does not\n", "", strings.Join(mv.Dropped, ", "))
		}
		set[mv.Module] = mv.To
	}
	if !*write {
		fmt.Printf("%d brief(s) would move; rerun with -w to write them\n", len(moves))
		return nil
	}
	files, err := spec.SetModuleBriefs(path, set)
	if err != nil {
		return fmt.Errorf("%w (files already rewritten: %s)", err, listOrNone(files))
	}
	after, _, err := loadValidated(path)
	if err != nil {
		return fmt.Errorf("after writing briefs into %s: %w", listOrNone(files), err)
	}
	fmt.Printf("moved %d brief(s) in %d file(s)\n", len(moves), len(files))
	used := map[string]bool{}
	for _, b := range after.Briefs() {
		if b.Path != "" {
			used[filepath.Clean(b.Path)] = true
		}
	}
	for _, mv := range moves {
		if mv.From != "" && !used[filepath.Clean(mv.From)] {
			fmt.Printf("no longer referenced: %s (delete it once you have moved anything it holds that is still true)\n", filepath.Join(s.Dir, filepath.FromSlash(mv.From)))
		}
	}
	return nil
}
