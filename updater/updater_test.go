package updater

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------- helpers ----------

// zipEntry is one file to put in a test zip.
type zipEntry struct {
	name     string
	contents string
	mode     fs.FileMode
	outside  bool // not inside the zip's hopamine-captions/ folder
}

// makeZip builds a zip the way the build scripts do (everything inside a
// hopamine-captions/ folder), uncompressed so tests can find and change a
// file's bytes inside it.
func makeZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, entry := range entries {
		name := zipFolder + entry.name
		if entry.outside {
			name = entry.name
		}
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// fakeRelease serves files the way GitHub lays a release out (/<tag>/<name>)
// and points downloadBaseURL at it. Anything not in files is a 404, which is
// what GitHub gives for an asset a release doesn't have.
func fakeRelease(t *testing.T, tag string, files map[string][]byte) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, underTag := strings.CutPrefix(r.URL.Path, "/"+tag+"/")
		body, exists := files[name]
		if !underTag || !exists {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	swap(t, &downloadBaseURL, server.URL+"/")
}

// releaseOf serves archive as this platform's update zip, with a matching
// checksums.txt.
func releaseOf(t *testing.T, tag string, archive []byte) {
	t.Helper()
	asset := thisPlatformsAsset(t)
	fakeRelease(t, tag, map[string][]byte{asset: archive, checksumsFileName: sumsLine(asset, archive)})
}

// fakeLatest makes GitHub's "latest release" answer tag, and counts the
// questions.
func fakeLatest(t *testing.T, status int, body string) *atomic.Int32 {
	t.Helper()
	var asked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	swap(t, &releaseAPIURL, server.URL)
	return &asked
}

func swap[T any](t *testing.T, variable *T, value T) {
	t.Helper()
	original := *variable
	*variable = value
	t.Cleanup(func() { *variable = original })
}

// sumsLine is one line of sha256sum output for contents.
func sumsLine(name string, contents []byte) []byte {
	sum := sha256.Sum256(contents)
	return []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
}

// thisPlatformsAsset is what Apply asks for on the machine running the test.
func thisPlatformsAsset(t *testing.T) string {
	t.Helper()
	name, err := assetName(goos, goarch)
	if err != nil {
		t.Skipf("no release is built for %s/%s", goos, goarch)
	}
	return name
}

// installedApp is an unzipped download: the program, a library, the model,
// and the files the host made (settings and token), which updates never touch.
func installedApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"hopamine-captions":  "old program",
		"libdave.so":         "old library",
		"settings.json":      "the host's settings",
		".env":               "BOT_TOKEN=secret",
		"models/model.onnx":  "the model",
		"HOW-TO-RUN.txt":     "old instructions",
		"docs/old-notes.txt": "kept",
	} {
		write(t, filepath.Join(dir, name), contents, 0o644)
	}
	if err := os.Chmod(filepath.Join(dir, "hopamine-captions"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, contents string, perm fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), perm); err != nil {
		t.Fatal(err)
	}
}

// snapshot is every file in a folder and what it holds.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(dir, path)
		files[filepath.ToSlash(relative)] = string(contents)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func wantUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	if after := snapshot(t, dir); !maps.Equal(before, after) {
		t.Errorf("the folder changed:\nbefore %v\nafter  %v", before, after)
	}
}

// failRenames makes renames for which fails returns true fail.
func failRenames(t *testing.T, fails func(from, to string) bool) {
	t.Helper()
	swap(t, &rename, func(from, to string) error {
		if fails(from, to) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("refused by the test")}
		}
		return os.Rename(from, to)
	})
}

func captureLog(t *testing.T) *strings.Builder {
	t.Helper()
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

// ---------- names and versions ----------

func TestAssetName(t *testing.T) {
	for _, test := range []struct {
		goos, arch, want string
	}{
		{"windows", "amd64", "hopamine-captions-windows-x64.zip"},
		{"linux", "amd64", "hopamine-captions-linux-x64.zip"},
		{"darwin", "arm64", ""},
		{"linux", "arm64", ""},
		{"windows", "386", ""},
	} {
		got, err := assetName(test.goos, test.arch)
		if test.want == "" {
			if err == nil {
				t.Errorf("%s/%s: got %q, want an error (nothing is built for it)", test.goos, test.arch, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("%s/%s: got (%q, %v), want %q", test.goos, test.arch, got, err, test.want)
		}
	}
}

// The names are written twice: in the build scripts, which make the zips, and
// here, which downloads them. Renaming one without the other would pass every
// other test and then 404 for everyone on the next release.
func TestAssetNamesMatchTheBuildScripts(t *testing.T) {
	for script, platform := range map[string][2]string{
		"../build/package-linux.sh":   {"linux", "amd64"},
		"../build/package-windows.sh": {"windows", "amd64"},
	} {
		contents, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("reading %s: %v", script, err)
		}
		name, _ := assetName(platform[0], platform[1])
		if !strings.Contains(string(contents), name) {
			t.Errorf("%s never makes %s, so updating on %s would 404", script, name, platform[0])
		}
	}
}

// The release workflow publishes where running bots look for updates.
// Changing one without the other would leave every bot on its old version.
func TestReleasesArePublishedWhereUpdatesAreRead(t *testing.T) {
	workflow, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if want := "RELEASES_REPO: " + repository; !strings.Contains(string(workflow), want) {
		t.Errorf("release.yml should publish to %s (have it say %q)", repository, want)
	}
}

func TestIsNewer(t *testing.T) {
	for _, test := range []struct {
		name, current, latest string
		want                  bool
	}{
		{"patch bump", "v0.1.0", "v0.1.1", true},
		{"minor bump", "v0.1.0", "v0.2.0", true},
		{"major bump", "v0.9.9", "v1.0.0", true},
		{"same version", "v0.2.0", "v0.2.0", false},
		{"latest is older", "v0.3.0", "v0.2.0", false},
		// The case a string comparison gets wrong: "1" sorts before "9".
		{"numbers, not alphabetical", "v0.9.0", "v0.10.0", true},
		{"numbers, not alphabetical, reversed", "v0.10.0", "v0.9.0", false},
		// Local builds are never updated over.
		{"dev build", "dev", "v0.2.0", false},
		{"commits past a tag", "v0.1.0-3-gabc123", "v0.2.0", false},
		// A strange tag on GitHub's side is ignored rather than installed.
		{"unreadable latest", "v0.1.0", "latest", false},
		{"two-part latest", "v0.1.0", "v0.2", false},
	} {
		if got := IsNewer(test.current, test.latest); got != test.want {
			t.Errorf("%s: IsNewer(%q, %q) = %v, want %v", test.name, test.current, test.latest, got, test.want)
		}
	}
}

func TestIsRelease(t *testing.T) {
	for version, want := range map[string]bool{
		"v0.1.0":        true,
		"v12.34.56":     true,
		"dev":           false,
		"0.1.0":         false, // releases are only built for v* tags
		"v0.1":          false,
		"v0.1.0.0":      false,
		"v0.1.0-rc1":    false,
		"v0.-1.0":       false,
		"55b099c-dirty": false,
		"":              false,
	} {
		if got := IsRelease(version); got != want {
			t.Errorf("IsRelease(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestLatestVersion(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"a release", `{"tag_name":"v0.2.0","name":"v0.2.0","draft":false}`, http.StatusOK, "v0.2.0"},
		// What GitHub gives a private repository, or one with no releases.
		{"not found", `{"message":"Not Found"}`, http.StatusNotFound, ""},
		{"rate limited", `{"message":"API rate limit exceeded"}`, http.StatusForbidden, ""},
		{"not json", "<html>", http.StatusOK, ""},
		{"empty tag", `{"tag_name":""}`, http.StatusOK, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeLatest(t, test.status, test.body)
			got, err := LatestVersion()
			if test.want == "" {
				if err == nil {
					t.Errorf("got %q, want an error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Errorf("got (%q, %v), want %q", got, err, test.want)
			}
		})
	}
}

func TestLatestVersionWhenGitHubCantBeReached(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close() // nothing listening any more
	swap(t, &releaseAPIURL, server.URL)
	if _, err := LatestVersion(); err == nil {
		t.Error("want an error when GitHub can't be reached")
	}

	swap(t, &releaseAPIURL, "://not a url")
	if _, err := LatestVersion(); err == nil {
		t.Error("want an error for a broken address")
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte(
		"aaaa  hopamine-captions-linux-x64.zip.sig\n" +
			"BBBB *hopamine-captions-windows-x64.zip\r\n" +
			"cccc  hopamine-captions-linux-x64.zip\n" +
			"\n" +
			"not a checksum line at all\n")

	for _, test := range []struct {
		name, asset, want string
	}{
		{"text mode, two spaces", "hopamine-captions-linux-x64.zip.sig", "aaaa"},
		// sha256sum -b writes " *" before the name. Lowercased because that's
		// how Apply writes its own hash.
		{"binary mode and CRLF", "hopamine-captions-windows-x64.zip", "bbbb"},
		// The zip's name is the start of the line above's: matching on a
		// prefix would return the wrong hash.
		{"exact name, not prefix", "hopamine-captions-linux-x64.zip", "cccc"},
		{"missing", "hopamine-captions-macos.zip", ""},
	} {
		got, err := checksumFor(sums, test.asset)
		if test.want == "" {
			if err == nil {
				t.Errorf("%s: got %q, want an error", test.name, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("%s: got (%q, %v), want %q", test.name, got, err, test.want)
		}
	}
}

// ---------- installing ----------

func TestApplySwapsTheFilesIn(t *testing.T) {
	dir := installedApp(t)
	releaseOf(t, "v9.9.9", makeZip(t,
		// The zip says 0644, but the program keeps its 0755 so it still runs.
		zipEntry{name: "hopamine-captions", contents: "new program", mode: 0o644},
		zipEntry{name: "libdave.so", contents: "new library"},
		zipEntry{name: "libnew.so", contents: "a library this version added", mode: 0o640},
		zipEntry{name: "HOW-TO-RUN.txt", contents: "new instructions"},
		zipEntry{name: "hopamine-captions/", mode: fs.ModeDir | 0o755, outside: true}, // as zip -r makes it
		zipEntry{name: "docs/", mode: fs.ModeDir | 0o755},
		zipEntry{name: "docs/new-notes.txt", contents: "added"},
	))

	if err := Apply("v9.9.9", dir); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	want := map[string]string{
		"hopamine-captions":  "new program",
		"libdave.so":         "new library",
		"libnew.so":          "a library this version added",
		"HOW-TO-RUN.txt":     "new instructions",
		"docs/new-notes.txt": "added",
		// Never in an update zip, and never touched.
		"settings.json":      "the host's settings",
		".env":               "BOT_TOKEN=secret",
		"models/model.onnx":  "the model",
		"docs/old-notes.txt": "kept",
	}
	// No .old or .new files left behind either.
	if got := snapshot(t, dir); !maps.Equal(got, want) {
		t.Errorf("after the update the folder holds\n%v\nwant\n%v", got, want)
	}

	if runtime.GOOS != "windows" {
		for name, perm := range map[string]fs.FileMode{"hopamine-captions": 0o755, "libnew.so": 0o640} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != perm {
				t.Errorf("%s is %v, want %v", name, info.Mode().Perm(), perm)
			}
		}
	}
}

// Every way the download can be wrong must leave the folder exactly as it
// was: a program that won't start can't update itself out of it.
func TestApplyRefusesABadUpdate(t *testing.T) {
	asset := thisPlatformsAsset(t)
	good := makeZip(t, zipEntry{name: "hopamine-captions", contents: "new program"})

	symlink := makeZip(t, zipEntry{name: "hopamine-captions", contents: "/bin/sh", mode: fs.ModeSymlink | 0o777})

	// The zip's own check that a file wasn't damaged: change a byte of it.
	damaged := makeZip(t, zipEntry{name: "hopamine-captions", contents: "new program"})
	damaged[bytes.Index(damaged, []byte("new program"))] = 'N'

	// A compression method nobody knows, set in the zip's central directory
	// (where a reader looks it up).
	unknownMethod := makeZip(t, zipEntry{name: "hopamine-captions", contents: "new program"})
	central := bytes.Index(unknownMethod, []byte("PK\x01\x02"))
	unknownMethod[central+10] = 99

	for _, test := range []struct {
		name     string
		files    map[string][]byte
		contains string
	}{
		{"checksum doesn't match", map[string][]byte{asset: good, checksumsFileName: sumsLine(asset, []byte("something else"))}, "checksum"},
		{"no checksums file", map[string][]byte{asset: good}, "404"},
		{"checksums file doesn't list it", map[string][]byte{asset: good, checksumsFileName: sumsLine("other.zip", good)}, "no entry"},
		{"zip missing", map[string][]byte{checksumsFileName: sumsLine(asset, good)}, "404"},
		{"not a zip", withSums(asset, []byte("not a zip at all")), "opening the update"},
		{"empty zip", withSums(asset, makeZip(t)), "no files"},
		{"climbs out of the folder", withSums(asset, makeZip(t, zipEntry{name: "../outside", contents: "x"})), "outside the program's folder"},
		{"not in the zip's folder", withSums(asset, makeZip(t,
			zipEntry{name: "hopamine-captions", contents: "new program"},
			zipEntry{name: "stray.txt", contents: "x", outside: true})), "outside the program's folder"},
		{"absolute path", withSums(asset, makeZip(t, zipEntry{name: "/etc/outside", contents: "x"})), "outside the program's folder"},
		{"Windows-style climb", withSums(asset, makeZip(t, zipEntry{name: `docs\..\..\outside`, contents: "x"})), "outside the program's folder"},
		{"a link, not a file", withSums(asset, symlink), "isn't a plain file"},
		{"damaged file", withSums(asset, damaged), "reading hopamine-captions"},
		{"unknown compression", withSums(asset, unknownMethod), "reading hopamine-captions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := installedApp(t)
			before := snapshot(t, dir)
			fakeRelease(t, "v9.9.9", test.files)

			err := Apply("v9.9.9", dir)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("got %v, want an error mentioning %q", err, test.contains)
			}
			wantUnchanged(t, dir, before)
		})
	}
}

func withSums(asset string, archive []byte) map[string][]byte {
	return map[string][]byte{asset: archive, checksumsFileName: sumsLine(asset, archive)}
}

// A file failing partway puts back the ones already swapped in, so the folder
// is never half old and half new.
func TestApplyPutsEverythingBackWhenAFileFails(t *testing.T) {
	dir := installedApp(t)
	write(t, filepath.Join(dir, "blocker"), "a file where a folder is needed", 0o644)
	before := snapshot(t, dir)
	releaseOf(t, "v9.9.9", makeZip(t,
		zipEntry{name: "hopamine-captions", contents: "new program"}, // replaces a file
		zipEntry{name: "libnew.so", contents: "new library"},         // adds one
		zipEntry{name: "blocker/inside.txt", contents: "can't go here"},
	))

	err := Apply("v9.9.9", dir)
	if err == nil || !strings.Contains(err.Error(), "installing blocker/inside.txt") {
		t.Fatalf("got %v, want the failing file named", err)
	}
	wantUnchanged(t, dir, before)
}

func TestApplyWhenTheOldVersionCantBeMovedAside(t *testing.T) {
	dir := installedApp(t)
	// A leftover folder where the old version would go.
	write(t, filepath.Join(dir, "libdave.so.old", "stuck"), "x", 0o644)
	before := snapshot(t, dir)
	releaseOf(t, "v9.9.9", makeZip(t, zipEntry{name: "libdave.so", contents: "new library"}))

	err := Apply("v9.9.9", dir)
	if err == nil || !strings.Contains(err.Error(), "moving the old version aside") {
		t.Fatalf("got %v, want the move refused", err)
	}
	wantUnchanged(t, dir, before)
}

func TestApplyWhenTheNewVersionCantBePutInPlace(t *testing.T) {
	for _, name := range []string{"libdave.so", "libnew.so"} { // replacing a file, and adding one
		t.Run(name, func(t *testing.T) {
			dir := installedApp(t)
			before := snapshot(t, dir)
			releaseOf(t, "v9.9.9", makeZip(t, zipEntry{name: name, contents: "new library"}))
			failRenames(t, func(from, _ string) bool { return strings.HasSuffix(from, ".new") })

			err := Apply("v9.9.9", dir)
			if err == nil || !strings.Contains(err.Error(), "putting the new version in place") {
				t.Fatalf("got %v, want the swap refused", err)
			}
			wantUnchanged(t, dir, before)
		})
	}
}

// If even putting the old version back fails, the error says where it is, so
// someone can put it back by hand.
func TestApplyWhenPuttingTheOldVersionBackFails(t *testing.T) {
	dir := installedApp(t)
	releaseOf(t, "v9.9.9", makeZip(t, zipEntry{name: "libdave.so", contents: "new library"}))
	failRenames(t, func(from, to string) bool {
		return strings.HasSuffix(from, ".new") || strings.HasSuffix(from, ".old")
	})

	err := Apply("v9.9.9", dir)
	if err == nil || !strings.Contains(err.Error(), "it's at "+filepath.Join(dir, "libdave.so.old")) {
		t.Fatalf("got %v, want it to say where the old version is", err)
	}
}

func TestApplyWhenUndoingFails(t *testing.T) {
	dir := installedApp(t)
	write(t, filepath.Join(dir, "blocker"), "a file where a folder is needed", 0o644)
	releaseOf(t, "v9.9.9", makeZip(t,
		zipEntry{name: "libdave.so", contents: "new library"},
		zipEntry{name: "blocker/inside.txt", contents: "can't go here"},
	))
	failRenames(t, func(from, _ string) bool { return strings.HasSuffix(from, ".old") })

	err := Apply("v9.9.9", dir)
	if err == nil || !strings.Contains(err.Error(), "putting the previous files back failed") {
		t.Fatalf("got %v, want the failed undo reported", err)
	}
}

func TestApplyInAFolderItCantWriteTo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only folders don't stop new files on Windows")
	}
	for _, test := range []struct {
		name, contains string
	}{
		{"libnew.so", "writing the new version"},
		{"plugins/libnew.so", "permission denied"}, // the folder can't be made
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := installedApp(t)
			before := snapshot(t, dir)
			releaseOf(t, "v9.9.9", makeZip(t, zipEntry{name: test.name, contents: "new library"}))
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o755) })

			err := Apply("v9.9.9", dir)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("got %v, want an error mentioning %q", err, test.contains)
			}
			wantUnchanged(t, dir, before)
		})
	}
}

func TestApplyOnAPlatformWithNoRelease(t *testing.T) {
	swap(t, &goos, "plan9")
	dir := installedApp(t)
	before := snapshot(t, dir)
	if err := Apply("v9.9.9", dir); err == nil || !strings.Contains(err.Error(), "no release is built for plan9") {
		t.Errorf("got %v, want it to say there's no release for this platform", err)
	}
	wantUnchanged(t, dir, before)
}

func TestDownloadFailures(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, err := download(gone.URL); err == nil {
		t.Error("want an error when the server can't be reached")
	}

	// Says 100 bytes are coming, then hangs up after 10.
	cutOff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("0123456789"))
	}))
	t.Cleanup(cutOff.Close)
	if _, err := download(cutOff.URL); err == nil {
		t.Error("want an error when the download is cut off")
	}

	tooBig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0123456789"))
	}))
	t.Cleanup(tooBig.Close)
	swap(t, &maxDownloadBytes, 9)
	if _, err := download(tooBig.URL); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("got %v, want an over-the-cap download refused", err)
	}
	swap(t, &maxDownloadBytes, 10)
	if _, err := download(tooBig.URL); err != nil {
		t.Errorf("a download of exactly the cap should be fine, got %v", err)
	}
}

// ---------- Update: check, install, log ----------

func TestUpdate(t *testing.T) {
	asset := thisPlatformsAsset(t)
	update := makeZip(t, zipEntry{name: "hopamine-captions", contents: "new program"})

	t.Run("a dev build never asks", func(t *testing.T) {
		asked := fakeLatest(t, http.StatusOK, `{"tag_name":"v9.9.9"}`)
		if _, updated := Update("dev", installedApp(t)); updated || asked.Load() != 0 {
			t.Errorf("updated=%v, asked GitHub %d times; want neither", updated, asked.Load())
		}
	})

	t.Run("GitHub can't say", func(t *testing.T) {
		logs := captureLog(t)
		fakeLatest(t, http.StatusNotFound, `{"message":"Not Found"}`) // e.g. still private
		if _, updated := Update("v0.1.0", installedApp(t)); updated {
			t.Error("shouldn't update")
		}
		if !strings.Contains(logs.String(), "couldn't check for updates: GitHub answered HTTP 404") {
			t.Errorf("the failed check should be logged, logged %q", logs)
		}
	})

	t.Run("already the newest", func(t *testing.T) {
		logs := captureLog(t)
		fakeLatest(t, http.StatusOK, `{"tag_name":"v0.1.0"}`)
		if _, updated := Update("v0.1.0", installedApp(t)); updated || logs.Len() != 0 {
			t.Errorf("updated=%v, logged %q; want nothing", updated, logs)
		}
	})

	t.Run("installing fails", func(t *testing.T) {
		logs := captureLog(t)
		dir := installedApp(t)
		before := snapshot(t, dir)
		fakeLatest(t, http.StatusOK, `{"tag_name":"v9.9.9"}`)
		fakeRelease(t, "v9.9.9", map[string][]byte{asset: update}) // no checksums
		if _, updated := Update("v0.1.0", dir); updated {
			t.Error("shouldn't say it updated")
		}
		if !strings.Contains(logs.String(), "couldn't install v9.9.9, carrying on with v0.1.0") {
			t.Errorf("the failure should be logged, logged %q", logs)
		}
		wantUnchanged(t, dir, before)
	})

	t.Run("a newer version", func(t *testing.T) {
		logs := captureLog(t)
		dir := installedApp(t)
		fakeLatest(t, http.StatusOK, `{"tag_name":"v9.9.9"}`)
		releaseOf(t, "v9.9.9", update)
		tag, updated := Update("v0.1.0", dir)
		if !updated || tag != "v9.9.9" {
			t.Fatalf("got (%q, %v), want v9.9.9 installed", tag, updated)
		}
		if got := snapshot(t, dir)["hopamine-captions"]; got != "new program" {
			t.Errorf("the program holds %q", got)
		}
		if !strings.Contains(logs.String(), "updating from v0.1.0 to v9.9.9") || !strings.Contains(logs.String(), "installed v9.9.9") {
			t.Errorf("the update should be logged, logged %q", logs)
		}
	})
}

// ---------- cleaning up ----------

func TestCleanupOldRemovesLeftovers(t *testing.T) {
	dir := installedApp(t)
	// What Windows leaves behind: the update moved the running files aside
	// but couldn't delete them while they were in use.
	write(t, filepath.Join(dir, "hopamine-captions.old"), "previous program", 0o755)
	write(t, filepath.Join(dir, "plugins", "libx.so.old"), "previous library", 0o644)
	if runtime.GOOS != "windows" {
		// A folder it can't look in doesn't stop it.
		write(t, filepath.Join(dir, "locked", "file"), "x", 0o644)
		if err := os.Chmod(filepath.Join(dir, "locked"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(filepath.Join(dir, "locked"), 0o755) })
	}

	CleanupOld(dir)

	for _, name := range []string{"hopamine-captions.old", "plugins/libx.so.old"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was left behind (%v)", name, err)
		}
	}
	for _, name := range []string{"hopamine-captions", "libdave.so", "settings.json", ".env", "models/model.onnx"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("CleanupOld touched %s: %v", name, err)
		}
	}
}
