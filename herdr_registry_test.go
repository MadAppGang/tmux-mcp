package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestRegistry(t *testing.T) *herdrRegistry {
	t.Helper()
	return &herdrRegistry{path: filepath.Join(t.TempDir(), "herdr", "registry.json")}
}

// TestHerdrRegistryWitness pins the rule that makes a state-file record a
// record: herdr must still list the pane id with the terminal id the record was
// written against. A herdr server that restarts numbers its panes from p1 again,
// so a pane id alone would hand the agent whichever new pane got the old number.
func TestHerdrRegistryWitness(t *testing.T) {
	r := newTestRegistry(t)
	for _, e := range []herdrEntry{
		{PaneID: "w1:p2", TerminalID: "term_aa", Owner: ownerAgent, Slot: 1},
		{PaneID: "w1:p3", TerminalID: "term_bb", Owner: ownerAgent, Slot: 2},
		{PaneID: "w1:p4", TerminalID: "term_cc", Owner: ownerAgent},
		{PaneID: "w1:p5", TerminalID: "term_dd", Owner: "from-a-newer-version", Slot: 3},
	} {
		if err := r.upsert(e); err != nil {
			t.Fatalf("upsert %s: %v", e.PaneID, err)
		}
	}

	live := map[string]string{
		"w1:p2": "term_aa", // confirmed
		"w1:p3": "term_zz", // same id, a different terminal: a restarted server
		// w1:p4 is gone
		"w1:p5": "term_dd", // confirmed, but an owner this binary does not know
	}
	got, err := r.valid(live)
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if len(got) != 1 || got[0].PaneID != "w1:p2" || got[0].Slot != 1 {
		t.Fatalf("valid returned %+v, want only w1:p2 in slot 1", got)
	}

	// The stale records are gone from the file; the unknown owner is kept, so a
	// newer server that shares this herdr does not lose it.
	entries, err := r.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.PaneID] = true
	}
	if ids["w1:p3"] || ids["w1:p4"] {
		t.Errorf("stale records survived the read: %+v", entries)
	}
	if !ids["w1:p2"] || !ids["w1:p5"] {
		t.Errorf("confirmed records were dropped: %+v", entries)
	}
}

// TestHerdrRegistryWritesAreSerialised is the lock. Several servers can share
// one herdr, each claiming panes in the same file; a read-modify-write that is
// not serialised loses whichever claims landed in between.
func TestHerdrRegistryWritesAreSerialised(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr", "registry.json")
	const writers = 24

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A registry per goroutine: separate mutexes and separate lock-file
			// opens, so only the file lock orders them, as between processes.
			r := &herdrRegistry{path: path}
			errs <- r.upsert(herdrEntry{
				PaneID: fmt.Sprintf("w1:p%d", i), TerminalID: fmt.Sprintf("term_%d", i), Owner: ownerAgent, Slot: i,
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	entries, err := (&herdrRegistry{path: path}).load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != writers {
		t.Fatalf("the file holds %d records after %d concurrent claims; the rest were lost", len(entries), writers)
	}

	// Each write goes through a temporary file and a rename, so nothing else is
	// left in the directory.
	files, _ := os.ReadDir(filepath.Dir(path))
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".registry-") {
			t.Errorf("a temporary file was left behind: %s", f.Name())
		}
	}
}

// TestHerdrRegistrySlotAndRemove covers the two narrower writes: a slot change
// touches nothing else, needs a record to change, and removal forgets a pane.
func TestHerdrRegistrySlotAndRemove(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.setSlot("w1:p9", 1); err == nil {
		t.Error("setSlot succeeded on a pane with no record")
	}
	if err := r.upsert(herdrEntry{PaneID: "w1:p2", TerminalID: "term_aa", Owner: ownerAgent}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := r.setSlot("w1:p2", 4); err != nil {
		t.Fatalf("setSlot: %v", err)
	}
	got, _ := r.valid(map[string]string{"w1:p2": "term_aa"})
	if len(got) != 1 || got[0].Slot != 4 || got[0].Owner != ownerAgent || got[0].TerminalID != "term_aa" {
		t.Fatalf("after setSlot the record is %+v", got)
	}
	if err := r.remove("w1:p2"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got, _ := r.valid(map[string]string{"w1:p2": "term_aa"}); len(got) != 0 {
		t.Errorf("a removed record is still returned: %+v", got)
	}
}

// TestHerdrKeyNames pins the translation of the key names the tool surface
// documents into what herdr accepts.
func TestHerdrKeyNames(t *testing.T) {
	cases := []struct{ in, key, seq string }{
		{"Enter", "enter", ""},
		{"Escape", "esc", ""},
		{"BSpace", "backspace", ""},
		{"C-c", "ctrl+c", ""},
		{"M-x", "alt+x", ""},
		{"F5", "f5", ""},
		{"Up", "up", ""},
		{"PageUp", "", "\x1b[5~"},
		{"Home", "", "\x1b[H"},
		// A modifier on a key herdr has no name for is the xterm form of that
		// key's sequence, with the modifier parameter 1 + shift(1) + alt(2) + ctrl(4).
		{"C-Home", "", "\x1b[1;5H"},
		{"S-End", "", "\x1b[1;2F"},
		{"M-PageUp", "", "\x1b[5;3~"},
		{"C-M-Home", "", "\x1b[1;7H"},
		{"C-DC", "", "\x1b[3;5~"},
		{"C-Up", "ctrl+up", ""},
	}
	for _, tc := range cases {
		key, seq, named := herdrKey(tc.in)
		if !named || key != tc.key || seq != tc.seq {
			t.Errorf("herdrKey(%q) = %q, %q, %v; want %q, %q, true", tc.in, key, seq, named, tc.key, tc.seq)
		}
	}
	if _, _, named := herdrKey("echo hello"); named {
		t.Error("plain text was taken for a key name; it must be sent as text, as tmux does")
	}
}
