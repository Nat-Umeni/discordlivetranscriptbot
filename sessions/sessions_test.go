package sessions

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func TestSessionStartGetRemove(t *testing.T) {
	store := New()

	if _, running := store.Get(1); running {
		t.Fatal("a new store should have no sessions")
	}

	first := Session{StartedBy: 10, ChannelID: 20, Title: "Weekly"}
	got, claimed := store.Start(1, first)
	if !claimed || got != first {
		t.Fatalf("first start: got (%+v, %v), want (%+v, true)", got, claimed, first)
	}

	current, running := store.Get(1)
	if !running || current != first {
		t.Fatalf("get: got (%+v, %v), want (%+v, true)", current, running, first)
	}

	store.Remove(1)
	if _, running := store.Get(1); running {
		t.Fatal("Session should be gone after remove")
	}
}

func TestSessionSecondStartGetsTheExistingOne(t *testing.T) {
	store := New()
	first := Session{StartedBy: 10, Title: "first"}
	store.Start(1, first)

	got, claimed := store.Start(1, Session{StartedBy: 11, Title: "second"})
	if claimed {
		t.Fatal("a second start in the same server should not claim it")
	}
	if got != first {
		t.Fatalf("should get the running Session back, got %+v", got)
	}

	current, _ := store.Get(1)
	if current != first {
		t.Fatalf("the running Session should be unchanged, got %+v", current)
	}
}

func TestSessionsAreSeparatePerServer(t *testing.T) {
	store := New()
	store.Start(1, Session{Title: "server one"})

	if _, claimed := store.Start(2, Session{Title: "server two"}); !claimed {
		t.Fatal("a Session in one server should not block another server")
	}
}

func TestSessionRemoveWhenNothingRunningIsHarmless(t *testing.T) {
	store := New()
	store.Remove(1) // stop and the voice-leave listener can both remove; must not panic
}

// The reason start checks and records in one locked step: out of many
// simultaneous starts, exactly one may win. Run with -race.
func TestSessionOnlyOneOfManySimultaneousStartsWins(t *testing.T) {
	store := New()

	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, claimed := store.Start(1, Session{StartedBy: snowflake.ID(i)}); claimed {
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
	store := New()
	a := Session{StartedBy: 1, ChannelID: 10, Title: "A"}
	b := Session{StartedBy: 2, ChannelID: 20, Title: "B"}
	store.Start(100, a)
	store.Start(200, b)

	all := store.All()
	if len(all) != 2 || all[100] != a || all[200] != b {
		t.Fatalf("got %+v, want both sessions", all)
	}

	// Changing the copy mustn't touch the store.
	delete(all, 100)
	if _, running := store.Get(100); !running {
		t.Error("all should return a copy, not the store's own map")
	}
}
