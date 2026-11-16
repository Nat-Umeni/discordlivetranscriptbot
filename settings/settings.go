// Package settings keeps each server's caption settings (note-taker role,
// captions channel, no-captions role) in settings.json.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// serverSettings is what the bot remembers about one server.
type serverSettings struct {
	// The role whose members count as note-takers, picked by an admin with
	// /captions-setup. The struct tag sets the key name used in settings.json;
	// omitempty leaves it out of the file while it's not set (0).
	NoteTakerRoleID snowflake.ID `json:"noteTakerRoleID,omitempty"`

	// The text channel where each call's captions thread is created, also
	// picked with /captions-setup.
	CaptionsChannelID snowflake.ID `json:"captionsChannelID,omitempty"`

	// Captions channels picked before the current one. Their threads are still
	// captions threads, so their transcripts can still be rebuilt.
	PastCaptionsChannelIDs []snowflake.ID `json:"pastCaptionsChannelIDs,omitempty"`

	// Members with this role are never captioned: the way to opt out. Also
	// picked with /captions-setup.
	NoCaptionsRoleID snowflake.ID `json:"noCaptionsRoleID,omitempty"`
}

// marshalSettings turns the settings into the file's JSON. It's a variable
// only so a test can make it fail: these settings always encode fine.
var marshalSettings = json.MarshalIndent

// Store holds every server's settings in memory and saves them to a
// JSON file, so they survive the bot restarting.
type Store struct {
	// mu stops two commands touching servers at the same time. middleware.Go
	// runs commands concurrently, and two goroutines using one map at once
	// crashes the whole bot ("concurrent map writes"), so every method locks
	// it first.
	mu      sync.Mutex
	path    string
	servers map[snowflake.ID]serverSettings
}

// Load reads the settings file into memory. A missing file isn't an
// error: it just means no server has been set up yet, so it starts empty.
func Load(path string) (*Store, error) {
	store := &Store{
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
func (s *Store) NoteTakerRole(serverID snowflake.ID) (snowflake.ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 0 means not set: a server that only has a captions channel saved still
	// has an entry here, but no role.
	roleID := s.servers[serverID].NoteTakerRoleID
	return roleID, roleID != 0
}

// SetNoteTakerRole remembers a server's note-taker role and saves every
// server's settings to the file.
func (s *Store) SetNoteTakerRole(serverID, roleID snowflake.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Go can't change one field of a struct stored in a map directly, so copy
	// the server's settings out, change the copy and put it back. Building a
	// fresh serverSettings instead would wipe the captions channel.
	entry := s.servers[serverID]
	entry.NoteTakerRoleID = roleID
	s.servers[serverID] = entry

	return s.save()
}

// CaptionsChannel returns the channel picked for a server's captions threads,
// and false if an admin hasn't picked one yet.
func (s *Store) CaptionsChannel(serverID snowflake.ID) (snowflake.ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	channelID := s.servers[serverID].CaptionsChannelID
	return channelID, channelID != 0
}

// SetCaptionsChannel remembers where a server's captions threads go and saves
// every server's settings to the file. A channel it replaces goes on the list
// of past captions channels, so its threads still count as captions threads.
func (s *Store) SetCaptionsChannel(serverID, channelID snowflake.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.servers[serverID]
	old := entry.CaptionsChannelID
	if old != 0 && old != channelID && !slices.Contains(entry.PastCaptionsChannelIDs, old) {
		entry.PastCaptionsChannelIDs = append(entry.PastCaptionsChannelIDs, old)
	}
	// Picking an old channel again makes it the current one, not both.
	entry.PastCaptionsChannelIDs = slices.DeleteFunc(entry.PastCaptionsChannelIDs, func(id snowflake.ID) bool {
		return id == channelID
	})
	entry.CaptionsChannelID = channelID
	s.servers[serverID] = entry

	return s.save()
}

// IsCaptionsChannel says whether a channel is, or ever was, a server's
// captions channel: where its captions threads are.
func (s *Store) IsCaptionsChannel(serverID, channelID snowflake.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.servers[serverID]
	return channelID != 0 && (channelID == entry.CaptionsChannelID || slices.Contains(entry.PastCaptionsChannelIDs, channelID))
}

// NoCaptionsRole returns the role whose members are never captioned, and false
// if an admin hasn't picked one yet.
func (s *Store) NoCaptionsRole(serverID snowflake.ID) (snowflake.ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	roleID := s.servers[serverID].NoCaptionsRoleID
	return roleID, roleID != 0
}

// SetNoCaptionsRole remembers a server's no-captions role and saves every
// server's settings to the file.
func (s *Store) SetNoCaptionsRole(serverID, roleID snowflake.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.servers[serverID]
	entry.NoCaptionsRoleID = roleID
	s.servers[serverID] = entry

	return s.save()
}

// save writes every server's settings to the file. Callers must hold s.mu.
func (s *Store) save() error {
	// MarshalIndent lays the JSON out over several lines so the file is
	// readable when opened by hand.
	data, err := marshalSettings(s.servers, "", "  ")
	if err != nil {
		return err
	}

	// 0o644: you can read and write the file, everyone else can only read it.
	return os.WriteFile(s.path, data, 0o644)
}
