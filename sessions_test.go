package main

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func newSessionStore() *sessionStore {
	return &sessionStore{byServer: make(map[snowflake.ID]session)}
}

func TestSessionStartGetRemove(t *testing.T) {
	store := newSessionStore()

	if _, running := store.get(1); running {
		t.Fatal("a new store should have no sessions")
	}

	first := session{StartedBy: 10, ChannelID: 20, Title: "Weekly"}
	got, claimed := store.start(1, first)
	if !claimed || got != first {
		t.Fatalf("first start: got (%+v, %v), want (%+v, true)", got, claimed, first)
	}

	current, running := store.get(1)
	if !running || current != first {
		t.Fatalf("get: got (%+v, %v), want (%+v, true)", current, running, first)
	}

	store.remove(1)
	if _, running := store.get(1); running {
		t.Fatal("session should be gone after remove")
	}
}

func TestSessionSecondStartGetsTheExistingOne(t *testing.T) {
	store := newSessionStore()
	first := session{StartedBy: 10, Title: "first"}
	store.start(1, first)

	got, claimed := store.start(1, session{StartedBy: 11, Title: "second"})
	if claimed {
		t.Fatal("a second start in the same server should not claim it")
	}
	if got != first {
		t.Fatalf("should get the running session back, got %+v", got)
	}

	current, _ := store.get(1)
	if current != first {
		t.Fatalf("the running session should be unchanged, got %+v", current)
	}
}

func TestSessionsAreSeparatePerServer(t *testing.T) {
	store := newSessionStore()
	store.start(1, session{Title: "server one"})

	if _, claimed := store.start(2, session{Title: "server two"}); !claimed {
		t.Fatal("a session in one server should not block another server")
	}
}

func TestSessionRemoveWhenNothingRunningIsHarmless(t *testing.T) {
	store := newSessionStore()
	store.remove(1) // stop and the voice-leave listener can both remove; must not panic
}

// The reason start checks and records in one locked step: out of many
// simultaneous starts, exactly one may win. Run with -race.
func TestSessionOnlyOneOfManySimultaneousStartsWins(t *testing.T) {
	store := newSessionStore()

	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, claimed := store.start(1, session{StartedBy: snowflake.ID(i)}); claimed {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	if wins.Load() != 1 {
		t.Fatalf("%d starts claimed the server, want exactly 1", wins.Load())
	}
}

func TestSessionAllIsACopyOfEverySession(t *testing.T) {
	store := newSessionStore()
	a := session{StartedBy: 1, ChannelID: 10, Title: "A"}
	b := session{StartedBy: 2, ChannelID: 20, Title: "B"}
	store.start(100, a)
	store.start(200, b)

	all := store.all()
	if len(all) != 2 || all[100] != a || all[200] != b {
		t.Fatalf("got %+v, want both sessions", all)
	}

	// Changing the copy mustn't touch the store.
	delete(all, 100)
	if _, running := store.get(100); !running {
		t.Error("all should return a copy, not the store's own map")
	}
}
