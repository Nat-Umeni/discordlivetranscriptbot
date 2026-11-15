// Package sessions remembers which servers have captions running.
package sessions

import (
	"maps"
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// Session is one running transcription.
type Session struct {
	StartedBy snowflake.ID // who ran /transcribe start; they can always stop it
	ChannelID snowflake.ID // the voice channel being transcribed
	Title     string
}

// Store remembers which servers have a transcription running. It only lives
// in memory: when the bot stops, it leaves voice anyway.
type Store struct {
	// mu: commands run concurrently, and simultaneous map access crashes the
	// bot ("concurrent map writes").
	mu       sync.Mutex
	byServer map[snowflake.ID]Session
}

// New makes an empty store.
func New() *Store {
	return &Store{byServer: make(map[snowflake.ID]Session)}
}

// Start records a new session for a server, unless one is already running,
// in which case it returns that session and false.
//
// Checking and recording happen under one lock. If they were separate steps,
// two people running /transcribe start at the same moment could both see "no
// session", both carry on, and fight over the one voice connection a bot can
// have per server.
func (s *Store) Start(serverID snowflake.ID, newSession Session) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, running := s.byServer[serverID]
	if running {
		return existing, false
	}

	s.byServer[serverID] = newSession
	return newSession, true
}

// Get returns the server's running session, and false if there isn't one.
func (s *Store) Get(serverID snowflake.ID) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, running := s.byServer[serverID]
	return current, running
}

// Remove forgets the server's session, so a new one can start.
func (s *Store) Remove(serverID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.byServer, serverID)
}

// All returns every running session, keyed by server. It's a copy, so the
// caller can loop over it without holding the lock.
func (s *Store) All() map[snowflake.ID]Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	return maps.Clone(s.byServer)
}
