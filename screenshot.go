package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// defaultFontFamily is the CSS font-family fallback when terminal font
// detection fails or the terminal emulator is unrecognized.
const defaultFontFamily = `"JetBrains Mono", "Menlo", "Consolas", monospace`

// defaultFontSize is the font size used when detection fails.
const defaultFontSize = 14

// terminalStyle holds the visual properties detected from the terminal emulator.
type terminalStyle struct {
	FontFamily string // CSS font-family
	FontSize   int    // font size in px
	Background string // hex color for terminal background (e.g. "#000000")
	Foreground string // hex color for terminal foreground (e.g. "#d4d4d4")
}

// findChrome discovers a Chrome/Chromium binary on the system.
// Returns the path or empty string if not found.
func findChrome() string {
	candidates := []string{
		"google-chrome-stable",
		"google-chrome",
		"chromium",
		"chromium-browser",
	}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	// macOS app bundle
	macPath := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	if _, err := os.Stat(macPath); err == nil {
		return macPath
	}
	return ""
}

// renderHTMLToPNG renders an HTML string to a PNG image using headless Chrome.
// cols and rows are used to approximate the window size.
func renderHTMLToPNG(ctx context.Context, htmlContent string, cols, rows int) ([]byte, error) {
	chromePath := findChrome()
	if chromePath == "" {
		return nil, fmt.Errorf("chrome/chromium not found")
	}

	// Write HTML to a temp file.
	tmpHTML, err := os.CreateTemp("", "tmux-screenshot-*.html")
	if err != nil {
		return nil, fmt.Errorf("create temp HTML file: %w", err)
	}
	defer os.Remove(tmpHTML.Name())
	if _, err := tmpHTML.WriteString(htmlContent); err != nil {
		tmpHTML.Close()
		return nil, fmt.Errorf("write HTML: %w", err)
	}
	tmpHTML.Close()

	// Create temp output path for PNG.
	tmpPNG, err := os.CreateTemp("", "tmux-screenshot-*.png")
	if err != nil {
		return nil, fmt.Errorf("create temp PNG file: %w", err)
	}
	tmpPNG.Close()
	defer os.Remove(tmpPNG.Name())

	// Approximate window size from terminal dimensions.
	const charWidth = 8
	const lineHeight = 18
	const padding = 40
	width := cols*charWidth + padding
	height := rows*lineHeight + padding

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, chromePath,
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		fmt.Sprintf("--screenshot=%s", tmpPNG.Name()),
		fmt.Sprintf("--window-size=%d,%d", width, height),
		tmpHTML.Name(),
	)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("chrome headless render: %w", err)
	}

	pngData, err := os.ReadFile(tmpPNG.Name())
	if err != nil {
		return nil, fmt.Errorf("read PNG output: %w", err)
	}
	return pngData, nil
}

// handleScreenshotPane is the handler for the screenshot-pane tool.
//
// It takes the port, a handle AND the slot, and the slot is not redundant: the
// image caption names the pane to the model, and the slot is the only name this
// contract has. That caption used to read "Terminal screenshot of pane %3" —
// one of the four paths an id escaped through that no response type covers,
// because an image caption is not a response field.
func handleScreenshotPane(ctx context.Context, b Backend, pane paneRef, slot int, theme, output string) (*mcp.CallToolResult, error) {
	// 1. Snapshot the pane: its content and its geometry. Both tmux commands,
	// and the 80x24 fallback that covers a failed measurement, live behind
	// Screen — this file renders, it does not talk to the multiplexer.
	snap, err := b.Screen(ctx, pane)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to capture pane", err), nil
	}
	cols, rows := snap.Cols, snap.Rows

	// 2. Detect terminal style (best-effort, never errors).
	style := detectTerminalStyle()

	// 3. Generate HTML.
	html := generateScreenshotHTML(snap.ANSI, cols, rows, style, theme)

	// 4. Output based on mode.
	switch output {
	case "html":
		return mcp.NewToolResultText(html), nil
	case "browser":
		filePath, err := openInBrowser(html)
		if err != nil {
			if filePath != "" {
				return mcp.NewToolResultText(fmt.Sprintf("Screenshot saved but could not open browser: %s\nFile: %s", err, filePath)), nil
			}
			return mcp.NewToolResultErrorFromErr("failed to open screenshot", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Screenshot opened in browser: %s", filePath)), nil
	default:
		// Default: render PNG image via headless Chrome.
		pngData, renderErr := renderHTMLToPNG(ctx, html, cols, rows)
		if renderErr == nil {
			return mcp.NewToolResultImage(
				fmt.Sprintf("Terminal screenshot of slot %d", slot),
				base64.StdEncoding.EncodeToString(pngData),
				"image/png",
			), nil
		}
		// Fallback: return HTML as text with a note.
		return mcp.NewToolResultText("Note: PNG rendering unavailable (" + renderErr.Error() + "). Returning HTML instead.\n\n" + html), nil
	}
}

// defaultStyle returns a terminalStyle with safe defaults.
func defaultStyle() terminalStyle {
	return terminalStyle{
		FontFamily: defaultFontFamily,
		FontSize:   defaultFontSize,
		Background: "#000000",
		Foreground: "#d4d4d4",
	}
}

// detectTerminalStyle reads the terminal emulator's font and color settings.
// On any failure, returns defaults silently.
func detectTerminalStyle() terminalStyle {
	style := defaultStyle()
	termProgram := os.Getenv("TERM_PROGRAM")
	switch termProgram {
	case "iTerm.app":
		detectITerm2Style(&style)
	case "ghostty":
		detectGhosttyStyle(&style)
	case "kitty":
		detectKittyStyle(&style)
	}
	return style
}

// detectITerm2Style reads iTerm2's font and color settings from macOS defaults.
func detectITerm2Style(style *terminalStyle) {
	out, err := exec.Command("defaults", "read", "com.googlecode.iterm2", "New Bookmarks").Output()
	if err != nil {
		return
	}
	s := string(out)

	// Font: "Normal Font" = "JetBrainsMonoNLNF-Regular 15";
	re := regexp.MustCompile(`"Normal Font"\s*=\s*"([^"]+)"`)
	if matches := re.FindStringSubmatch(s); len(matches) >= 2 {
		fontSpec := matches[1]
		parts := strings.Fields(fontSpec)
		if len(parts) >= 2 {
			if size, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
				style.FontSize = size
			}
			psName := strings.Join(parts[:len(parts)-1], " ")
			cssFamily := mapPostScriptToCSS(psName)
			style.FontFamily = fmt.Sprintf("%q, %s", cssFamily, defaultFontFamily)
		}
	}

	// Background color: look for dark mode first, then regular.
	if bg := parseITerm2Color(s, "Background Color (Dark)"); bg != "" {
		style.Background = bg
	} else if bg := parseITerm2Color(s, "Background Color"); bg != "" {
		style.Background = bg
	}

	// Foreground color.
	if fg := parseITerm2Color(s, "Foreground Color (Dark)"); fg != "" {
		style.Foreground = fg
	} else if fg := parseITerm2Color(s, "Foreground Color"); fg != "" {
		style.Foreground = fg
	}
}

// parseITerm2Color extracts a color from iTerm2's plist-style defaults output.
// It looks for a block like:
//
//	"<name>" = {
//	    "Red Component" = "0.08";
//	    "Green Component" = "0.10";
//	    "Blue Component" = "0.12";
//	};
//
// Returns a hex color string like "#141a1f" or "" on failure.
func parseITerm2Color(data, colorName string) string {
	// Find the color block
	marker := fmt.Sprintf("%q = ", colorName)
	idx := strings.Index(data, marker)
	if idx < 0 {
		return ""
	}
	block := data[idx:]
	endIdx := strings.Index(block, "};")
	if endIdx < 0 {
		return ""
	}
	block = block[:endIdx]

	parseComponent := func(name string) float64 {
		re := regexp.MustCompile(fmt.Sprintf(`"%s"\s*=\s*"?([0-9.]+)"?`, name))
		if m := re.FindStringSubmatch(block); len(m) >= 2 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				return v
			}
		}
		return -1
	}

	r := parseComponent("Red Component")
	g := parseComponent("Green Component")
	b := parseComponent("Blue Component")
	if r < 0 || g < 0 || b < 0 {
		return ""
	}

	ri := int(math.Round(r * 255))
	gi := int(math.Round(g * 255))
	bi := int(math.Round(b * 255))
	return fmt.Sprintf("#%02x%02x%02x", ri, gi, bi)
}

// mapPostScriptToCSS converts a PostScript font name to a CSS-friendly
// font-family value. PostScript names use no spaces and often have style
// suffixes like "-Regular".
func mapPostScriptToCSS(psName string) string {
	// Strip style suffixes.
	psName = strings.TrimSuffix(psName, "-Regular")
	psName = strings.TrimSuffix(psName, "-Medium")
	psName = strings.TrimSuffix(psName, "-Bold")
	psName = strings.TrimSuffix(psName, "-Light")
	psName = strings.TrimSuffix(psName, "-Italic")

	// Known mappings for popular programming fonts.
	known := map[string]string{
		"JetBrainsMonoNLNF": "JetBrainsMonoNL Nerd Font",
		"JetBrainsMonoNF":   "JetBrainsMono Nerd Font",
		"JetBrainsMono":     "JetBrains Mono",
		"FiraCode":          "Fira Code",
		"FiraCodeNF":        "FiraCode Nerd Font",
		"SourceCodePro":     "Source Code Pro",
		"CascadiaCode":      "Cascadia Code",
		"CascadiaMono":      "Cascadia Mono",
		"Hack":              "Hack",
		"HackNF":            "Hack Nerd Font",
		"MesloLGS":          "MesloLGS NF",
		"IBMPlexMono":       "IBM Plex Mono",
	}

	if css, ok := known[psName]; ok {
		return css
	}

	// Fallback: insert spaces before uppercase letters for readability.
	// e.g. "SomeFont" -> "Some Font"
	var result strings.Builder
	for i, r := range psName {
		if i > 0 && r >= 'A' && r <= 'Z' {
			result.WriteRune(' ')
		}
		result.WriteRune(r)
	}
	return result.String()
}

// detectGhosttyStyle reads font and color settings from Ghostty config.
func detectGhosttyStyle(style *terminalStyle) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	f, err := os.Open(home + "/.config/ghostty/config")
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := parseGhosttyLine(line)
		if !ok {
			continue
		}
		switch key {
		case "font-family":
			style.FontFamily = fmt.Sprintf("%q, %s", val, defaultFontFamily)
		case "font-size":
			if s, err := strconv.Atoi(val); err == nil {
				style.FontSize = s
			}
		case "background":
			if c := normalizeColor(val); c != "" {
				style.Background = c
			}
		case "foreground":
			if c := normalizeColor(val); c != "" {
				style.Foreground = c
			}
		}
	}
}

// parseGhosttyLine splits a "key = value" line.
func parseGhosttyLine(line string) (key, val string, ok bool) {
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

// detectKittyStyle reads font and color settings from Kitty config.
func detectKittyStyle(style *terminalStyle) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	f, err := os.Open(home + "/.config/kitty/kitty.conf")
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		switch parts[0] {
		case "font_family":
			style.FontFamily = fmt.Sprintf("%q, %s", strings.Join(parts[1:], " "), defaultFontFamily)
		case "font_size":
			if s, err := strconv.Atoi(parts[1]); err == nil {
				style.FontSize = s
			}
		case "background":
			if c := normalizeColor(parts[1]); c != "" {
				style.Background = c
			}
		case "foreground":
			if c := normalizeColor(parts[1]); c != "" {
				style.Foreground = c
			}
		}
	}
}

// normalizeColor ensures a color value starts with '#'.
// Accepts "#rrggbb" or "rrggbb".
func normalizeColor(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "#") {
		s = "#" + s
	}
	if len(s) != 7 {
		return ""
	}
	return s
}

// generateScreenshotHTML produces a self-contained HTML page that renders
// the ANSI content using xterm.js from CDN. The content is base64-encoded
// to avoid any escaping issues with ANSI escape sequences.
func generateScreenshotHTML(ansiContent string, cols, rows int, style terminalStyle, theme string) string {
	b64Data := base64.StdEncoding.EncodeToString([]byte(ansiContent))

	bg := style.Background
	fg := style.Foreground

	var themeObj, bgColor, dotBg string
	if theme == "light" {
		themeObj = `{ background: '#eff1f5', foreground: '#4c4f69', cursor: '#dc8a78', black: '#5c5f77', red: '#d20f39', green: '#40a02b', yellow: '#df8e1d', blue: '#1e66f5', magenta: '#8839ef', cyan: '#179299', white: '#acb0be', brightBlack: '#6c6f85', brightRed: '#d20f39', brightGreen: '#40a02b', brightYellow: '#df8e1d', brightBlue: '#1e66f5', brightMagenta: '#8839ef', brightCyan: '#179299', brightWhite: '#bcc0cc' }`
		bgColor = "#dce0e8"
		dotBg = "#e6e9ef"
	} else {
		themeObj = fmt.Sprintf(`{ background: '%s', foreground: '%s', cursor: '#ffffff', black: '#000000', red: '#f38ba8', green: '#a6e3a1', yellow: '#f9e2af', blue: '#89b4fa', magenta: '#cba6f7', cyan: '#94e2d5', white: '#d4d4d4', brightBlack: '#585b70', brightRed: '#f38ba8', brightGreen: '#a6e3a1', brightYellow: '#f9e2af', brightBlue: '#89b4fa', brightMagenta: '#cba6f7', brightCyan: '#94e2d5', brightWhite: '#ffffff' }`, bg, fg)
		bgColor = bg
		dotBg = bg
	}

	r := strings.NewReplacer(
		"{{BASE64_DATA}}", b64Data,
		"{{COLS}}", strconv.Itoa(cols),
		"{{ROWS}}", strconv.Itoa(rows),
		"{{FONT_FAMILY}}", style.FontFamily,
		"{{FONT_SIZE}}", strconv.Itoa(style.FontSize),
		"{{THEME_OBJ}}", themeObj,
		"{{BG_COLOR}}", bgColor,
		"{{DOT_BG}}", dotBg,
	)
	return r.Replace(screenshotHTMLTemplate)
}

// openInBrowser writes HTML content to a temporary file and opens it in
// the default browser. Returns the file path (even on error, if the file
// was created).
func openInBrowser(htmlContent string) (string, error) {
	tmpFile, err := os.CreateTemp("", "tmux-screenshot-*.html")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	if _, err := tmpFile.WriteString(htmlContent); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("write HTML: %w", err)
	}
	tmpFile.Close()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", tmpFile.Name())
	case "linux":
		cmd = exec.Command("xdg-open", tmpFile.Name())
	default:
		return tmpFile.Name(), fmt.Errorf("unsupported OS %q: file saved to %s", runtime.GOOS, tmpFile.Name())
	}
	if err := cmd.Start(); err != nil {
		return tmpFile.Name(), fmt.Errorf("open browser: %w (file saved to %s)", err, tmpFile.Name())
	}
	return tmpFile.Name(), nil
}

// screenshotHTMLTemplate is the self-contained HTML page template.
// Placeholders: {{BASE64_DATA}}, {{COLS}}, {{ROWS}}, {{FONT_FAMILY}},
// {{FONT_SIZE}}, {{THEME_OBJ}}, {{BG_COLOR}}, {{DOT_BG}}.
const screenshotHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>tmux pane screenshot</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.min.css">
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body {
      background: {{BG_COLOR}};
      display: flex;
      justify-content: center;
      align-items: center;
      min-height: 100vh;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    }
    .window {
      border-radius: 12px;
      overflow: hidden;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.5);
    }
    .title-bar {
      background: {{DOT_BG}};
      padding: 12px 16px;
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .dot {
      width: 12px;
      height: 12px;
      border-radius: 50%;
    }
    .dot-red { background: #ff5f57; }
    .dot-yellow { background: #febc2e; }
    .dot-green { background: #28c840; }
    .title-text {
      flex: 1;
      text-align: center;
      color: #888;
      font-size: 13px;
      margin-right: 52px;
    }
    #terminal {
      padding: 4px;
    }
    .xterm { padding: 8px; }
  </style>
</head>
<body>
  <div class="window">
    <div class="title-bar">
      <div class="dot dot-red"></div>
      <div class="dot dot-yellow"></div>
      <div class="dot dot-green"></div>
      <div class="title-text">tmux pane screenshot</div>
    </div>
    <div id="terminal"></div>
  </div>
  <script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.min.js"></script>
  <script>
    (function() {
      var term = new Terminal({
        cols: {{COLS}},
        rows: {{ROWS}},
        fontFamily: '{{FONT_FAMILY}}',
        fontSize: {{FONT_SIZE}},
        theme: {{THEME_OBJ}},
        convertEol: false,
        disableStdin: true,
        cursorBlink: false,
        cursorStyle: 'bar',
        cursorInactiveStyle: 'none',
        scrollback: 0,
      });
      term.open(document.getElementById('terminal'));

      var b64 = '{{BASE64_DATA}}';
      var raw = atob(b64);
      var bytes = new Uint8Array(raw.length);
      for (var i = 0; i < raw.length; i++) {
        bytes[i] = raw.charCodeAt(i);
      }
      var text = new TextDecoder('utf-8').decode(bytes);
      var lines = text.split('\n');
      for (var j = 0; j < lines.length; j++) {
        if (j > 0) {
          term.write('\r\n');
        }
        term.write(lines[j]);
      }
    })();
  </script>
</body>
</html>
`
