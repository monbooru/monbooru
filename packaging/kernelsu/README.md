# KernelSU module

Packs the arm64 server, the script that keeps it running and the module's own
page into one zip that a KernelSU manager installs on a phone. The page in the
manager starts and stops the server, shows its state and tails its log; the
library itself opens in a browser.

## Requirements

- arm64 Android, KernelSU or a compatible manager (`$ARCH` other than arm64 is
  refused at install time)
- no requirement on `/system`: the module carries its binary and keeps
  everything else under `/data/adb/monbooru`, so it needs neither a metamodule
  nor `skip_mount`

## Install

Download `monbooru_<version>_kernelsu_arm64.zip` from the release, then install
it from the manager's module page. The installer writes a starting config to
`/data/adb/monbooru/monbooru.toml` and creates the gallery directory; an
existing config is left alone.

The server starts on boot. To start it now, open the module's page and press
Start, or tap Action, which toggles it.

## Where things live

| Path | What |
|---|---|
| `/data/adb/modules/monbooru/` | the module: binary, scripts, page; replaced on update |
| `/data/adb/monbooru/monbooru.toml` | the config; written once, never overwritten |
| `/data/adb/monbooru/data/` | the gallery databases and thumbnails |
| `/data/adb/monbooru/models/` | ONNX models, if you later switch to a tagger build |
| `/data/adb/monbooru/bin/` | `ffmpeg` and `ffprobe`, optional; this directory is first on the server's `PATH` |
| `/data/adb/monbooru/log/monbooru.log` | the server's output; rotated to `.log.1` past 1 MiB |
| `/sdcard/Pictures/monbooru/` | the default gallery |

## Config

`/data/adb/monbooru/monbooru.toml` is a normal monbooru config; the module's
copy sets only the paths the phone needs. Restart from the page after editing
it, and delete the file to get the shipped defaults back on the next install.

It listens on `127.0.0.1:8455`, which only the phone itself can reach. For the
library on your network, set both:

```toml
[server]
  bind_address = "0.0.0.0:8455"
  base_url = "http://<the phone's address>:8455"
```

## Videos

`ffmpeg` and `ffprobe` are not bundled. Drop static arm64 builds into
`/data/adb/monbooru/bin/` and restart; the server looks for them on its `PATH`
before giving up on video support.

## What this build leaves out

It is the lite build: no ONNX Runtime and no model files, so the auto tagger
stays off and the server never loads a model. Everything else, including the
gallery, the API and the scheduler, is the same as the portable archives.

## Uninstalling

Removing the module stops the server and keeps `/data/adb/monbooru`: the
library database, the config and the log are the user's files.
