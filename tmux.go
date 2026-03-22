package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Session represents a tmux session.
type Session struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Attached bool   `json:"attached"`
}

// Window represents a tmux window.
type Window struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Panes  int    `json:"panes"`
}

// Pane represents a tmux pane with extended info.
type Pane struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Active         bool   `json:"active"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	CurrentCommand string `json:"currentCommand"`
	CurrentPath    string `json:"currentPath"`
}

// CommandResult holds the output and exit code of a completed command.
type CommandResult struct {
	PaneID   string `json:"paneId"`
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
}

// CreatedSession holds the IDs returned when a new session is created.
type CreatedSession struct {
	SessionID   string `json:"sessionId"`
	SessionName string `json:"sessionName"`
	WindowID    string `json:"windowId"`
	PaneID      string `json:"paneId"`
}

// CreatedWindow holds the IDs returned when a new window is created.
type CreatedWindow struct {
	WindowID   string `json:"windowId"`
	WindowName string `json:"windowName"`
	PaneID     string `json:"paneId"`
}

// CreatedPane holds the IDs returned when a new pane is created.
type CreatedPane struct {
	PaneID   string `json:"paneId"`
	WindowID string `json:"windowId"`
}

// tmuxClient wraps tmux CLI interactions.
type tmuxClient struct {
	shellType string
}

func newTmuxClient(shellType string) *tmuxClient {
	return &tmuxClient{shellType: shellType}
}

// run executes a tmux command and returns its combined output.
func (t *tmuxClient) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("tmux %s: %w", args[0], err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// ListSessions returns all active tmux sessions.
func (t *tmuxClient) ListSessions(ctx context.Context) ([]Session, error) {
	out, err := t.run(ctx, "list-sessions",
		"-F", "#{session_id}\t#{session_name}\t#{session_windows}\t#{session_attached}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") ||
			strings.Contains(err.Error(), "no sessions") {
			return []Session{}, nil
		}
		return nil, err
	}
	if out == "" {
		return []Session{}, nil
	}

	var sessions []Session
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		windows, _ := strconv.Atoi(parts[2])
		sessions = append(sessions, Session{
			ID:       parts[0],
			Name:     parts[1],
			Windows:  windows,
			Attached: parts[3] != "0",
		})
	}
	return sessions, nil
}

// ListWindows returns windows in the given session.
func (t *tmuxClient) ListWindows(ctx context.Context, sessionID string) ([]Window, error) {
	out, err := t.run(ctx, "list-windows",
		"-t", sessionID,
		"-F", "#{window_id}\t#{window_name}\t#{window_active}\t#{window_panes}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []Window{}, nil
	}

	var windows []Window
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		panes, _ := strconv.Atoi(parts[3])
		windows = append(windows, Window{
			ID:     parts[0],
			Name:   parts[1],
			Active: parts[2] == "1",
			Panes:  panes,
		})
	}
	return windows, nil
}

// ListPanes returns panes in the given window with extended info.
func (t *tmuxClient) ListPanes(ctx context.Context, windowID string) ([]Pane, error) {
	out, err := t.run(ctx, "list-panes",
		"-t", windowID,
		"-F", "#{pane_id}\t#{pane_title}\t#{pane_active}\t#{pane_width}\t#{pane_height}\t#{pane_current_command}\t#{pane_current_path}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []Pane{}, nil
	}

	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 7 {
			continue
		}
		width, _ := strconv.Atoi(parts[3])
		height, _ := strconv.Atoi(parts[4])
		panes = append(panes, Pane{
			ID:             parts[0],
			Title:          parts[1],
			Active:         parts[2] == "1",
			Width:          width,
			Height:         height,
			CurrentCommand: parts[5],
			CurrentPath:    parts[6],
		})
	}
	return panes, nil
}

// CapturePane returns the terminal content of a pane.
// lines <= 0 uses the pane's default history size.
// colors controls whether ANSI escape sequences are preserved.
func (t *tmuxClient) CapturePane(ctx context.Context, paneID string, lines int, colors bool) (string, error) {
	args := []string{"capture-pane", "-t", paneID, "-p"}
	if lines > 0 {
		args = append(args, "-S", strconv.Itoa(-lines))
	}
	if colors {
		args = append(args, "-e")
	}
	return t.run(ctx, args...)
}

// CreateSession creates a new detached tmux session.
// Returns a CreatedSession with the session, window, and pane IDs.
func (t *tmuxClient) CreateSession(ctx context.Context, name string) (*CreatedSession, error) {
	args := []string{"new-session", "-d", "-P", "-F", "#{session_id}\t#{session_name}\t#{window_id}\t#{pane_id}"}
	if name != "" {
		args = append(args, "-s", name)
	}
	out, err := t.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	return &CreatedSession{
		SessionID:   parts[0],
		SessionName: parts[1],
		WindowID:    parts[2],
		PaneID:      parts[3],
	}, nil
}

// CreateWindow creates a new window in the given session.
// Returns a CreatedWindow with the window and pane IDs.
func (t *tmuxClient) CreateWindow(ctx context.Context, sessionID, name string) (*CreatedWindow, error) {
	args := []string{"new-window", "-t", sessionID, "-P", "-F", "#{window_id}\t#{window_name}\t#{pane_id}"}
	if name != "" {
		args = append(args, "-n", name)
	}
	out, err := t.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 3 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	return &CreatedWindow{
		WindowID:   parts[0],
		WindowName: parts[1],
		PaneID:     parts[2],
	}, nil
}

// SplitPane splits the given pane. direction is "horizontal" or "vertical".
// size is a percentage (0 means use tmux default of 50%).
// Returns a CreatedPane with the new pane ID and its parent window ID.
func (t *tmuxClient) SplitPane(ctx context.Context, paneID, direction string, size int) (*CreatedPane, error) {
	args := []string{"split-window", "-t", paneID, "-P", "-F", "#{pane_id}\t#{window_id}"}
	if direction == "horizontal" {
		args = append(args, "-h")
	}
	if size > 0 {
		args = append(args, "-p", strconv.Itoa(size))
	}
	out, err := t.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 2 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	return &CreatedPane{
		PaneID:   parts[0],
		WindowID: parts[1],
	}, nil
}

// SendKeys sends keystrokes to a pane.
// When literal is true, -l flag is used so tmux treats input as literal text
// rather than interpreting key names like "Enter" or "C-c".
// When enter is true, a real Enter keystroke is appended after the keys.
func (t *tmuxClient) SendKeys(ctx context.Context, paneID, keys string, literal, enter bool) error {
	args := []string{"send-keys", "-t", paneID}
	if literal {
		args = append(args, "-l")
	}
	args = append(args, keys)
	if _, err := t.run(ctx, args...); err != nil {
		return err
	}
	if enter {
		// Send Enter as a separate call so that the -l flag (if set above)
		// does not cause "Enter" to be treated as the five literal characters
		// E-n-t-e-r. This second call intentionally omits -l so tmux
		// interprets "Enter" as the Return key.
		if _, err := t.run(ctx, "send-keys", "-t", paneID, "Enter"); err != nil {
			return err
		}
	}
	return nil
}

// ExecuteCommand runs a shell command synchronously in the given pane.
// It wraps the command so output is tee'd to a temp file and the exit code is
// written to a separate file, then uses "tmux wait-for" to block until the
// command finishes. The context deadline controls the overall timeout.
func (t *tmuxClient) ExecuteCommand(ctx context.Context, paneID, command string) (*CommandResult, error) {
	id := uuid.New().String()
	outFile := fmt.Sprintf("/tmp/tmux-mcp-%s.out", id)
	exitFile := fmt.Sprintf("/tmp/tmux-mcp-%s.exit", id)
	waitChannel := "tmux-mcp-" + id

	defer os.Remove(outFile)
	defer os.Remove(exitFile)

	wrapped := t.wrapCommand(command, outFile, exitFile, waitChannel)

	// Send the wrapped command to the pane (literal=false so Enter sends a real Enter).
	if err := t.SendKeys(ctx, paneID, wrapped, false, true); err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}

	// Block until the command signals completion or the context is cancelled.
	if err := exec.CommandContext(ctx, "tmux", "wait-for", waitChannel).Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("command timed out: %w", ctx.Err())
		}
		return nil, fmt.Errorf("wait-for: %w", err)
	}

	outputBytes, err := os.ReadFile(outFile)
	if err != nil {
		return nil, fmt.Errorf("read output file: %w", err)
	}

	exitBytes, err := os.ReadFile(exitFile)
	if err != nil {
		return nil, fmt.Errorf("read exit file: %w", err)
	}

	exitCode, _ := strconv.Atoi(strings.TrimSpace(string(exitBytes)))

	return &CommandResult{
		PaneID:   paneID,
		Output:   strings.TrimRight(string(outputBytes), "\n"),
		ExitCode: exitCode,
	}, nil
}

// wrapCommand builds a shell command that:
//  1. Pipes stdout+stderr through tee into outFile (so it shows in the pane)
//  2. Captures the exit code into exitFile
//  3. Signals waitChannel via "tmux wait-for -S" when done
func (t *tmuxClient) wrapCommand(command, outFile, exitFile, waitChannel string) string {
	switch t.shellType {
	case "fish":
		// fish uses $pipestatus[1] (1-indexed) after a pipe, but pipestatus is
		// only set inside a pipeline. Capture exit status before the pipe instead.
		return fmt.Sprintf(
			`begin; %s; set _exit $status; end 2>&1 | tee %s; echo $_exit > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	case "zsh":
		return fmt.Sprintf(
			`{ %s } 2>&1 | tee %s; echo ${pipestatus[1]} > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	default: // bash
		return fmt.Sprintf(
			`{ %s; } 2>&1 | tee %s; echo ${PIPESTATUS[0]} > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	}
}

// ResizePaneAbsolute resizes a pane to exact dimensions in columns and rows.
func (t *tmuxClient) ResizePaneAbsolute(ctx context.Context, paneID string, width, height int) error {
	_, err := t.run(ctx, "resize-pane", "-t", paneID,
		"-x", strconv.Itoa(width),
		"-y", strconv.Itoa(height))
	return err
}

// ResizePaneRelative adjusts a pane size in the given direction.
// direction must be one of U, D, L, R.
func (t *tmuxClient) ResizePaneRelative(ctx context.Context, paneID, direction string, amount int) error {
	_, err := t.run(ctx, "resize-pane", "-t", paneID, "-"+strings.ToUpper(direction), strconv.Itoa(amount))
	return err
}

// RenameSession renames a tmux session.
func (t *tmuxClient) RenameSession(ctx context.Context, sessionID, newName string) error {
	_, err := t.run(ctx, "rename-session", "-t", sessionID, newName)
	return err
}

// KillSession kills a tmux session and all its windows.
func (t *tmuxClient) KillSession(ctx context.Context, sessionID string) error {
	_, err := t.run(ctx, "kill-session", "-t", sessionID)
	return err
}

// KillWindow kills a tmux window and all its panes.
func (t *tmuxClient) KillWindow(ctx context.Context, windowID string) error {
	_, err := t.run(ctx, "kill-window", "-t", windowID)
	return err
}

// KillPane kills a tmux pane.
func (t *tmuxClient) KillPane(ctx context.Context, paneID string) error {
	_, err := t.run(ctx, "kill-pane", "-t", paneID)
	return err
}

// DisplayMessage shows a transient message in the tmux status bar.
// durationMs controls how long (in milliseconds) the message is shown.
func (t *tmuxClient) DisplayMessage(ctx context.Context, message string, durationMs int) error {
	_, err := t.run(ctx, "display-message", "-d", strconv.Itoa(durationMs), message)
	return err
}
