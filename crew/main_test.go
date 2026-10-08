package main

import "testing"

func TestParseSessions(t *testing.T) {
	const (
		needsValue = "crew: --crew needs a value"
	)
	tests := []struct {
		name                string
		args                []string
		branch, crew, wants string
	}{
		{"branch only", []string{"b"}, "b", "", ""},
		{"crew filter", []string{"b", "--crew", "c1"}, "b", "c1", ""},
		{"later crew wins", []string{"b", "--crew", "c1", "--crew", "c2"}, "b", "c2", ""},
		{"no args", nil, "", "", sessionsUsage},
		{"empty branch", []string{""}, "", "", sessionsUsage},
		{"crew taken as branch", []string{"--crew", "x"}, "", "", sessionsUsage},
		{"missing value", []string{"b", "--crew"}, "", "", needsValue},
		{"empty value", []string{"b", "--crew", ""}, "", "", needsValue},
		{"second positional", []string{"b", "x"}, "", "", sessionsUsage},
		{"empty branch with crew", []string{"", "--crew", "c1"}, "", "", sessionsUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch, crew, msg := parseSessions(tt.args)
			if branch != tt.branch || crew != tt.crew || msg != tt.wants {
				t.Errorf("parseSessions(%q) = %q, %q, %q; want %q, %q, %q", tt.args, branch, crew, msg, tt.branch, tt.crew, tt.wants)
			}
		})
	}
}
