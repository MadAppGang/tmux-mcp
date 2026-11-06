package main

import "testing"

// TestDetectBackendChecksALoneClaim covers a single claim against what the
// ancestry probe can say about it. A claim the probe disproves — the claimed
// pane's shell exists and is not an ancestor of this process — is a leaked
// variable, so there is no window; a claim the probe cannot check is believed,
// because a server started outside its pane's process tree has no other way to
// be found.
//
// REGRESSION: a lone leaked TMUX_PANE or HERDR_PANE_ID was believed even when the probe proved it was not this process's pane
func TestDetectBackendChecksALoneClaim(t *testing.T) {
	tmuxEnv := map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TMUX_PANE": "%3"}
	herdrEnv := map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "w1:p2"}
	disproved := func(string) (int, bool) { return 0, false }
	unchecked := func(string) (int, bool) { return probeUnavailable, false }

	cases := []struct {
		name  string
		env   map[string]string
		probe func(string) (int, bool)
		want  detectedBackend
	}{
		{"tmux claim the probe disproves", tmuxEnv, disproved, detectedTmuxNoWindow},
		{"herdr claim the probe disproves", herdrEnv, disproved, detectedTmuxNoWindow},
		{"tmux claim the probe cannot check", tmuxEnv, unchecked, detectedTmux},
		{"herdr claim the probe cannot check", herdrEnv, unchecked, detectedHerdr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectBackend(tc.env, tc.probe, ""); got != tc.want {
				t.Errorf("detectBackend = %v, want %v", got, tc.want)
			}
		})
	}
}
