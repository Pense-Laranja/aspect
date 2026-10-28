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
	topicFlag := fs.String("topic", "", "comma-separated topic names to show (or pass <module>:<topic> as an argument)")
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
	names, files, topics, byName, unowned := modulesForWithTopics(s, root, fs.Args())
	if *topicFlag != "" {
		for _, t := range strings.Split(*topicFlag, ",") {
			if t = strings.TrimSpace(t); t != "" {
				topics = append(topics, t)
			}
		}
		byName = false
	}
	for _, a := range unowned {
		fmt.Fprintf(os.Stderr, "aspect: no module of the spec owns %s\n", a)
	}
	if len(names) == 0 {
		return exitCode{code: 1, msg: "nothing to show"}
	}
	fmt.Print(speccontext.Render(s, names, speccontext.Options{Budget: *budget, OmitSystem: !*system, OmitBriefs: !*briefs, Files: files, Topics: topics, AllTopics: byName}))
	return nil
}

// modulesFor resolves each argument to the module that owns it, in order,
// without repeats: a path (absolute, or relative to the working directory)
// that exists is looked up by ownership (a directory as the files directly
// in it), anything else as a module name. A module one of whose topics a
// path touches (spec.Spec.TopicsTouching: its patterns, its brief) is that
// path's too, as in a review's plan. files are the paths, relative to the
// repository (a directory's files directly in it), that select the topics
// shown; byName says an argument named a module, whose topics are then all
// shown.
func modulesFor(s *spec.Spec, root string, args []string) (names, files []string, byName bool, unowned []string) {
	names, files, _, byName, unowned = modulesForWithTopics(s, root, args)
	return names, files, byName, unowned
}

// modulesForWithTopics is modulesFor plus `<module>:<topic>` (or `<topic>`
// when a single module in the spec defines that topic) arguments, which add
// the module and select that topic without setting byName.
func modulesForWithTopics(s *spec.Spec, root string, args []string) (names, files, topics []string, byName bool, unowned []string) {
	rootAbs, _ := filepath.Abs(root)
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	matchTopicArg := func(arg string) bool {
		if modName, topicName, ok := strings.Cut(arg, ":"); ok && modName != "" && topicName != "" {
			m := s.Module(modName)
			if m == nil {
				return false
			}
			for j := range m.Topics {
				if m.Topics[j].Name == topicName {
					add(m.Name)
					topics = append(topics, m.Name+":"+topicName)
					return true
				}
			}
			return false
		}
		var matched []*spec.Module
		for _, m := range s.AllModules() {
			for j := range m.Topics {
				if m.Topics[j].Name == arg {
					matched = append(matched, m)
					break
				}
			}
		}
		if len(matched) == 1 {
			add(matched[0].Name)
			topics = append(topics, matched[0].Name+":"+arg)
			return true
		}
		return false
	}
	// addTouching adds the modules whose topics file touches, and says
	// whether there was one, whether or not it was added before.
	addTouching := func(file string) bool {
		files = append(files, file)
		touched := false
		for _, m := range s.AllModules() {
			if t, _ := s.TopicsTouching(m, []string{file}); len(t) > 0 {
				add(m.Name)
				touched = true
			}
		}
		return touched
	}
	for _, a := range args {
		info, statErr := os.Stat(a)
		if statErr != nil {
			if s.Module(a) != nil {
				add(a)
				byName = true
			} else if !matchTopicArg(a) {
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
		touched := false
		if info.IsDir() {
			if file == "." {
				file = ""
			}
			_, m = lang.ModuleOfDir(s, file)
			if m != nil {
				add(m.Name)
			}
			entries, err := os.ReadDir(abs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "aspect: cannot list %s, so the topics of its files are not shown: %v\n", a, err)
			}
			for _, e := range entries {
				if !e.IsDir() && addTouching(path.Join(file, e.Name())) {
					touched = true
				}
			}
		} else {
			if _, m = lang.ModuleOf(s, file); m != nil {
				add(m.Name)
			}
			touched = addTouching(file)
		}
		if m == nil && !touched {
			unowned = append(unowned, a)
		}
	}
	return names, files, topics, byName, unowned
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
