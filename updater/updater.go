// Package updater keeps a downloaded copy of the bot up to date: it checks
// GitHub for a newer release and swaps the program and its libraries for the
// new ones. The speech model, settings.json and .env aren't in a release, so
// they're never touched.
package updater

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// repository is where releases are published: a public repository of its
// own, so the code can stay private while running bots can still read it.
const repository = "Nat-Umeni/discord-transcription-releases"

// Variables rather than constants so tests can point them at a local server.
var (
	releaseAPIURL   = "https://api.github.com/repos/" + repository + "/releases/latest"
	downloadBaseURL = "https://github.com/" + repository + "/releases/download/"
)

// rename is os.Rename, swapped in tests to make one particular rename fail.
var rename = os.Rename

// The platform this is running on. Variables so a test can be somewhere no
// release is built for.
var goos, goarch = runtime.GOOS, runtime.GOARCH

// checksumsFileName is what the release workflow publishes beside the zips.
const checksumsFileName = "checksums.txt"

// zipFolder is the folder everything in a release zip is in, so unzipping it
// makes one tidy folder. An update installs what's inside it.
const zipFolder = "discord-live-captions/"

// maxDownloadBytes caps what a download may read. The zips are tens of MB
// (the program and its libraries; the model is downloaded separately); the
// cap only stops a broken response from filling memory. A variable so a test
// can lower it.
var maxDownloadBytes = 500 << 20

// checkClient is short because the check runs on every start: being offline
// should cost a few seconds, not hold the bot up.
var checkClient = &http.Client{Timeout: 10 * time.Second}

// downloadClient is generous because it fetches the program and its
// libraries over whatever connection the host has.
var downloadClient = &http.Client{Timeout: 10 * time.Minute}

// assetName is the zip the release workflow publishes for a platform: the
// same one people download to install it. It has to match the names in the
// build scripts exactly, and a test holds it to that.
func assetName(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "windows/amd64":
		return "discord-live-captions-windows-x64.zip", nil
	case "linux/amd64":
		return "discord-live-captions-linux-x64.zip", nil
	}
	return "", fmt.Errorf("no release is built for %s/%s", goos, goarch)
}

// parseVersion reads a release tag like v0.2.0 into its three numbers.
//
// Anything else is rejected on purpose: "dev" is a plain `go build`, and
// git describe gives things like "v0.1.0-3-gabc123" for a local build. None
// of those should ever be updated over the top of.
func parseVersion(version string) ([3]int, bool) {
	var parts [3]int

	trimmed, hasPrefix := strings.CutPrefix(version, "v")
	if !hasPrefix {
		return parts, false
	}

	fields := strings.Split(trimmed, ".")
	if len(fields) != len(parts) {
		return parts, false
	}

	for index, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil || number < 0 {
			return parts, false
		}
		parts[index] = number
	}

	return parts, true
}

// IsRelease reports whether version is a tagged release build. Only those
// update themselves, so running from the code never touches the network.
func IsRelease(version string) bool {
	_, ok := parseVersion(version)
	return ok
}

// IsNewer reports whether latest is a later release than current. Compared as
// numbers, not strings: as strings, v0.10.0 sorts before v0.9.0.
func IsNewer(current, latest string) bool {
	currentParts, ok := parseVersion(current)
	if !ok {
		return false
	}

	latestParts, ok := parseVersion(latest)
	if !ok {
		return false
	}

	// Element by element, stopping at the first difference: major, then
	// minor, then patch.
	return slices.Compare(latestParts[:], currentParts[:]) > 0
}

// LatestVersion asks GitHub for the newest published release's tag. Drafts
// and prereleases are never returned, so tagging something as a prerelease
// publishes it without updating anyone to it.
func LatestVersion() (string, error) {
	request, err := http.NewRequest(http.MethodGet, releaseAPIURL, nil)
	if err != nil {
		return "", err
	}

	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "discord-live-captions")

	response, err := checkClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	// A repository with no releases yet answers 404.
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered HTTP %s", response.Status)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("reading the release: %w", err)
	}

	if release.TagName == "" {
		return "", errors.New("the latest release has no tag")
	}

	return release.TagName, nil
}

// Update installs the newest release over this one if it's newer, and says
// which version it installed. Restarting into it is up to the caller.
//
// Every failure is logged and leaves this version as it was: being offline
// or GitHub being down must never stop the bot.
func Update(current, appDir string) (string, bool) {
	if !IsRelease(current) {
		return "", false
	}

	latest, err := LatestVersion()
	if err != nil {
		log.Printf("couldn't check for updates: %v", err)
		return "", false
	}
	if !IsNewer(current, latest) {
		return "", false
	}

	log.Printf("updating from %s to %s…", current, latest)
	if err := Apply(latest, appDir); err != nil {
		log.Printf("couldn't install %s, carrying on with %s: %v", latest, current, err)
		return "", false
	}
	log.Printf("installed %s", latest)
	return latest, true
}

// Apply downloads the given release's zip for this platform, checks it
// against the release's checksums.txt, and swaps its files in over the ones
// in appDir. Running programs keep running the old files, so the new version
// takes effect when the bot restarts.
func Apply(tag, appDir string) error {
	asset, err := assetName(goos, goarch)
	if err != nil {
		return err
	}

	// The tag's own URL, not releases/latest/download: a release published
	// between the check and this download would otherwise install a version
	// other than the one that was checked.
	base := downloadBaseURL + tag + "/"

	// Checksums first: it's tiny, and a release without one fails here before
	// downloading a zip that would be refused anyway.
	sums, err := download(base + checksumsFileName)
	if err != nil {
		return err
	}

	want, err := checksumFor(sums, asset)
	if err != nil {
		return err
	}

	archive, err := download(base + asset)
	if err != nil {
		return err
	}

	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s doesn't match its checksum, so it wasn't installed", asset)
	}

	// Everything is read and checked before the first file is touched.
	files, err := readArchive(archive)
	if err != nil {
		return err
	}

	return install(appDir, files)
}

// CleanupOld removes the previous versions an earlier update left behind.
// Windows can't delete a program or library while it's running, so an update
// only moves them aside (as name.old), and this clears them on the next start.
func CleanupOld(appDir string) {
	// Failures are harmless: the file just sits there until the next start.
	_ = filepath.WalkDir(appDir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() && strings.HasSuffix(path, ".old") {
			_ = os.Remove(path)
		}
		return nil
	})
}

// download fetches url into memory, treating anything but a 200 as a failure.
func download(url string) ([]byte, error) {
	response, err := downloadClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: HTTP %s", url, response.Status)
	}

	// One byte over the cap, so a body of exactly the cap is still accepted
	// and anything larger is detectable.
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maxDownloadBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}

	if len(body) > maxDownloadBytes {
		return nil, fmt.Errorf("downloading %s: larger than %d MB, refusing it", url, maxDownloadBytes>>20)
	}

	return body, nil
}

// checksumFor finds name's hash in sha256sum output. Each line is the hash,
// then the file name: separated by two spaces in text mode, or " *" in binary
// mode, so the asterisk is stripped rather than relied on either way.
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		// Fields also eats a trailing \r, should the file ever pass through
		// something that adds Windows line endings.
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		if strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}

	return "", fmt.Errorf("%s has no entry for %s", checksumsFileName, name)
}

// newFile is one file from a release zip.
type newFile struct {
	name     string // its path inside the program's folder, folders separated by /
	contents []byte
	mode     fs.FileMode
}

// readArchive reads every file out of a release zip's folder. A file outside
// that folder, one that would land outside the program's folder, or one that
// isn't a plain file refuses the whole update.
func readArchive(archive []byte) ([]newFile, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("opening the update: %w", err)
	}

	var files []newFile
	for _, entry := range reader.File {
		if entry.Mode().IsDir() {
			continue
		}
		name, inFolder := strings.CutPrefix(entry.Name, zipFolder)
		// Backslashes are refused everywhere, because Windows reads them as
		// folder separators ("..\..\x").
		if !inFolder || !filepath.IsLocal(name) || strings.Contains(name, `\`) {
			return nil, fmt.Errorf("the update holds %q, which is outside the program's folder", entry.Name)
		}
		if !entry.Mode().IsRegular() {
			return nil, fmt.Errorf("the update holds %q, which isn't a plain file", entry.Name)
		}

		contents, err := readEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("reading %s from the update: %w", entry.Name, err)
		}
		files = append(files, newFile{name: name, contents: contents, mode: entry.Mode()})
	}

	if len(files) == 0 {
		return nil, errors.New("the update has no files in it")
	}
	return files, nil
}

func readEntry(entry *zip.File) ([]byte, error) {
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// swapped is a file an update has put in place, and how to undo it.
type swapped struct {
	target string
	hadOld bool // the previous version is at target + ".old"
}

// install swaps every file in. If any one fails, the ones already swapped are
// put back, so the folder is never left half old and half new: the program
// could fail to start with mismatched libraries, and a program that won't
// start can't update itself out of it.
func install(appDir string, files []newFile) error {
	var done []swapped
	for _, file := range files {
		target := filepath.Join(appDir, filepath.FromSlash(file.name))
		hadOld, err := swapIn(target, file)
		if err != nil {
			if undoErr := undo(done); undoErr != nil {
				return fmt.Errorf("installing %s: %w (and putting the previous files back failed: %v)", file.name, err, undoErr)
			}
			return fmt.Errorf("installing %s: %w", file.name, err)
		}
		done = append(done, swapped{target: target, hadOld: hadOld})
	}

	// Linux lets the old versions go now. Windows refuses for the ones this
	// program is running from, which is what CleanupOld is for.
	for _, file := range done {
		if file.hadOld {
			_ = os.Remove(file.target + ".old")
		}
	}
	return nil
}

// swapIn puts one new file at target, moving any previous version aside to
// target.old. It says whether there was a previous version.
func swapIn(target string, file newFile) (bool, error) {
	// A file that's already there keeps its permissions, so the program stays
	// runnable whatever the zip says. A new one takes the zip's.
	perm := file.mode.Perm()
	hadOld := false
	info, err := os.Stat(target)
	if err == nil {
		perm = info.Mode().Perm()
		hadOld = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}

	// Beside the target rather than in a temp folder, because a file can't be
	// renamed onto another drive.
	newPath := target + ".new"
	oldPath := target + ".old"

	// A .new left by an earlier failed attempt would keep its own permissions
	// through WriteFile. Once the swap succeeds there's nothing left to remove.
	_ = os.Remove(newPath)
	defer os.Remove(newPath)
	if err := os.WriteFile(newPath, file.contents, perm); err != nil {
		return false, fmt.Errorf("writing the new version: %w", err)
	}

	// Windows won't overwrite a running program or library, but it will
	// rename one, so the old version is moved aside rather than replaced. The
	// same steps on every platform, which lets a Linux test cover Windows.
	if hadOld {
		_ = os.Remove(oldPath)
		if err := rename(target, oldPath); err != nil {
			return false, fmt.Errorf("moving the old version aside: %w", err)
		}
	}

	if err := rename(newPath, target); err != nil {
		if hadOld {
			if restoreErr := rename(oldPath, target); restoreErr != nil {
				return false, fmt.Errorf("putting the new version in place: %w (and putting the old one back failed too (%v), it's at %s)", err, restoreErr, oldPath)
			}
		}
		return false, fmt.Errorf("putting the new version in place: %w", err)
	}
	return hadOld, nil
}

// undo puts back the files an update had already swapped in, newest first.
func undo(done []swapped) error {
	var errs []error
	for i := len(done) - 1; i >= 0; i-- {
		file := done[i]
		var err error
		if file.hadOld {
			err = rename(file.target+".old", file.target)
		} else {
			err = os.Remove(file.target)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
