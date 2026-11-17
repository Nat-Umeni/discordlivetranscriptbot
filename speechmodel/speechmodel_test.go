package speechmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeModel swaps the real model for two small files served by a local
// server, and returns what the server was asked for.
func fakeModel(t *testing.T, served map[string]string) *requests {
	t.Helper()
	asked := &requests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		asked.add(name)
		contents, found := served[name]
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(contents))
	}))
	t.Cleanup(server.Close)

	swap(t, &baseURL, server.URL+"/")
	swap(t, &files, []modelFile{described("encoder.onnx", "the encoder's weights"), described("tokens.txt", "a b c")})
	return asked
}

// described is a model file whose expected size and hash are contents'.
func described(name, contents string) modelFile {
	sum := sha256.Sum256([]byte(contents))
	return modelFile{name: name, size: int64(len(contents)), sha256: hex.EncodeToString(sum[:])}
}

var realFiles = map[string]string{"encoder.onnx": "the encoder's weights", "tokens.txt": "a b c"}

type requests struct {
	mu    sync.Mutex
	names []string
}

func (r *requests) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
}

func (r *requests) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names)
}

func swap[T any](t *testing.T, variable *T, value T) {
	t.Helper()
	original := *variable
	*variable = value
	t.Cleanup(func() { *variable = original })
}

func modelFileIn(appDir, name string) string {
	return filepath.Join(appDir, Dir, name)
}

func wantFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Errorf("%s holds (%q, %v), want %q", filepath.Base(path), got, err, want)
	}
}

func wantNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s shouldn't be there (%v)", filepath.Base(path), err)
	}
}

func TestEnsureDownloadsTheModelTheFirstTime(t *testing.T) {
	asked := fakeModel(t, realFiles)
	appDir := t.TempDir()
	var out strings.Builder

	if err := Ensure(appDir, &out); err != nil {
		t.Fatal(err)
	}

	wantFile(t, modelFileIn(appDir, "encoder.onnx"), "the encoder's weights")
	wantFile(t, modelFileIn(appDir, "tokens.txt"), "a b c")
	wantNoFile(t, modelFileIn(appDir, "encoder.onnx.part"))
	for _, want := range []string{"Downloading the speech model", "\r  encoder.onnx  100%", "\r  tokens.txt  100%", "Speech model downloaded."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("should print %q, printed %q", want, out.String())
		}
	}
	if got := asked.list(); len(got) != 2 {
		t.Errorf("want each file asked for once, asked for %q", got)
	}
}

func TestEnsureDoesNothingWhenTheModelIsThere(t *testing.T) {
	asked := fakeModel(t, realFiles)
	appDir := t.TempDir()
	if err := Ensure(appDir, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	if err := Ensure(appDir, &out); err != nil {
		t.Fatal(err)
	}

	if out.Len() != 0 || len(asked.list()) != 2 {
		t.Errorf("a second start shouldn't download anything: printed %q, asked for %q", out.String(), asked.list())
	}
}

// After a failed download, the next start fetches only what's missing.
func TestEnsureOnlyFetchesWhatsMissing(t *testing.T) {
	asked := fakeModel(t, realFiles)
	appDir := t.TempDir()
	writeModelFile(t, appDir, "encoder.onnx", "the encoder's weights")

	if err := Ensure(appDir, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}

	if got := asked.list(); !slices.Equal(got, []string{"tokens.txt"}) {
		t.Errorf("want only tokens.txt fetched, asked for %q", got)
	}
}

func TestEnsureReplacesAFileOfTheWrongSize(t *testing.T) {
	fakeModel(t, realFiles)
	appDir := t.TempDir()
	writeModelFile(t, appDir, "encoder.onnx", "half of it")

	if err := Ensure(appDir, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}

	wantFile(t, modelFileIn(appDir, "encoder.onnx"), "the encoder's weights")
}

func TestEnsureRefusesAWrongFile(t *testing.T) {
	for _, test := range []struct {
		why, contents, contains string
	}{
		{"same size, different bytes", "THE ENCODER'S WEIGHTS", "doesn't match its checksum"},
		{"too short", "the encoder", "want 21"},
		{"too long", "the encoder's weights and more", "want 21"},
	} {
		t.Run(test.why, func(t *testing.T) {
			fakeModel(t, map[string]string{"encoder.onnx": test.contents, "tokens.txt": "a b c"})
			appDir := t.TempDir()

			err := Ensure(appDir, &strings.Builder{})

			if err == nil || !strings.Contains(err.Error(), "encoder.onnx") || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("got %v, want an error about encoder.onnx mentioning %q", err, test.contains)
			}
			wantNoFile(t, modelFileIn(appDir, "encoder.onnx"))
			wantNoFile(t, modelFileIn(appDir, "encoder.onnx.part"))
		})
	}
}

func TestEnsureWhenTheServerHasntGotIt(t *testing.T) {
	fakeModel(t, map[string]string{"encoder.onnx": "the encoder's weights"}) // no tokens.txt
	appDir := t.TempDir()

	err := Ensure(appDir, &strings.Builder{})

	if err == nil || !strings.Contains(err.Error(), "tokens.txt") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v, want a 404 for tokens.txt", err)
	}
	// What did download is kept for next time.
	wantFile(t, modelFileIn(appDir, "encoder.onnx"), "the encoder's weights")
}

func TestEnsureWhenTheServerCantBeReached(t *testing.T) {
	fakeModel(t, realFiles)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	swap(t, &baseURL, gone.URL+"/")

	if err := Ensure(t.TempDir(), &strings.Builder{}); err == nil {
		t.Error("want an error when the server can't be reached")
	}
}

func TestEnsureWhenTheDownloadIsCutOff(t *testing.T) {
	fakeModel(t, realFiles)
	// Says the whole file is coming, then hangs up partway.
	cutOff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "21")
		w.Write([]byte("the enc"))
	}))
	t.Cleanup(cutOff.Close)
	swap(t, &baseURL, cutOff.URL+"/")
	appDir := t.TempDir()

	if err := Ensure(appDir, &strings.Builder{}); err == nil {
		t.Fatal("want an error when the download is cut off")
	}
	wantNoFile(t, modelFileIn(appDir, "encoder.onnx.part"))
}

func TestEnsureWhenItCantWriteTheFiles(t *testing.T) {
	t.Run("the folder can't be made", func(t *testing.T) {
		fakeModel(t, realFiles)
		appDir := t.TempDir()
		// A file where the models folder should be.
		if err := os.WriteFile(filepath.Join(appDir, "models"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Ensure(appDir, &strings.Builder{}); err == nil {
			t.Error("want an error")
		}
	})

	t.Run("the download can't be written", func(t *testing.T) {
		fakeModel(t, realFiles)
		appDir := t.TempDir()
		// A folder where the download goes.
		if err := os.MkdirAll(modelFileIn(appDir, "encoder.onnx.part"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := Ensure(appDir, &strings.Builder{}); err == nil {
			t.Error("want an error")
		}
	})

	t.Run("the download can't be put in place", func(t *testing.T) {
		fakeModel(t, realFiles)
		appDir := t.TempDir()
		// A folder, with something in it, where the finished file goes.
		writeModelFile(t, appDir, filepath.Join("encoder.onnx", "stuck"), "x")
		if err := Ensure(appDir, &strings.Builder{}); err == nil {
			t.Error("want an error")
		}
	})
}

// The real list must be the files speech.NewRecogniser loads, with sizes and
// hashes that look right (a typo in one would fail every first start).
func TestTheRealModelFiles(t *testing.T) {
	var names []string
	for _, file := range files {
		names = append(names, file.name)
		if file.size <= 0 || len(file.sha256) != 64 {
			t.Errorf("%s: size %d, hash %q", file.name, file.size, file.sha256)
		}
		if _, err := hex.DecodeString(file.sha256); err != nil {
			t.Errorf("%s's hash isn't hex: %v", file.name, err)
		}
	}
	want := []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"}
	if !slices.Equal(names, want) {
		t.Errorf("got %q, want %q", names, want)
	}
	if !strings.HasPrefix(baseURL, "https://huggingface.co/") || !strings.Contains(baseURL, modelName) {
		t.Errorf("unexpected download address %q", baseURL)
	}
}

func writeModelFile(t *testing.T, appDir, name, contents string) {
	t.Helper()
	path := modelFileIn(appDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
