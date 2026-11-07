package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func captureAssertStructuredText(t *testing.T, result callToolResult, tool string, slot int) string {
	t.Helper()
	if result.IsError {
		t.Fatalf("%s failed: %s", tool, result.text(t, tool))
	}
	body := result.text(t, tool)
	want := map[string]any{"slot": float64(slot), "content": body}
	if !reflect.DeepEqual(result.StructuredContent, want) {
		t.Errorf("%s structuredContent=%#v, want exactly %#v (text must match byte for byte)",
			tool, result.StructuredContent, want)
	}
	return body
}

func captureQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// empty content must be represented, not mistaken for an old server.
func TestSlotContentFields(t *testing.T) {
	for _, content := range []string{"", "plain\n", "\x1b[31mλ\tred\x1b[0m\n\n"} {
		t.Run(fmt.Sprintf("bytes=%d", len(content)), func(t *testing.T) {
			encoded, err := json.Marshal(slotContent{Slot: 3, Content: content})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"slot": float64(3), "content": content}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("slotContent JSON=%s, want required slot/content including the empty string", encoded)
			}
		})
	}
}

func TestCaptureStructuredShell(t *testing.T) {
	c, self := agentPaneFixture(t)
	c.callToolJSON(t, "execute-command", map[string]any{
		"slot": 3, "command": "printf '\\033[31mCAP_RED λ\\033[0m\\n\\nCAP_END\\n'",
		"timeoutSeconds": 12,
	}, &map[string]any{})
	waitForPaneIdle(t, slotPaneID(t, self, 3))

	tests := []struct {
		name       string
		args       map[string]any
		wantMarker bool
		wantColors bool
	}{
		{"defaults", map[string]any{"slot": 3}, true, false},
		{"plain", map[string]any{"slot": 3, "lines": 200, "colors": false}, true, false},
		{"colors", map[string]any{"slot": 3, "lines": 200, "colors": true}, true, true},
		{"short plain", map[string]any{"slot": 3, "lines": 1, "colors": false}, false, false},
		{"short colors", map[string]any{"slot": 3, "lines": 5, "colors": true}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := captureAssertStructuredText(t, callTool(t, c, "capture-pane", tc.args), "capture-pane", 3)
			if tc.wantMarker && (!strings.Contains(body, "CAP_RED λ") || !strings.Contains(body, "CAP_END")) {
				t.Errorf("plain-shell capture lost the fixture output: %q", body)
			}
			if tc.wantColors && !strings.Contains(body, "\x1b[") {
				t.Errorf("colors:true capture stripped styled output: %q", body)
			}
			if colors, stated := tc.args["colors"].(bool); stated && !colors && strings.Contains(body, "\x1b[") {
				t.Errorf("colors:false capture retained terminal escapes: %q", body)
			}
		})
	}
}

// This is a deterministic full-screen application: it enters the alternate
// screen, paints a small UI, waits for a key in raw mode, and restores the tty.
func TestCaptureStructuredTUI(t *testing.T) {
	c, _ := agentPaneFixture(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("full-screen capture fixture requires python3")
	}
	dir := t.TempDir()
	script, ready := filepath.Join(dir, "screen.py"), filepath.Join(dir, "ready")
	source := `import os
import sys
import termios
import tty

fd = sys.stdin.fileno()
saved = termios.tcgetattr(fd)
try:
    tty.setraw(fd)
    sys.stdout.write("\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")
    sys.stdout.write("\x1b[32mTUI_READY λ\x1b[0m\r\n")
    sys.stdout.write("+------------------+\r\n| full-screen test |\r\n+------------------+\r\n")
    sys.stdout.flush()
    with open(sys.argv[1], "w") as marker:
        marker.write("ready")
    while os.read(fd, 1) not in (b"q", b"\x03", b""):
        pass
finally:
    sys.stdout.write("\x1b[0m\x1b[?25h\x1b[?1049l")
    sys.stdout.flush()
    termios.tcsetattr(fd, termios.TCSANOW, saved)
`
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	c.callToolJSON(t, "send-keys", map[string]any{
		"slot": 2, "keys": captureQuote(python) + " -u " + captureQuote(script) + " " + captureQuote(ready), "enter": true,
	}, &map[string]any{})
	if !waitForFile(ready, 8*time.Second) {
		t.Fatal("full-screen fixture never entered its alternate screen")
	}
	for _, colors := range []bool{false, true} {
		t.Run(fmt.Sprintf("colors=%v", colors), func(t *testing.T) {
			body := captureAssertStructuredText(t, callTool(t, c, "capture-pane", map[string]any{
				"slot": 2, "lines": 200, "colors": colors,
			}), "capture-pane", 2)
			if !strings.Contains(body, "TUI_READY λ") || !strings.Contains(body, "full-screen test") {
				t.Errorf("capture did not include the full-screen UI: %q", body)
			}
			if colors && !strings.Contains(body, "\x1b[") {
				t.Errorf("colored full-screen capture has no color sequences: %q", body)
			}
			if !colors && strings.Contains(body, "\x1b[") {
				t.Errorf("plain full-screen capture retained terminal escapes: %q", body)
			}
		})
	}
	c.callToolJSON(t, "send-keys", map[string]any{"slot": 2, "keys": "q"}, &map[string]any{})
}

func captureSafeBrowserOpener(t *testing.T) {
	t.Helper()
	// The browser opener is an external dependency. It must not open a real
	// desktop browser during a test; no assertions inspect its invocation.
	dir := t.TempDir()
	for _, name := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestScreenshotStructuredText(t *testing.T) {
	for _, output := range []string{"html", "browser"} {
		t.Run(output, func(t *testing.T) {
			if testing.Short() {
				t.Skip("requires tmux")
			}
			captureSafeBrowserOpener(t)
			c, self := agentPaneFixture(t)
			openSlot(t, c, self, 3)
			result := callTool(t, c, "screenshot-pane", map[string]any{"slot": 3, "output": output})
			body := captureAssertStructuredText(t, result, "screenshot-pane", 3)
			if body == "" {
				t.Error("text screenshot response is empty")
			}
		})
	}
}

// The same request checks either a real PNG or the documented textual fallback.
// Which branch ran is logged, rather than claiming that both were exercised.
func TestScreenshotStructuredDefault(t *testing.T) {
	c, self := agentPaneFixture(t)
	openSlot(t, c, self, 3)
	raw := c.callToolRaw(t, "screenshot-pane", map[string]any{"slot": 3})
	var result callToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("default screenshot failed rather than rendering or falling back: %s", result.text(t, "screenshot-pane"))
	}
	var wire struct {
		Content []struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MIMEType string `json:"mimeType"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	images := 0
	for _, block := range wire.Content {
		if block.Type != "image" {
			continue
		}
		images++
		if block.MIMEType != "image/png" {
			t.Errorf("screenshot MIME type=%q, want image/png", block.MIMEType)
		}
		data, err := base64.StdEncoding.DecodeString(block.Data)
		if err != nil {
			t.Fatalf("image is not base64: %v", err)
		}
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width <= 0 || config.Height <= 0 {
			t.Fatalf("screenshot is not a nonempty PNG: %+v, %v", config, err)
		}
	}
	if images == 0 {
		t.Log("default screenshot exercised the textual PNG-fallback branch")
		captureAssertStructuredText(t, result, "screenshot-pane", 3)
		return
	}
	t.Log("default screenshot exercised the real PNG branch")
	want := map[string]any{"slot": float64(3)}
	if !reflect.DeepEqual(result.StructuredContent, want) {
		t.Errorf("image screenshot structuredContent=%#v, want exactly %#v", result.StructuredContent, want)
	}
}

func TestReadingErrorsOmitStructured(t *testing.T) {
	c, self := agentPaneFixture(t)
	openSlot(t, c, self, 1)
	for _, tool := range []string{"capture-pane", "screenshot-pane"} {
		t.Run(tool, func(t *testing.T) {
			raw := c.callToolRaw(t, tool, map[string]any{"slot": 64})
			var result callToolResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatalf("%s succeeded on a missing slot: %s", tool, raw)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["structuredContent"]; present {
				t.Errorf("%s error carries structuredContent: %s", tool, raw)
			}
		})
	}
}
