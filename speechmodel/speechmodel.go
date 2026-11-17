// Package speechmodel downloads the speech model the first time the bot runs.
//
// The model isn't in the download: it's NVIDIA's, under NVIDIA's own licence,
// so the bot fetches it straight from the sherpa-onnx maintainer's copy rather
// than us handing out copies of it.
package speechmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Dir is where the model lives, inside the program's folder: NVIDIA's
// Nemotron speech streaming 0.6b, 560 ms chunks, int8, as converted for
// sherpa-onnx.
const Dir = "models/" + modelName

const modelName = "sherpa-onnx-nemotron-speech-streaming-en-0.6b-560ms-int8-2026-04-25"

// baseURL is the Hugging Face copy published by sherpa-onnx's maintainer, at
// a fixed revision so the files can't change under us. A variable so tests
// can use a local server.
var baseURL = "https://huggingface.co/csukuangfj2/" + modelName + "/resolve/52056fdc070914a48dcd68b31b44d6a6f5b85902/"

// modelFile is one of the model's files, as it must be when downloaded.
type modelFile struct {
	name   string
	size   int64
	sha256 string
}

// files are the parts the bot loads (speech.NewRecogniser). The sizes and
// hashes are of the copy the bot was tested with.
var files = []modelFile{
	{"encoder.int8.onnx", 652916849, "7d932213491ad355c6e5576705dc3494731a52af87d7a1b954559340147909d8"},
	{"decoder.int8.onnx", 7257753, "0be9702c2f427a2b6bb241d298e0d3836a558de1f5b9fd3018f1cce6e2b3fa98"},
	{"joiner.int8.onnx", 1735862, "a35eac38a22ebceb04d230ed7afe0d68f446ba6914a036b97f14fece95967e23"},
	{"tokens.txt", 8952, "dc0b4584ab2e4ddbf888425c076c61b736e7356a015250db7d307e6f1a8188ff"},
}

// client has no overall time limit, because 660 MB over a slow connection
// can take a long time. Waiting for the server to answer at all is limited.
var client = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ResponseHeaderTimeout: 30 * time.Second,
}}

// Ensure downloads whichever of the model's files aren't in appDir yet. A
// file that's there at the right size is trusted (checking its hash would
// read 660 MB on every start); every download is checked against its hash.
// Files that finished stay, so a failed download only fetches what's left
// next time.
func Ensure(appDir string, out io.Writer) error {
	dir := filepath.Join(appDir, Dir)
	announced := false
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == file.size {
			continue
		}
		if !announced {
			fmt.Fprintf(out, "Downloading the speech model (one time, about %d MB)…\n", totalSize()/1_000_000)
			announced = true
		}
		if err := download(file, path, out); err != nil {
			return fmt.Errorf("downloading the speech model's %s: %w", file.name, err)
		}
	}
	if announced {
		fmt.Fprintln(out, "Speech model downloaded.")
	}
	return nil
}

func totalSize() int64 {
	var total int64
	for _, file := range files {
		total += file.size
	}
	return total
}

// download fetches one file into path. It's written to path.part first and
// only renamed into place once its size and hash are right, so a half
// download or a wrong file never looks like a finished one.
func download(file modelFile, path string, out io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	response, err := client.Get(baseURL + file.name)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered HTTP %s", response.Status)
	}

	partPath := path + ".part"
	part, err := os.Create(partPath)
	if err != nil {
		return err
	}
	// Covers every failure below. Once the rename succeeds there's nothing
	// left at partPath.
	defer os.Remove(partPath)

	hash := sha256.New()
	progress := &progress{out: out, name: file.name, size: file.size, shown: -1}
	// One byte more than expected, so a file that's too big is caught.
	written, copyErr := io.Copy(io.MultiWriter(part, hash, progress), io.LimitReader(response.Body, file.size+1))
	fmt.Fprintln(out)
	if err := errors.Join(copyErr, part.Close()); err != nil {
		return err
	}
	if written != file.size {
		return fmt.Errorf("got %d bytes, want %d", written, file.size)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != file.sha256 {
		return errors.New("it doesn't match its checksum, so it wasn't kept")
	}
	return os.Rename(partPath, path)
}

// progress shows how far a download has got, every 10%, on one line.
type progress struct {
	out   io.Writer
	name  string
	size  int64
	done  int64
	shown int64 // tenths shown so far, -1 before the first
}

func (p *progress) Write(chunk []byte) (int, error) {
	p.done += int64(len(chunk))
	if tenths := min(p.done*10/p.size, 10); tenths != p.shown {
		p.shown = tenths
		fmt.Fprintf(p.out, "\r  %s  %d%%", p.name, tenths*10)
	}
	return len(chunk), nil
}
