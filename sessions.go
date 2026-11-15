package main

import (
	"maps"
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// session is one running transcription.
type session struct {
	StartedBy snowflake.ID // who ran /transcribe start; they can always stop it
	ChannelID snowflake.ID // the voice channel being transcribed
	Title     string
}

// sessionStore remembers which servers have a transcription running. It only
// lives in memory: when the bot stops, it leaves voice anyway.
type sessionStore struct {
	// mu: same reason as settingsStore. Commands run concurrently and
	// simultaneous map access crashes the bot.
	mu       sync.Mutex
	byServer map[snowflake.ID]session
}

var activeSessions = &sessionStore{byServer: make(map[snowflake.ID]session)}

// start records a new session for a server, unless one is already running,
// in which case it returns that session and false.
//
// Checking and recording happen under one lock. If they were separate steps,
// two people running /transcribe start at the same moment could both see "no
// session", both carry on, and fight over the one voice connection a bot can
// have per server.
func (s *sessionStore) start(serverID snowflake.ID, newSession session) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, running := s.byServer[serverID]
	if running {
		return existing, false
	}

	s.byServer[serverID] = newSession
	return newSession, true
}

// get returns the server's running session, and false if there isn't one.
func (s *sessionStore) get(serverID snowflake.ID) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, running := s.byServer[serverID]
	return current, running
}

// remove forgets the server's session, so a new one can start.
func (s *sessionStore) remove(serverID snowflake.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.byServer, serverID)
}

// all returns every running session, keyed by server. It's a copy, so the
// caller can loop over it without holding the lock.
func (s *sessionStore) all() map[snowflake.ID]session {
	s.mu.Lock()
	defer s.mu.Unlock()

	return maps.Clone(s.byServer)
}
