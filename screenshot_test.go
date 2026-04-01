package main

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// ---------- TestMapPostScriptToCSS ----------

func TestMapPostScriptToCSS(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "JetBrains Mono NL Nerd Font with Regular suffix",
			input:  "JetBrainsMonoNLNF-Regular",
			expect: "JetBrainsMonoNL Nerd Font",
		},
		{
			name:   "FiraCode with Bold suffix",
			input:  "FiraCode-Bold",
			expect: "Fira Code",
		},
		{
			name:   "JetBrainsMono without suffix",
			input:  "JetBrainsMono",
			expect: "JetBrains Mono",
		},
		{
			name:   "Hack no suffix (known mapping, no transform needed)",
			input:  "Hack",
			expect: "Hack",
		},
		{
			name:   "HackNF with Medium suffix",
			input:  "HackNF-Medium",
			expect: "Hack Nerd Font",
		},
		{
			name:   "SourceCodePro with Light suffix",
			input:  "SourceCodePro-Light",
			expect: "Source Code Pro",
		},
		{
			name:   "CascadiaCode with Italic suffix",
			input:  "CascadiaCode-Italic",
			expect: "Cascadia Code",
		},
		{
			name:   "Unknown font gets space insertion",
			input:  "SomeCustomFont",
			expect: "Some Custom Font",
		},
		{
			name:   "Unknown single-word font unchanged",
			input:  "Monospace",
			expect: "Monospace",
		},
		{
			name:   "Unknown multi-word with suffix stripped",
			input:  "MyNiceFont-Regular",
			expect: "My Nice Font",
		},
		{
			name:   "IBMPlexMono known mapping",
			input:  "IBMPlexMono",
			expect: "IBM Plex Mono",
		},
		{
			name:   "MesloLGS known mapping",
			input:  "MesloLGS",
			expect: "MesloLGS NF",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapPostScriptToCSS(tc.input)
			if got != tc.expect {
				t.Errorf("mapPostScriptToCSS(%q) = %q, want %q", tc.input, got, tc.expect)
			}
		})
	}
}

// ---------- TestGenerateScreenshotHTML ----------

func TestGenerateScreenshotHTML(t *testing.T) {
	ansiContent := "hello \033[31mworld\033[0m"
	cols := 120
	rows := 40
	style := terminalStyle{
		FontFamily: `"JetBrains Mono", monospace`,
		FontSize:   16,
		Background: "#141920",
		Foreground: "#d4d4d4",
	}

	t.Run("dark theme basics", func(t *testing.T) {
		html := generateScreenshotHTML(ansiContent, cols, rows, style, "dark")

		// Contains xterm.js CDN CSS link.
		if !strings.Contains(html, "cdn.jsdelivr.net/npm/@xterm/xterm") {
			t.Error("missing xterm.js CDN CSS link")
		}
		// Contains xterm.js CDN script.
		if !strings.Contains(html, "xterm.min.js") {
			t.Error("missing xterm.js CDN script")
		}
		// Contains base64-encoded content.
		b64 := base64.StdEncoding.EncodeToString([]byte(ansiContent))
		if !strings.Contains(html, b64) {
			t.Error("missing base64-encoded ANSI content")
		}
		// Contains cols and rows.
		if !strings.Contains(html, "cols: 120") {
			t.Errorf("missing cols value, expected 'cols: 120'")
		}
		if !strings.Contains(html, "rows: 40") {
			t.Errorf("missing rows value, expected 'rows: 40'")
		}
		// Contains font family.
		if !strings.Contains(html, style.FontFamily) {
			t.Errorf("missing font family %q", style.FontFamily)
		}
		// Contains font size.
		if !strings.Contains(html, "fontSize: 16") {
			t.Errorf("missing fontSize value")
		}
	})

	t.Run("dark theme uses detected background", func(t *testing.T) {
		html := generateScreenshotHTML(ansiContent, cols, rows, style, "dark")
		if !strings.Contains(html, style.Background) {
			t.Errorf("dark theme should contain detected background %s", style.Background)
		}
	})

	t.Run("light theme has light background", func(t *testing.T) {
		html := generateScreenshotHTML(ansiContent, cols, rows, style, "light")
		// Light theme uses Catppuccin Latte background.
		if !strings.Contains(html, "#eff1f5") {
			t.Error("light theme should contain light terminal background #eff1f5")
		}
		if !strings.Contains(html, "#dce0e8") {
			t.Error("light theme should contain light page background #dce0e8")
		}
		// Light theme should NOT contain the dark background.
		if strings.Contains(html, "#1e1e2e") {
			t.Error("light theme should not contain dark terminal background")
		}
	})

	t.Run("valid HTML structure", func(t *testing.T) {
		html := generateScreenshotHTML(ansiContent, cols, rows, style, "dark")
		if !strings.HasPrefix(html, "<!DOCTYPE html>") {
			t.Error("should start with <!DOCTYPE html>")
		}
		if !strings.Contains(html, "<html lang=\"en\">") {
			t.Error("missing <html lang> tag")
		}
		if !strings.Contains(html, "</html>") {
			t.Error("missing closing </html> tag")
		}
	})
}

// ---------- TestDetectTerminalStyle ----------

func TestDetectTerminalStyle(t *testing.T) {
	t.Run("unset TERM_PROGRAM returns defaults", func(t *testing.T) {
		orig := os.Getenv("TERM_PROGRAM")
		os.Unsetenv("TERM_PROGRAM")
		defer func() {
			if orig != "" {
				os.Setenv("TERM_PROGRAM", orig)
			}
		}()

		s := detectTerminalStyle()
		ds := defaultStyle()

		if s.FontFamily != ds.FontFamily {
			t.Errorf("expected default font family %q, got %q", ds.FontFamily, s.FontFamily)
		}
		if s.FontSize != ds.FontSize {
			t.Errorf("expected default font size %d, got %d", ds.FontSize, s.FontSize)
		}
		if s.Background != ds.Background {
			t.Errorf("expected default background %q, got %q", ds.Background, s.Background)
		}
	})

	t.Run("unknown TERM_PROGRAM returns defaults", func(t *testing.T) {
		orig := os.Getenv("TERM_PROGRAM")
		os.Setenv("TERM_PROGRAM", "SomeUnknownTerminal")
		defer func() {
			if orig != "" {
				os.Setenv("TERM_PROGRAM", orig)
			} else {
				os.Unsetenv("TERM_PROGRAM")
			}
		}()

		s := detectTerminalStyle()
		ds := defaultStyle()

		if s.FontFamily != ds.FontFamily {
			t.Errorf("expected default font family %q, got %q", ds.FontFamily, s.FontFamily)
		}
	})

	t.Run("never panics", func(t *testing.T) {
		envVars := []string{"", "iTerm.app", "ghostty", "kitty", "alacritty", "WezTerm"}
		for _, env := range envVars {
			func() {
				orig := os.Getenv("TERM_PROGRAM")
				if env == "" {
					os.Unsetenv("TERM_PROGRAM")
				} else {
					os.Setenv("TERM_PROGRAM", env)
				}
				defer func() {
					if orig != "" {
						os.Setenv("TERM_PROGRAM", orig)
					} else {
						os.Unsetenv("TERM_PROGRAM")
					}
				}()

				s := detectTerminalStyle()
				if s.FontFamily == "" {
					t.Errorf("TERM_PROGRAM=%q: font family should never be empty", env)
				}
				if s.FontSize <= 0 {
					t.Errorf("TERM_PROGRAM=%q: font size should be positive, got %d", env, s.FontSize)
				}
				if s.Background == "" {
					t.Errorf("TERM_PROGRAM=%q: background should never be empty", env)
				}
			}()
		}
	})
}

// ---------- TestParseITerm2Color ----------

func TestParseITerm2Color(t *testing.T) {
	data := `"Background Color (Dark)" = {
		"Alpha Component" = 1;
		"Blue Component" = "0.12103271484375";
		"Color Space" = sRGB;
		"Green Component" = "0.09911105036735535";
		"Red Component" = "0.0806884765625";
	};`

	t.Run("parses dark background", func(t *testing.T) {
		c := parseITerm2Color(data, "Background Color (Dark)")
		if c == "" {
			t.Fatal("expected a color, got empty")
		}
		// R=0.08*255≈20, G=0.10*255≈25, B=0.12*255≈30 → #14191e
		if c[0] != '#' || len(c) != 7 {
			t.Errorf("expected hex color, got %q", c)
		}
	})

	t.Run("returns empty for missing color", func(t *testing.T) {
		c := parseITerm2Color(data, "Cursor Color")
		if c != "" {
			t.Errorf("expected empty, got %q", c)
		}
	})
}

// ---------- TestGenerateScreenshotHTMLSpecialChars ----------

func TestGenerateScreenshotHTMLSpecialChars(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "single quotes",
			content: "it's a test",
		},
		{
			name:    "double quotes",
			content: `he said "hello"`,
		},
		{
			name:    "backslashes",
			content: `path\to\file`,
		},
		{
			name:    "HTML tags",
			content: `<script>alert("xss")</script>`,
		},
		{
			name:    "ampersands and entities",
			content: `foo & bar &amp; baz`,
		},
		{
			name:    "ANSI escapes with special chars",
			content: "\033[31m<b>bold & \"red\"</b>\033[0m",
		},
		{
			name:    "newlines and tabs",
			content: "line1\nline2\ttab",
		},
		{
			name:    "template-like braces",
			content: "{{NOT_A_PLACEHOLDER}}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			html := generateScreenshotHTML(tc.content, 80, 24, defaultStyle(), "dark")

			// The raw content should be base64-encoded, not appear literally.
			b64 := base64.StdEncoding.EncodeToString([]byte(tc.content))
			if !strings.Contains(html, b64) {
				t.Errorf("base64 encoding of content not found in HTML")
			}

			// HTML structure should remain valid (not broken by the content).
			if !strings.HasPrefix(html, "<!DOCTYPE html>") {
				t.Error("HTML structure broken: missing DOCTYPE")
			}
			if !strings.Contains(html, "</html>") {
				t.Error("HTML structure broken: missing closing </html>")
			}

			// The raw content with HTML-special chars should NOT appear unencoded.
			if strings.Contains(tc.content, "<") && strings.Contains(html, tc.content) {
				t.Error("raw HTML-special content should not appear unencoded in output")
			}
		})
	}
}
