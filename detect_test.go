package main

import (
	"testing"
)

// This file is written against an API that does not exist yet:
//
//	type detectedBackend int
//
//	const (
//		detectedTmuxNoWindow detectedBackend = iota // no verified multiplexer: tmux, isolated slots only
//		detectedTmux
//		detectedHerdr
//	)
//
//	// detectBackend picks the multiplexer from the -backend flag value
//	// ("auto", "" for auto, "tmux" or "herdr"), the process environment, and an
//	// ancestry probe. The probe is asked only about a candidate whose
//	// environment is present (tmux: TMUX_PANE; herdr: HERDR_ENV=1 and
//	// HERDR_PANE_ID), and answers how many ppid hops separate this process
//	// from that pane's shell, or ok=false when the shell is not an ancestor.
//	func detectBackend(
//		env map[string]string,
//		ancestorDepth func(kind string) (depth int, ok bool),
//		flag string,
//	) detectedBackend
//
// The probe's kind argument is "tmux" or "herdr".

// REGRESSION: leaked multiplexer variables from an outer or inner multiplexer picked the wrong backend
func TestDetectBackend(t *testing.T) {
	tmuxEnv := map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TMUX_PANE": "%3"}
	herdrEnv := map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "w1:p2", "HERDR_SOCKET_PATH": "/tmp/h.sock"}
	both := map[string]string{}
	for k, v := range tmuxEnv {
		both[k] = v
	}
	for k, v := range herdrEnv {
		both[k] = v
	}

	// ancestry builds a probe from a table of kind → depth; a kind not in the
	// table is not an ancestor.
	ancestry := func(depths map[string]int) func(string) (int, bool) {
		return func(kind string) (int, bool) {
			d, ok := depths[kind]
			return d, ok
		}
	}

	cases := []struct {
		name   string
		env    map[string]string
		depths map[string]int
		flag   string
		want   detectedBackend
	}{
		{
			name:   "TMUX_PANE leaked into a herdr pane started from tmux",
			env:    both,
			depths: map[string]int{"herdr": 2}, // tmux's pane shell is not our ancestor
			want:   detectedHerdr,
		},
		{
			name:   "HERDR variables leaked into tmux started inside herdr",
			env:    both,
			depths: map[string]int{"tmux": 2},
			want:   detectedTmux,
		},
		{
			name:   "both genuine: herdr inside tmux, herdr is nearer",
			env:    both,
			depths: map[string]int{"herdr": 2, "tmux": 5},
			want:   detectedHerdr,
		},
		{
			name:   "both genuine: tmux inside herdr, tmux is nearer",
			env:    both,
			depths: map[string]int{"herdr": 6, "tmux": 3},
			want:   detectedTmux,
		},
		{
			name:   "only tmux",
			env:    tmuxEnv,
			depths: map[string]int{"tmux": 2},
			want:   detectedTmux,
		},
		{
			name:   "only herdr",
			env:    herdrEnv,
			depths: map[string]int{"herdr": 2},
			want:   detectedHerdr,
		},
		{
			name:   "neither variable set",
			env:    map[string]string{},
			depths: map[string]int{},
			want:   detectedTmuxNoWindow,
		},
		{
			name:   "both set and neither verifies",
			env:    both,
			depths: map[string]int{},
			want:   detectedTmuxNoWindow,
		},
		{
			name:   "HERDR_ENV without HERDR_PANE_ID is not a herdr pane",
			env:    map[string]string{"HERDR_ENV": "1"},
			depths: map[string]int{"herdr": 1},
			want:   detectedTmuxNoWindow,
		},
		{
			name:   "auto spelled out behaves like the default",
			env:    herdrEnv,
			depths: map[string]int{"herdr": 2},
			flag:   "auto",
			want:   detectedHerdr,
		},
		{
			name:   "explicit -backend tmux overrides a verified herdr",
			env:    both,
			depths: map[string]int{"herdr": 2},
			flag:   "tmux",
			want:   detectedTmux,
		},
		{
			name:   "explicit -backend herdr overrides a verified tmux",
			env:    both,
			depths: map[string]int{"tmux": 2},
			flag:   "herdr",
			want:   detectedHerdr,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectBackend(tc.env, ancestry(tc.depths), tc.flag); got != tc.want {
				t.Errorf("detectBackend = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDetectBackendAsksAncestryOnlyForPresentCandidates pins that the probe is
// a verification of an environment claim, never a search: a kind with no
// variables is not asked about, so a stray multiplexer up the process tree with
// no claim in the environment cannot be chosen.
//
// REGRESSION: leaked multiplexer variables from an outer or inner multiplexer picked the wrong backend
func TestDetectBackendAsksAncestryOnlyForPresentCandidates(t *testing.T) {
	asked := map[string]int{}
	probe := func(kind string) (int, bool) {
		asked[kind]++
		return 1, true
	}
	if got := detectBackend(map[string]string{"TMUX_PANE": "%1", "TMUX": "/tmp/x,1,0"}, probe, ""); got != detectedTmux {
		t.Errorf("detectBackend = %v, want tmux", got)
	}
	if asked["herdr"] != 0 {
		t.Errorf("the ancestry probe was asked about herdr %d times with no HERDR_* in the environment", asked["herdr"])
	}
}
