package main

import (
	"slices"
	"testing"
)

func TestParseReadArgs(t *testing.T) {
	cases := []struct {
		sub      string
		args     []string
		runs     []string
		severity string
		err      bool
	}{
		{"findings", []string{"run-1", "-severity", "high"}, []string{"run-1"}, "high", false},
		{"findings", []string{"-severity", "high", "run-1"}, []string{"run-1"}, "high", false},
		{"findings", nil, nil, "", false},
		{"findings", []string{"a", "b"}, nil, "", true},
		{"findings", []string{"-severity", "hgih"}, nil, "", true},
		{"status", []string{"run-1", "-repo", "/r"}, []string{"run-1"}, "", false},
		{"compare", []string{"a", "b"}, []string{"a", "b"}, "", false},
		{"compare", []string{"only-one"}, nil, "", true},
		{"runs", []string{"-limit", "3"}, nil, "", false},
		{"runs", []string{"stray"}, nil, "", true},
	}
	for _, c := range cases {
		a, err := parseReadArgs(c.sub, c.args, ".")
		if (err != nil) != c.err {
			t.Errorf("%s %v: err = %v, want error %v", c.sub, c.args, err, c.err)
			continue
		}
		if c.err {
			continue
		}
		if !slices.Equal(a.runs, c.runs) || a.severity != c.severity {
			t.Errorf("%s %v: runs %v severity %q", c.sub, c.args, a.runs, a.severity)
		}
	}
	if a, _ := parseReadArgs("status", []string{"-repo", "/r"}, "."); a.repo != "/r" {
		t.Errorf("repo = %q", a.repo)
	}
	if a, _ := parseReadArgs("runs", []string{"-limit", "3"}, "."); a.limit != 3 {
		t.Errorf("limit = %d", a.limit)
	}
}
