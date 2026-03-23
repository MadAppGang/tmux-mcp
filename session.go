package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// headlessSession is an entry in the session registry.
type headlessSession struct {
	shortID  string
	name     string
	paneID   string // "headless:%N" format
	command  string
	created  time.Time
}

// sessionRegistry maps short human-friendly IDs to headless pane IDs.
type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*headlessSession
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{
		sessions: make(map[string]*headlessSession),
	}
}

func (r *sessionRegistry) add(s *headlessSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[s.shortID] = s
}

func (r *sessionRegistry) get(shortID string) (*headlessSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[shortID]
	return s, ok
}

func (r *sessionRegistry) remove(shortID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, shortID)
}

func (r *sessionRegistry) list() []*headlessSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*headlessSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// generateShortID returns the first 8 hex characters of a new UUID.
func generateShortID() string {
	id := uuid.New().String()
	// UUID format: xxxxxxxx-xxxx-... strip dashes and take 8 chars
	clean := ""
	for _, c := range id {
		if c != '-' {
			clean += string(c)
		}
		if len(clean) == 8 {
			break
		}
	}
	return clean
}

// RunResult is the result of the one-shot `run` tool.
type RunResult struct {
	SessionID string `json:"sessionId"`
	Output    string `json:"output"`
	ExitCode  int    `json:"exitCode"`
}

// Run executes a command in a new headless session, captures output and exit
// code, then destroys the session. It is a one-shot convenience wrapper.
func (t *tmuxClient) Run(ctx context.Context, command string) (*RunResult, error) {
	id := generateShortID()
	sessionName := "_run_" + id

	created, err := t.CreateHeadlessSession(ctx, sessionName, "")
	if err != nil {
		return nil, fmt.Errorf("create headless session: %w", err)
	}
	paneID := created.PaneID

	// Defer cleanup — kill the session regardless of outcome.
	defer func() {
		_ = t.KillSession(context.Background(), created.SessionID)
	}()

	result, err := t.ExecuteCommand(ctx, paneID, command)
	if err != nil {
		return nil, fmt.Errorf("execute command: %w", err)
	}

	return &RunResult{
		SessionID: id,
		Output:    result.Output,
		ExitCode:  result.ExitCode,
	}, nil
}

// SessionStartResult is returned by session-start.
type SessionStartResult struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
}

// SessionStart creates a long-lived headless session. If command is non-empty
// it is sent to the shell via send-keys (not execute-command, so the session
// stays open after the command runs). Returns the short session ID.
func (t *tmuxClient) SessionStart(ctx context.Context, registry *sessionRegistry, command, name string) (*SessionStartResult, error) {
	id := generateShortID()
	tmuxName := "_session_" + id
	if name == "" {
		name = id
	}

	created, err := t.CreateHeadlessSession(ctx, tmuxName, "")
	if err != nil {
		return nil, fmt.Errorf("create headless session: %w", err)
	}

	if command != "" {
		if err := t.SendKeys(ctx, created.PaneID, command, true, true); err != nil {
			// Best-effort cleanup.
			_ = t.KillSession(context.Background(), created.SessionID)
			return nil, fmt.Errorf("send command: %w", err)
		}
	}

	registry.add(&headlessSession{
		shortID: id,
		name:    name,
		paneID:  created.PaneID,
		command: command,
		created: time.Now(),
	})

	return &SessionStartResult{SessionID: id, Name: name}, nil
}

// SessionSend sends input to a registered headless session.
func (t *tmuxClient) SessionSend(ctx context.Context, registry *sessionRegistry, shortID, input string, enter bool) error {
	sess, ok := registry.get(shortID)
	if !ok {
		return fmt.Errorf("session %q not found", shortID)
	}
	return t.SendKeys(ctx, sess.paneID, input, true, enter)
}

// SessionReadResult is returned by session-read.
type SessionReadResult struct {
	SessionID string     `json:"sessionId"`
	Output    string     `json:"output"`
	PaneState *PaneState `json:"paneState,omitempty"`
}

// SessionRead captures output from a registered headless session.
func (t *tmuxClient) SessionRead(ctx context.Context, registry *sessionRegistry, shortID string, lines int) (*SessionReadResult, error) {
	sess, ok := registry.get(shortID)
	if !ok {
		return nil, fmt.Errorf("session %q not found", shortID)
	}
	output, err := t.CapturePane(ctx, sess.paneID, lines, false)
	if err != nil {
		return nil, fmt.Errorf("capture pane: %w", err)
	}
	paneState, _ := t.GetPaneState(ctx, sess.paneID)
	return &SessionReadResult{
		SessionID: shortID,
		Output:    output,
		PaneState: paneState,
	}, nil
}

// SessionCloseResult is returned by session-close.
type SessionCloseResult struct {
	SessionID string `json:"sessionId"`
	Closed    bool   `json:"closed"`
}

// SessionClose kills the headless session and removes it from the registry.
func (t *tmuxClient) SessionClose(ctx context.Context, registry *sessionRegistry, shortID string) (*SessionCloseResult, error) {
	sess, ok := registry.get(shortID)
	if !ok {
		return nil, fmt.Errorf("session %q not found", shortID)
	}
	// Derive the tmux session ID from the pane ID by querying tmux.
	// The pane is "headless:%N"; we need the session that owns it.
	// Easiest: kill-session targeting the session name "_session_<id>".
	tmuxName := "_session_" + shortID
	// Kill via the headless socket by constructing a headless-prefixed session ID.
	// Since CreateHeadlessSession prefixed the session ID we don't have it stored,
	// but we can use the session name directly on the headless socket.
	_, err := t.runWithSocket(ctx, headlessSocket, "kill-session", "-t", tmuxName)
	if err != nil {
		// Try to kill by pane instead (fallback if session name lookup fails).
		_ = t.KillPane(ctx, sess.paneID)
	}
	registry.remove(shortID)
	return &SessionCloseResult{SessionID: shortID, Closed: true}, nil
}

// SessionInfo is one entry in the session-list result.
type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Command   string `json:"command"`
	Alive     bool   `json:"alive"`
}

// SessionList returns info about all registered headless sessions.
func (t *tmuxClient) SessionList(ctx context.Context, registry *sessionRegistry) ([]SessionInfo, error) {
	entries := registry.list()
	out := make([]SessionInfo, 0, len(entries))
	for _, e := range entries {
		alive := true
		if ps, err := t.GetPaneState(ctx, e.paneID); err == nil {
			alive = ps.IsAlive
		}
		out = append(out, SessionInfo{
			SessionID: e.shortID,
			Name:      e.name,
			Command:   e.command,
			Alive:     alive,
		})
	}
	return out, nil
}
