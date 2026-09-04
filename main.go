package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"runtime/debug"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// version is the build's version string. GoReleaser sets it at link time
// (-ldflags "-X main.version=<tag>"), so release tarballs report the clean git
// tag; a plain `go build` leaves it as "dev".
var version = "dev"

// resolveVersion returns the build's version. GoReleaser's injected tag wins.
// Failing that — the case that matters for the terminal plugin, which installs
// this via `go install github.com/MadAppGang/tmux-mcp/v2@latest` (its setup.go
// dependency) — the resolved tag is carried in the binary's build info. That is
// accepted only when it is a clean release tag: never the "(devel)" placeholder
// and never a build with metadata such as the "+dirty" suffix a modified working
// tree adds, so the reported version is always a real release and never a weird
// local-build string.
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" && !strings.Contains(v, "+") {
			return v
		}
	}
	return version
}

// backendTmux is the only accepted value of -backend in v2.0.0.
const backendTmux = "tmux"

func main() {
	shellType := flag.String("shell-type", "bash", "Shell type for exit code capture (bash/zsh/fish)")
	channelMode := flag.Bool("channel", false, "Enable Claude Code channel mode: push terminal events as channel notifications")
	showVersion := flag.Bool("version", false, "Print the version and exit")

	// -backend selects the multiplexer. "tmux" is the only accepted value in
	// v2.0.0. The flag exists ahead of a second backend so the surface a
	// consumer pins does not change when one lands: a config file that already
	// says --backend=tmux keeps working, and nothing about the tool surface
	// depends on the answer.
	//
	// There is no environment detection to do. Every agent runtime starts this
	// server from inside a pane, and with nothing set the answer is still tmux —
	// invisible slots need no window and work outside it, so refusing to start
	// would remove the one mode that still functions there.
	//
	// $MAGMUX_SOCK is RESERVED and deliberately not implemented: detecting it now
	// would hand a caller a backend that does not exist.
	backendName := flag.String("backend", backendTmux, "Multiplexer backend (tmux)")

	flag.Parse()

	if *showVersion {
		fmt.Println(resolveVersion())
		return
	}

	if *backendName != backendTmux {
		log.Fatalf("unknown backend %q: this build implements %q", *backendName, backendTmux)
	}

	// The policy layer holds the port and nothing else, so no handler can reach
	// a tmux command even by accident.
	//
	// Constructing the backend also fixes this server's isolated namespace and
	// reclaims the namespaces of servers that are gone — see
	// reapOrphanedNamespaces. That replaces the stale-socket cleanup this line
	// used to perform, which ran kill-server on a socket other servers share.
	sl := newSlots(newTmuxBackend(newTmuxClient(*shellType)))

	// Resource capabilities are NOT advertised: this server registers no
	// resources. The two it used to serve were tmux://sessions and the
	// tmux://pane/{paneId} template, and the template is a paneId in the
	// surface — the one thing this contract does not have. Advertising an empty
	// capability would tell a client to call resources/list for nothing.
	var serverOpts []server.ServerOption
	serverOpts = append(serverOpts,
		server.WithToolCapabilities(true),
	)

	if *channelMode {
		hooks := &server.Hooks{}
		hooks.AddAfterInitialize(func(
			ctx context.Context, id any,
			req *mcp.InitializeRequest,
			result *mcp.InitializeResult,
		) {
			if result.Capabilities.Experimental == nil {
				result.Capabilities.Experimental = make(map[string]any)
			}
			result.Capabilities.Experimental["claude/channel"] = map[string]any{}
		})
		serverOpts = append(serverOpts,
			server.WithHooks(hooks),
			server.WithInstructions(channelInstructions),
		)
	}

	s := server.NewMCPServer("tmux-mcp", resolveVersion(), serverOpts...)

	emitter := newChannelEmitter(*channelMode, s)

	registerAgentTools(s, sl, emitter)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// ---- Helpers ----

// jsonResult marshals v to indented JSON and returns it as a tool result.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to serialize result", err), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}
