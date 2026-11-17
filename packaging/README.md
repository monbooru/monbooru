# Packaging

| Path | Artifact |
|---|---|
| `icons/` | `monbooru.ico` for Windows shortcuts and the tray, plus the PNG sizes the Flatpak installs into hicolor |
| `flatpak/` | the manifest, the launcher wrapper, the desktop entry and the AppStream metainfo |
| `appimage/` | the AppRun the AppImage runtime executes |
| `monbooru.desktop` | the desktop entry the AppImage carries |
| `aur/` | the PKGBUILD the AUR package is stamped from, and the licence for those packaging sources |
| `windows/` | the Inno Setup script; the workflow stages the installer's own copy of the archive's contents under `windows/payload/` first |
| `kernelsu/` | the module a KernelSU manager installs on a phone: the arm64 binary, the scripts that keep it running, and the panel the manager shows for it |
| `release-env.sh` | the version and the ldflags every artifact is stamped with |
| `fetch-tools.sh` | downloads the ffmpeg and ONNX Runtime the bundled build carries, for one target, into `tools/` |
| `build-binaries.sh` | assembles the portable archives into `dist/`, one shape per run. Each carries an empty `monbooru.toml`, which is what makes unpacking one keep its data in the folder |
| `build-kernelsu.sh` | builds the module into `dist/kernelsu/`, stamped with the version and the release's changelog, next to the `update.json` a manager polls |
| `build-installer.sh` | runs ISCC over one staged payload to produce a Windows installer |
| `flatpak-bundle.sh` | installs the runtime and builds the Flatpak bundle; must run unprivileged |
| `build-appimage.sh` | builds the bundled AppImage for one target, from the same `tools/` |
| `appimage-core.sh` | sourced by `build-appimage.sh`: fetches the pinned appimagetool and runtime, adds the desktop entry and icons and packs the AppImage. Kept identical to the sibling app's copy |
| `build-aur.sh` | stamps the PKGBUILD with the version and the tag tarball's checksum, and generates its `.SRCINFO` |