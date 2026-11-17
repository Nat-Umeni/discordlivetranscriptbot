// Package startup finds the folder the bot keeps its files in and gets its
// Discord token, asking for it the first time the bot runs.
package startup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

const (
	tokenKey = "BOT_TOKEN"

	// EnvFileName is where the token is saved, next to the program.
	EnvFileName = ".env"
)

// tokenHelp is shown before asking for the token, the first time the bot runs.
const tokenHelp = `
This is the first time the bot has run here, so it needs its Discord bot token.
Get it from https://discord.com/developers/applications: pick the bot's
application, go to Bot, press Reset Token and copy the token it shows.
It's saved in the .env file next to this program. Keep that file private:
anyone with the token can control the bot.

`

// AppDir is the folder the bot keeps its files in: the speech model,
// settings.json and .env.
//
// The unzipped download is recognised by marker, a file that's always in it,
// next to the program: then it's that folder, found from the program's own
// location, because the program can be started from anywhere (a shortcut, or
// a terminal open in another folder). Otherwise, e.g. `go run .`, which
// builds into a temporary folder, it's the current folder: the project.
func AppDir(executable, workingDir, marker string) string {
	dir := filepath.Dir(executable)
	if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
		return dir
	}
	return workingDir
}

// Token is the bot's Discord token: from the BOT_TOKEN environment variable,
// else from the .env file in dir, else asked for on in/out and saved to .env
// so it's only asked for once.
func Token(dir string, in io.Reader, out io.Writer) (string, error) {
	if token := strings.TrimSpace(os.Getenv(tokenKey)); token != "" {
		return token, nil
	}

	path := filepath.Join(dir, EnvFileName)
	saved, err := godotenv.Read(path)
	if errors.Is(err, fs.ErrNotExist) {
		saved = map[string]string{}
	} else if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	if token := strings.TrimSpace(saved[tokenKey]); token != "" {
		return token, nil
	}

	token, err := askForToken(in, out)
	if err != nil {
		return "", err
	}

	// Anything else already in .env is kept.
	saved[tokenKey] = token
	if err := saveEnv(path, saved); err != nil {
		return "", fmt.Errorf("saving the token to %s: %w", path, err)
	}
	fmt.Fprintf(out, "Saved the token in %s.\n\n", path)
	return token, nil
}

// askForToken asks until it gets something shaped like a bot token, or the
// input ends.
func askForToken(in io.Reader, out io.Writer) (string, error) {
	fmt.Fprint(out, tokenHelp)
	lines := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "Paste the bot token here and press Enter: ")
		if !lines.Scan() {
			fmt.Fprintln(out) // so what's printed next starts on its own line
			if err := lines.Err(); err != nil {
				return "", fmt.Errorf("reading the token: %w", err)
			}
			return "", errors.New("no bot token was entered")
		}
		token := strings.TrimSpace(lines.Text())
		if looksLikeToken(token) {
			return token, nil
		}
		fmt.Fprintln(out, "That isn't a bot token. It's three parts joined by dots, from Bot → Reset Token (not the application ID or the client secret).")
	}
}

// looksLikeToken checks the shape of a bot token: three parts of letters,
// digits, - and _, joined by dots. It catches pasting the application ID or
// the client secret (no dots), or the token in quotes. Whether Discord
// accepts it is only known when the bot connects.
func looksLikeToken(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			isTokenCharacter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
			if !isTokenCharacter {
				return false
			}
		}
	}
	return true
}

// saveEnv writes .env readable by its owner only: the token controls the bot.
func saveEnv(path string, values map[string]string) error {
	// Marshal never fails: its error is always nil.
	text, _ := godotenv.Marshal(values)
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		return err
	}
	// WriteFile only sets the permissions when it creates the file, and .env
	// may already have been there (holding other settings).
	return os.Chmod(path, 0o600)
}
