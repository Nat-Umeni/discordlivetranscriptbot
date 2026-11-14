package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// serverSettings is what the bot remembers about one server.
type serverSettings struct {
	// The role whose members count as note-takers, picked by an admin with
	// /captions-setup. The struct tag sets the key name used in settings.json.
	NoteTakerRoleID snowflake.ID `json:"noteTakerRoleID"`
}

// settingsStore holds every server's settings in memory and saves them to a
// JSON file, so they survive the bot restarting.
type settingsStore struct {
	// mu stops two commands touching servers at the same time. middleware.Go
	// runs commands concurrently, and two goroutines using one map at once
	// crashes the whole bot ("concurrent map writes"), so every method locks
	// it first.
	mu      sync.Mutex
	path    string
	servers map[snowflake.ID]serverSettings
}

// settings is package-level so command handlers can reach it: disgo fixes
// their signature, so it can't be passed in as an argument.
var settings *settingsStore

// loadSettings reads the settings file into memory. A missing file isn't an
// error: it just means no server has been set up yet, so it starts empty.
func loadSettings(path string) (*settingsStore, error) {
	store := &settingsStore{
		path: path,
		// make creates an empty map ready to use. Without it the map is nil,
		// and writing to a nil map crashes.
		servers: make(map[snowflake.ID]serverSettings),
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}

	// & lets Unmarshal fill in the store's map rather than a copy of it.
	err = json.Unmarshal(data, &store.servers)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return store, nil
}

// NoteTakerRole returns the note-taker role picked for a server, and false if
// an admin hasn't picked one yet.
func (s *settingsStore) NoteTakerRole(serverID snowflake.ID) (snowflake.ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.servers[serverID]
	return entry.NoteTakerRoleID, ok
}

// SetNoteTakerRole remembers a server's note-taker role and saves every
// server's settings to the file.
func (s *settingsStore) SetNoteTakerRole(serverID, roleID snowflake.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.servers[serverID] = serverSettings{NoteTakerRoleID: roleID}

	// MarshalIndent lays the JSON out over several lines so the file is
	// readable when opened by hand.
	data, err := json.MarshalIndent(s.servers, "", "  ")
	if err != nil {
		return err
	}

	// 0o644: you can read and write the file, everyone else can only read it.
	return os.WriteFile(s.path, data, 0o644)
}
