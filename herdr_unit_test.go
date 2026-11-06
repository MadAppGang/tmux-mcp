package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHerdrRegistryCorruptFileIsMovedAside is a state file that does not parse.
// Treating it as fatal would fail every visible-slot call until somebody found
// the file and deleted it by hand; the records are only trusted once herdr
// confirms them, so starting empty is safe, and the old file is kept beside it.
//
// REGRESSION: an unparseable herdr state file failed every slot call permanently
func TestHerdrRegistryCorruptFileIsMovedAside(t *testing.T) {
	r := newTestRegistry(t)
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.upsert(herdrEntry{PaneID: "w1:p2", TerminalID: "term_aa", Owner: ownerAgent, Slot: 1}); err != nil {
		t.Fatalf("a write after a corrupt file failed: %v", err)
	}
	got, err := r.valid(map[string]string{"w1:p2": "term_aa"})
	if err != nil || len(got) != 1 {
		t.Fatalf("after recovery valid = %+v, %v; want the one new record", got, err)
	}
	aside, _ := filepath.Glob(r.path + ".corrupt-*")
	if len(aside) != 1 {
		t.Fatalf("the corrupt file was not kept aside: %v", aside)
	}
	if data, _ := os.ReadFile(aside[0]); string(data) != "{not json" {
		t.Errorf("the file kept aside holds %q, want the original bytes", data)
	}
}

// TestHerdrRegistryReadWritesOnlyWhenItDrops pins that a read whose records are
// all confirmed leaves the file alone. Every capture-pane reads the registry;
// rewriting it each time is needless, and breaks reads on a read-only directory.
func TestHerdrRegistryReadWritesOnlyWhenItDrops(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.upsert(herdrEntry{PaneID: "w1:p2", TerminalID: "term_aa", Owner: ownerAgent, Slot: 1}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.valid(map[string]string{"w1:p2": "term_aa"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("a read that dropped nothing rewrote the state file")
	}

	if _, err := r.valid(map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := r.load(); len(entries) != 0 {
		t.Errorf("a read that should drop the stale record kept it: %+v", entries)
	}
}

// TestHerdrRegistrySnapshotIsTakenUnderTheLock is two servers sharing one herdr.
// One reads the registry, which prunes every record whose pane is missing from
// herdr's live list; the other registers a pane it has just opened. If the live
// list was taken before the lock, a registration that lands between the two is
// checked against a list that predates its pane, and is deleted.
//
// REGRESSION: a read pruned against a pane list taken before the lock, deleting a helper another server had just registered
func TestHerdrRegistrySnapshotIsTakenUnderTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr", "registry.json")
	reader := &herdrRegistry{path: path}
	writer := &herdrRegistry{path: path}

	snapshotTaken := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	var writeErr error
	go func() {
		defer wg.Done()
		<-snapshotTaken
		writeErr = writer.upsert(herdrEntry{PaneID: "w1:p9", TerminalID: "term_99", Owner: ownerAgent, Slot: 2})
	}()

	_, err := reader.validFrom(func() (map[string]string, error) {
		close(snapshotTaken)
		// Long enough for the other server's write to land if nothing stops it.
		time.Sleep(300 * time.Millisecond)
		return map[string]string{}, nil
	})
	if err != nil {
		t.Fatalf("validFrom: %v", err)
	}
	wg.Wait()
	if writeErr != nil {
		t.Fatalf("upsert: %v", writeErr)
	}

	entries, err := reader.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].PaneID != "w1:p9" {
		t.Fatalf("the other server's new record was pruned against a stale pane list: %+v", entries)
	}
}

// TestHerdrRefusesAKeyItCannotTranslate pins that a key name is never typed as
// text. A modified key with no herdr name and no escape sequence is an error,
// not the characters of its name.
//
// REGRESSION: modified keys such as C-Home were typed as the text "C-Home" while the call reported success
func TestHerdrRefusesAKeyItCannotTranslate(t *testing.T) {
	b := &herdrBackend{bin: filepath.Join(t.TempDir(), "never-run")}
	err := b.SendKeys(context.Background(), newPaneRef("w1:p2"), "C-NoSuchKey", false, false)
	if !errors.Is(err, errUnknownKeyName) {
		t.Fatalf("SendKeys(C-NoSuchKey) = %v, want errUnknownKeyName", err)
	}
}

// TestBackendErrorsNameNoMultiplexer pins that what a failed multiplexer call
// says reaches the agent without saying which multiplexer it was.
//
// REGRESSION: herdr's failures reached callers as "herdr: …" and "herdr pane split: …", naming the multiplexer
func TestBackendErrorsNameNoMultiplexer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()

	h := &herdrBackend{bin: "herdr"}
	if _, err := h.run(ctx, "pane", "split", "w1:p1"); err == nil || strings.Contains(strings.ToLower(err.Error()), "herdr") {
		t.Errorf("a herdr call that could not run said %v", err)
	}
	if msg := (&herdrError{Code: "invalid", Message: "bad ratio"}).Error(); strings.Contains(msg, "herdr") {
		t.Errorf("a herdr server error reads %q", msg)
	}

	c := newTmuxClient()
	if _, err := c.run(ctx, "split-window"); err == nil || strings.Contains(strings.ToLower(err.Error()), "tmux") {
		t.Errorf("a tmux call that could not run said %v", err)
	}
}
