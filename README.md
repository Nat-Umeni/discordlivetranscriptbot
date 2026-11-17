# Discord Live Captions

A Discord bot that sits in a voice call and captions it live into a thread, so
Deaf and hard-of-hearing members can follow meetings and events.

- **Live captions**, one message per speaker turn, growing as they talk.
- **Note-takers fix captions** as they go: right-click → Apps → Fix caption
  to change words, re-credit the speaker or add tone tags like `[laughs]`.
- **Typed messages** in the call's chat are copied in, so people who'd rather
  type can take part.
- **A transcript** (`.txt` and `.md`) is posted when captions stop, for
  anyone who missed the call.
- **Opting out:** anyone with the server's no-captions role is never
  captioned; their audio isn't even decoded.
- **Green and private:** speech is turned into text on the host's own CPU by
  a small speech model (NVIDIA Nemotron Speech Streaming 0.6B, via
  [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx)). No cloud AI, no
  LLMs, no GPU. Audio is never sent anywhere or stored.

## Running it

Download the zip for your system from the public
[releases repository](https://github.com/Nat-Umeni/discord-transcription-releases/releases/latest)
(`discord-live-captions-windows-x64.zip` or `discord-live-captions-linux-x64.zip`),
unzip it and follow [`HOW-TO-RUN.txt`](build/HOW-TO-RUN.txt) inside: make
the Discord bot, invite it, start the program and paste the bot token. The
first start downloads the speech model (about 660 MB): it isn't in the zip,
because it's NVIDIA's, under their own licence.

It updates itself: it checks for a new release when it starts and once a day,
and installs it once no captions are running.

## Building from source

Needs Go (the version in `go.mod`), a C compiler, `pkg-config` and opus
(`libopus-dev` on Debian/Ubuntu, `opus` on Arch).

```sh
build/install-libdave.sh .libdave     # Discord's end-to-end encryption library
export PKG_CONFIG_PATH=$PWD/.libdave/lib/pkgconfig
go run ./build/getmodel               # the speech model (~660 MB download)
go run -tags nolibopusfile .          # asks for the bot token the first time
go test -race -tags nolibopusfile ./...
```

`build/package-linux.sh` builds the Linux download into `dist/` (needs `zip`,
and `patchelf` for a release). The Windows download is built on GitHub's
Windows runner with MSYS2 (`build/package-windows.sh`).

## Releasing

```sh
git tag v0.2.0 && git push --tags
```

The `release` workflow tests on Linux and Windows, builds both downloads and
publishes them in the public
[releases repository](https://github.com/Nat-Umeni/discord-transcription-releases),
so this code can stay private. It needs the `RELEASES_TOKEN` secret: a
fine-grained token with Contents read and write on that repository only. Each
release has one zip per system (new installs and updates use the same one)
and `checksums.txt`, which a bot checks every update against.

Changing the speech model (`speechmodel/`) needs nothing extra: an updated
bot downloads the new one when it restarts.

## Layout

| Folder | What's in it |
|---|---|
| `captionbot/` | the Discord side: commands, announcements, ending calls, fixes, transcripts |
| `speech/` | the speech model and a transcriber per call |
| `captions/` | caption formatting and the poster that keeps messages growing |
| `transcript/` | turns a captions thread back into a transcript |
| `settings/`, `sessions/` | per-server settings (`settings.json`) and running calls |
| `startup/`, `updater/`, `speechmodel/` | finding the program's folder and token; self-updating; downloading the model |
| `build/` | build scripts and the docs that go in the download |

Licences of everything it's built from: [`THIRD_PARTY_NOTICES.txt`](build/THIRD_PARTY_NOTICES.txt).
