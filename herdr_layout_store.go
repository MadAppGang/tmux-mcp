package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The herdr layout store holds what the agent grid needs that herdr has no
// place for: which agent each helper was opened for, and which agents have the
// grid switched on in which tab.
//
// It is a file of its own, beside the registry rather than inside it, and that
// is the point. Every version of this server rewrites the registry on its own
// reads and writes, and a version older than the grid rewrites it from a struct
// that has no field for any of this, so a stamp kept there would vanish the
// first time an older server sharing the herdr server touched the file. Every
// stamped helper would then look unstamped, and the stored grid modes would be
// gone. No older binary knows this file exists, so nothing but this code ever
// rewrites it.
//
// Every record carries a witness, as the registry's do. A stamp counts only
// while herdr still lists its pane with the same terminal id, and a tab's mode
// only while its agent pane is listed with the same terminal id, so a herdr
// restart that renumbers panes cannot make an old record name a new pane.
//
// A file that does not parse is read as empty and replaced on the next write.
// The cost is grid modes that have to be set again and helpers that are left
// out of the grid until they are reopened: the conservative direction.

// herdrAgentStamp is one helper's agent.
type herdrAgentStamp struct {
	PaneID     string `json:"pane_id"`
	TerminalID string `json:"terminal_id"`
	Agent      string `json:"agent_pane_id"`
}

// herdrTabMode is one agent's layout mode in one tab.
type herdrTabMode struct {
	TabID           string `json:"tab_id"`
	AgentPaneID     string `json:"agent_pane_id"`
	AgentTerminalID string `json:"agent_terminal_id"`
	Layout          string `json:"layout"`
}

// herdrLayoutFile is the store's shape.
type herdrLayoutFile struct {
	Version int               `json:"version"`
	Agents  []herdrAgentStamp `json:"agents"`
	Tabs    []herdrTabMode    `json:"tabs"`
}

const herdrLayoutVersion = 1

// herdrLayoutStore is the layout file for one herdr server.
type herdrLayoutStore struct {
	path string
	mu   sync.Mutex
}

// herdrLayoutPath names the layout file that sits beside a registry file.
func herdrLayoutPath(registryPath string) string {
	return strings.TrimSuffix(registryPath, ".json") + ".layout.json"
}

// errNoLayoutStore is a write to a backend that was built without a store.
var errNoLayoutStore = errors.New("there is nowhere to store the layout")

// read returns the current contents. A missing or unreadable-as-JSON file is
// empty.
func (r *herdrLayoutStore) read() (herdrLayoutFile, error) {
	if r == nil {
		return herdrLayoutFile{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := lockSibling(r.path)
	if err != nil {
		return herdrLayoutFile{}, err
	}
	defer unlock()
	return r.load()
}

// update applies change under the lock and writes the result.
func (r *herdrLayoutStore) update(change func(*herdrLayoutFile)) error {
	if r == nil {
		return errNoLayoutStore
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := lockSibling(r.path)
	if err != nil {
		return err
	}
	defer unlock()
	file, err := r.load()
	if err != nil {
		return err
	}
	change(&file)
	file.Version = herdrLayoutVersion
	if file.Agents == nil {
		file.Agents = []herdrAgentStamp{}
	}
	if file.Tabs == nil {
		file.Tabs = []herdrTabMode{}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the layout store: %w", err)
	}
	return writeFileAtomic(r.path, append(data, '\n'))
}

func (r *herdrLayoutStore) load() (herdrLayoutFile, error) {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return herdrLayoutFile{}, nil
	}
	if err != nil {
		return herdrLayoutFile{}, fmt.Errorf("read the layout store: %w", err)
	}
	var file herdrLayoutFile
	if json.Unmarshal(data, &file) != nil {
		return herdrLayoutFile{}, nil
	}
	return file, nil
}

// prune drops every record whose witness herdr no longer confirms. live maps
// every pane herdr lists to its terminal id.
func (f *herdrLayoutFile) prune(live map[string]string) {
	agents := f.Agents[:0]
	for _, a := range f.Agents {
		if term, ok := live[a.PaneID]; ok && term != "" && term == a.TerminalID {
			agents = append(agents, a)
		}
	}
	f.Agents = agents
	tabs := f.Tabs[:0]
	for _, t := range f.Tabs {
		if term, ok := live[t.AgentPaneID]; ok && term != "" && term == t.AgentTerminalID {
			tabs = append(tabs, t)
		}
	}
	f.Tabs = tabs
}

// renamePane carries a helper's stamp over to the identity herdr gave its pane
// when it moved it, with the terminal id herdr now reports as its witness.
func (r *herdrLayoutStore) renamePane(oldID, newID, terminalID string) error {
	return r.update(func(f *herdrLayoutFile) {
		for i := range f.Agents {
			if f.Agents[i].PaneID == oldID {
				f.Agents[i].PaneID, f.Agents[i].TerminalID = newID, terminalID
			}
		}
	})
}

// lockSibling takes the cross-process lock on path's sibling .lock file and
// returns its release.
func lockSibling(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create the state directory: %w", err)
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the state lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock the state file: %w", err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// writeFileAtomic writes data to a temporary file beside path and renames it
// into place, so a reader never sees half a file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("write the state file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write the state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write the state file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write the state file: %w", err)
	}
	return nil
}
