# Sourced, not run: the version and ldflags every artifact is stamped with.
VERSION=$(tr -d '[:space:]' < VERSION.md)
REPO=$(tr -d '[:space:]' < REPOSITORY.md)
DOC=$(tr -d '[:space:]' < DOC.md)
APP=monbooru
APPID=io.github.monbooru.Monbooru
MOD="github.com/monbooru/monbooru/internal/web"
LDFLAGS="-X '$MOD.Version=$VERSION' -X '$MOD.RepoURL=$REPO' -X '$MOD.DocURL=$DOC'"
