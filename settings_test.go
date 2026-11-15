package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func TestLoadSettingsMissingFileStartsEmpty(t *testing.T) {
	store, err := loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatalf("a missing file should not be an error, got %v", err)
	}
	if _, ok := store.NoteTakerRole(1); ok {
		t.Fatal("a fresh store should have no note-taker role")
	}
}

func TestSettingsSurviveSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	// real-sized Discord IDs, so a precision bug (e.g. going through float64) would show
	server, role := snowflake.ID(1171143553743200358), snowflake.ID(1271143553743200999)
	if err := store.SetNoteTakerRole(server, role); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("SetNoteTakerRole should have written the file: %v", err)
	}
	if !strings.Contains(string(data), `"noteTakerRoleID": "1271143553743200999"`) {
		t.Errorf("file should hold the role ID as a string, got:\n%s", data)
	}

	reloaded, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.NoteTakerRole(server)
	if !ok || got != role {
		t.Fatalf("after reload got (%v, %v), want (%v, true)", got, ok, role)
	}
	if _, ok := reloaded.NoteTakerRole(999); ok {
		t.Error("a server that was never set up should have no role")
	}
}

func TestSetNoteTakerRoleReplacesTheOldRole(t *testing.T) {
	store, _ := loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	_ = store.SetNoteTakerRole(1, 10)
	_ = store.SetNoteTakerRole(1, 20)

	got, _ := store.NoteTakerRole(1)
	if got != 20 {
		t.Fatalf("got role %v, want the newer role 20", got)
	}
}

func TestLoadSettingsReadError(t *testing.T) {
	// a directory exists but can't be read as a file, which isn't "file missing"
	if _, err := loadSettings(t.TempDir()); err == nil {
		t.Fatal("reading a directory as the settings file should fail")
	}
}

func TestLoadSettingsBrokenJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadSettings(path)
	if err == nil {
		t.Fatal("broken JSON should be an error, not silently start empty")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the file so it's easy to find, got %v", err)
	}
}

func TestSetNoteTakerRoleWriteError(t *testing.T) {
	// the folder doesn't exist, so the file can't be written
	store, _ := loadSettings(filepath.Join(t.TempDir(), "missing-folder", "settings.json"))
	if err := store.SetNoteTakerRole(1, 2); err == nil {
		t.Fatal("expected an error when the file can't be written")
	}
}

// Commands run concurrently (middleware.Go). Run with -race: without the mutex
// this fails with "concurrent map writes" or a data race report.
func TestSettingsConcurrentAccess(t *testing.T) {
	store, _ := loadSettings(filepath.Join(t.TempDir(), "settings.json"))

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.SetNoteTakerRole(snowflake.ID(i), 1)
		}()
		go func() {
			defer wg.Done()
			store.NoteTakerRole(snowflake.ID(i))
		}()
	}
	wg.Wait()
}

func TestCaptionsChannelSurvivesSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	server, channel := snowflake.ID(1171143553743200358), snowflake.ID(1371143553743200111)
	if _, ok := store.CaptionsChannel(server); ok {
		t.Error("a server that was never set up should have no captions channel")
	}
	if err := store.SetCaptionsChannel(server, channel); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"captionsChannelID": "1371143553743200111"`) {
		t.Errorf("file should hold the channel ID as a string, got:\n%s", data)
	}
	if strings.Contains(string(data), "noteTakerRoleID") {
		t.Errorf("an unset role should be left out of the file, got:\n%s", data)
	}

	reloaded, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.CaptionsChannel(server); !ok || got != channel {
		t.Fatalf("after reload got (%v, %v), want (%v, true)", got, ok, channel)
	}
	// The server has an entry now, but only a channel: no role.
	if _, ok := reloaded.NoteTakerRole(server); ok {
		t.Error("saving only a channel shouldn't make a note-taker role appear")
	}
}

// Each setter must only change its own setting. Replacing the whole entry
// would quietly wipe the other one.
func TestSettingOneThingKeepsTheOther(t *testing.T) {
	store, err := loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	const server, role, channel snowflake.ID = 1, 2, 3

	if err := store.SetNoteTakerRole(server, role); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCaptionsChannel(server, channel); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.NoteTakerRole(server); !ok || got != role {
		t.Errorf("setting the channel wiped the role: got (%v, %v)", got, ok)
	}

	if err := store.SetNoteTakerRole(server, 4); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.CaptionsChannel(server); !ok || got != channel {
		t.Errorf("setting the role wiped the channel: got (%v, %v)", got, ok)
	}
}

func TestSetCaptionsChannelWriteError(t *testing.T) {
	store, err := loadSettings(filepath.Join(t.TempDir(), "missing-folder", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCaptionsChannel(1, 2); err == nil {
		t.Fatal("expected an error when the settings file can't be written")
	}
}

func TestNoCaptionsRoleSurvivesSaveAndReloadAndKeepsTheOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	const server, noteTakers, channel, noCaptions snowflake.ID = 1, 2, 3, 4

	if _, ok := store.NoCaptionsRole(server); ok {
		t.Error("a server that was never set up should have no no-captions role")
	}
	if err := store.SetNoteTakerRole(server, noteTakers); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCaptionsChannel(server, channel); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNoCaptionsRole(server, noCaptions); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.NoCaptionsRole(server); !ok || got != noCaptions {
		t.Errorf("after reload got (%v, %v), want (%v, true)", got, ok, noCaptions)
	}
	if got, _ := reloaded.NoteTakerRole(server); got != noteTakers {
		t.Errorf("setting the no-captions role wiped the note-taker role, got %v", got)
	}
	if got, _ := reloaded.CaptionsChannel(server); got != channel {
		t.Errorf("setting the no-captions role wiped the captions channel, got %v", got)
	}
}

func TestSetNoCaptionsRoleWriteError(t *testing.T) {
	store, err := loadSettings(filepath.Join(t.TempDir(), "missing-folder", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetNoCaptionsRole(1, 2); err == nil {
		t.Fatal("expected an error when the settings file can't be written")
	}
}
