package startup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A made-up token in the right shape (not a real one).
const testToken = "MTIzNDU2Nzg5MDEyMzQ1Njc4.GabcDe.abcdefghijklmnopqrstuvwxyz_-0123456789"

func TestAppDirIsNextToTheProgramInTheDownload(t *testing.T) {
	unzipped := t.TempDir()
	if err := os.WriteFile(filepath.Join(unzipped, "HOW-TO-RUN.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Double-clicked or started from a shortcut: the current folder is
	// somewhere else.
	got := AppDir(filepath.Join(unzipped, "discord-live-captions"), t.TempDir(), "HOW-TO-RUN.txt")
	if got != unzipped {
		t.Errorf("got %q, want the program's folder %q", got, unzipped)
	}
}

func TestAppDirIsTheCurrentFolderForGoRun(t *testing.T) {
	// go run builds into a temporary folder, with nothing else in it.
	buildDir := t.TempDir()
	project := t.TempDir()

	if got := AppDir(filepath.Join(buildDir, "livetranscribebot"), project, "HOW-TO-RUN.txt"); got != project {
		t.Errorf("got %q, want the current folder %q", got, project)
	}
}

func TestTokenFromTheEnvironment(t *testing.T) {
	t.Setenv(tokenKey, " "+testToken+"\n")
	dir := t.TempDir()

	token, err := Token(dir, strings.NewReader(""), &strings.Builder{})
	if err != nil || token != testToken {
		t.Fatalf("got (%q, %v), want the environment's token", token, err)
	}
	if _, err := os.Stat(filepath.Join(dir, EnvFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a token from the environment shouldn't be saved anywhere")
	}
}

func TestTokenFromTheEnvFile(t *testing.T) {
	t.Setenv(tokenKey, "")
	dir := t.TempDir()
	writeEnv(t, dir, "BOT_TOKEN="+testToken+"\n")
	var out strings.Builder

	token, err := Token(dir, strings.NewReader(""), &out)
	if err != nil || token != testToken {
		t.Fatalf("got (%q, %v), want the saved token", token, err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be asked, printed %q", out.String())
	}
}

func TestTokenIsAskedForAndSavedTheFirstTime(t *testing.T) {
	t.Setenv(tokenKey, "")
	dir := t.TempDir()
	var out strings.Builder

	token, err := Token(dir, strings.NewReader("  "+testToken+"  \r\n"), &out)
	if err != nil || token != testToken {
		t.Fatalf("got (%q, %v), want the pasted token", token, err)
	}
	if !strings.Contains(out.String(), "Reset Token") || !strings.Contains(out.String(), "Saved the token") {
		t.Errorf("should explain where to get the token and say it's saved, printed:\n%s", out.String())
	}

	// The next run finds it without asking.
	again, err := Token(dir, strings.NewReader(""), &strings.Builder{})
	if err != nil || again != testToken {
		t.Fatalf("the saved token wasn't found: (%q, %v)", again, err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, EnvFileName))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf(".env is %v, want only its owner able to read it (0600)", info.Mode().Perm())
		}
	}
}

func TestTokenAsksAgainUntilItLooksRight(t *testing.T) {
	t.Setenv(tokenKey, "")
	var out strings.Builder
	input := strings.Join([]string{
		"",                      // just Enter
		"1234567890123456789",   // the application ID
		"aBcDeFgHiJkLmNoPqRsTu", // the client secret
		`"` + testToken + `"`,   // in quotes
		"a..b",                  // an empty part
		testToken,
	}, "\n")

	token, err := Token(t.TempDir(), strings.NewReader(input), &out)
	if err != nil || token != testToken {
		t.Fatalf("got (%q, %v), want the last, right-looking token", token, err)
	}
	if n := strings.Count(out.String(), "That isn't a bot token"); n != 5 {
		t.Errorf("want 5 refusals, got %d:\n%s", n, out.String())
	}
}

func TestTokenKeepsWhatElseIsInTheEnvFile(t *testing.T) {
	t.Setenv(tokenKey, "")
	dir := t.TempDir()
	writeEnv(t, dir, "OTHER_SETTING=kept\n")
	if err := os.Chmod(filepath.Join(dir, EnvFileName), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Token(dir, strings.NewReader(testToken+"\n"), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}

	saved, err := os.ReadFile(filepath.Join(dir, EnvFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `OTHER_SETTING="kept"`) || !strings.Contains(string(saved), testToken) {
		t.Errorf(".env should keep its other setting and gain the token, got:\n%s", saved)
	}
	if info, _ := os.Stat(filepath.Join(dir, EnvFileName)); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("an existing .env should be made private too, got %v", info.Mode().Perm())
	}
}

func TestTokenWhenNothingIsEntered(t *testing.T) {
	t.Setenv(tokenKey, "")
	dir := t.TempDir()

	_, err := Token(dir, strings.NewReader("not a token\n"), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "no bot token was entered") {
		t.Fatalf("got %v, want an error saying no token was entered", err)
	}
	if _, err := os.Stat(filepath.Join(dir, EnvFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("nothing should be saved")
	}
}

func TestTokenWhenTheInputBreaks(t *testing.T) {
	t.Setenv(tokenKey, "")
	_, err := Token(t.TempDir(), brokenReader{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "reading the token") {
		t.Fatalf("got %v, want the read error", err)
	}
}

func TestTokenWhenTheEnvFileCantBeRead(t *testing.T) {
	t.Setenv(tokenKey, "")
	dir := t.TempDir()
	// A folder where the file should be.
	if err := os.Mkdir(filepath.Join(dir, EnvFileName), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Token(dir, strings.NewReader(testToken+"\n"), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "reading") {
		t.Fatalf("got %v, want a read error", err)
	}
}

func TestTokenWhenItCantBeSaved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only folders don't stop new files on Windows")
	}
	t.Setenv(tokenKey, "")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	_, err := Token(dir, strings.NewReader(testToken+"\n"), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "saving the token") {
		t.Fatalf("got %v, want a save error", err)
	}
}

func writeEnv(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, EnvFileName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("the console went away") }
