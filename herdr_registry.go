package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// The herdr registry is a file, because herdr has nowhere else to put it.
//
// Under tmux the registry is three pane options, so it lives and dies with the
// panes and survives any number of server restarts. herdr panes carry no user
// options: a label and reported metadata are display text the user can edit,
// not state. So the records live in a state file this server keeps, one per
// herdr server:
//
//	$XDG_STATE_HOME/tmux-mcp/herdr/<sha256 of the socket path>.json
//	(~/.local/state when XDG_STATE_HOME is unset)
//
// # The witness
//
// A record names a pane id and the terminal id herdr gave the pane's terminal.
// It counts only while herdr still lists that pane id with that same terminal
// id, which is the job @mcp_pane does under tmux: a record that is not
// confirmed by the pane it describes is not a record. herdr does not reuse pane
// ids within a server, and the terminal id is a random handle, so a herdr
// restart that renumbers panes from p1 again cannot make a stale record match a
// new pane. Records that no longer match are dropped whenever the file is read.
//
// # Writes
//
// Every change is one read-modify-write under an exclusive lock on a sibling
// .lock file, written to a temporary file and renamed into place, so a reader
// never sees half a file and two servers sharing one herdr cannot lose each
// other's records. A claim is therefore one atomic write of owner and slot
// together, where tmux has to order three option writes.

// herdrEntry is one record in the state file.
type herdrEntry struct {
	PaneID     string `json:"pane_id"`
	TerminalID string `json:"terminal_id"`
	Owner      string `json:"owner"`
	Slot       int    `json:"slot,omitempty"`
}

// herdrRegistryFile is the state file's shape.
type herdrRegistryFile struct {
	Version int          `json:"version"`
	Panes   []herdrEntry `json:"panes"`
}

const herdrRegistryVersion = 1

// herdrRegistry is the state file for one herdr server.
type herdrRegistry struct {
	path string
	mu   sync.Mutex
}

// herdrRegistryPath names the state file for the herdr server behind a socket.
func herdrRegistryPath(socket string) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate the state directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(socket))
	return filepath.Join(base, "tmux-mcp", "herdr", hex.EncodeToString(sum[:])+".json"), nil
}

// errNoRecord is what a slot write answers for a pane with no record.
var errNoRecord = errors.New("the pane carries no record")

// update applies change to the records under the lock and writes the result.
func (r *herdrRegistry) update(change func([]herdrEntry) ([]herdrEntry, error)) error {
	return r.updateIf(func(entries []herdrEntry) ([]herdrEntry, bool, error) {
		next, err := change(entries)
		return next, true, err
	})
}

// updateIf is update for a change that may find nothing to do: the file is
// written only when change says it changed something.
func (r *herdrRegistry) updateIf(change func([]herdrEntry) ([]herdrEntry, bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := r.load()
	if err != nil {
		return err
	}
	next, changed, err := change(entries)
	if err != nil || !changed {
		return err
	}
	return r.store(next)
}

// upsert writes one record, replacing any record for the same pane.
func (r *herdrRegistry) upsert(e herdrEntry) error {
	return r.update(func(entries []herdrEntry) ([]herdrEntry, error) {
		out := entries[:0]
		for _, old := range entries {
			if old.PaneID != e.PaneID {
				out = append(out, old)
			}
		}
		return append(out, e), nil
	})
}

// setSlot changes only the slot of an existing record.
func (r *herdrRegistry) setSlot(paneID string, slot int) error {
	return r.update(func(entries []herdrEntry) ([]herdrEntry, error) {
		for i := range entries {
			if entries[i].PaneID == paneID {
				if slot < 0 {
					slot = 0
				}
				entries[i].Slot = slot
				return entries, nil
			}
		}
		return nil, errNoRecord
	})
}

// remove drops a pane's record, if it has one.
func (r *herdrRegistry) remove(paneID string) error {
	return r.update(func(entries []herdrEntry) ([]herdrEntry, error) {
		out := entries[:0]
		for _, e := range entries {
			if e.PaneID != paneID {
				out = append(out, e)
			}
		}
		return out, nil
	})
}

// valid returns the records the live panes confirm, and drops the rest from the
// file. live maps every pane id herdr lists to its terminal id, so it must be
// the server's whole pane list: a record whose pane is missing from it is gone.
// A record with an owner this binary does not recognise is kept in the file
// and not returned, for the reason parseRegistryLine gives.
func (r *herdrRegistry) valid(live map[string]string) ([]herdrEntry, error) {
	return r.validFrom(func() (map[string]string, error) { return live, nil })
}

// validFrom is valid with the pane list taken by snapshot, which runs while the
// registry is locked.
//
// The order is the point. Another server sharing this herdr registers a pane
// it has just opened under the same lock; a list taken before the lock can
// predate that pane, and pruning against it would delete a live helper's
// record. Taken inside the lock, the list is at least as new as every record
// the read can see.
//
// The file is written only when a record was dropped: every read of a slot
// passes through here, and a read that changes nothing should not rewrite the
// file.
func (r *herdrRegistry) validFrom(snapshot func() (map[string]string, error)) ([]herdrEntry, error) {
	var confirmed []herdrEntry
	err := r.updateIf(func(entries []herdrEntry) ([]herdrEntry, bool, error) {
		live, err := snapshot()
		if err != nil {
			return nil, false, err
		}
		kept := make([]herdrEntry, 0, len(entries))
		for _, e := range entries {
			if term, ok := live[e.PaneID]; !ok || term == "" || term != e.TerminalID {
				continue
			}
			kept = append(kept, e)
			if e.Owner == ownerAgent || e.Owner == ownerAcquired {
				confirmed = append(confirmed, e)
			}
		}
		return kept, len(kept) != len(entries), nil
	})
	return confirmed, err
}

// lock takes the cross-process lock and returns its release.
func (r *herdrRegistry) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return nil, fmt.Errorf("create the state directory: %w", err)
	}
	f, err := os.OpenFile(r.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the registry lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock the registry: %w", err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// load reads the records. A missing file is an empty registry, and so is one
// that does not parse: it is moved aside to <path>.corrupt-<unix seconds>, so
// that what was in it can still be looked at, and the registry starts again.
// Failing instead would fail every slot call until somebody deleted the file
// by hand. Starting empty is safe because a record is trusted only once herdr
// confirms it; the cost is that panes the lost records named are no longer
// known to be ours, so they are left alone like any pane the user opened.
//
// A file that cannot be read at all is still an error: that is a permissions
// or disk problem, and moving the file would not fix it.
func (r *herdrRegistry) load() ([]herdrEntry, error) {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the registry: %w", err)
	}
	var file herdrRegistryFile
	if err := json.Unmarshal(data, &file); err != nil {
		aside := r.path + ".corrupt-" + strconv.FormatInt(time.Now().Unix(), 10)
		if renameErr := os.Rename(r.path, aside); renameErr != nil {
			return nil, fmt.Errorf("parse the registry: %w", err)
		}
		return nil, nil
	}
	return file.Panes, nil
}

// store writes the records to a temporary file and renames it into place.
func (r *herdrRegistry) store(entries []herdrEntry) error {
	if entries == nil {
		entries = []herdrEntry{}
	}
	data, err := json.MarshalIndent(herdrRegistryFile{Version: herdrRegistryVersion, Panes: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the registry: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".registry-*")
	if err != nil {
		return fmt.Errorf("write the registry: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write the registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write the registry: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write the registry: %w", err)
	}
	return nil
}
