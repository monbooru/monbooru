package gallery

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

type FenceKind int

const (
	FenceGallery FenceKind = iota
	FenceData
	FenceModels
	FenceThemes
	FencePlugins
)

type Fence struct {
	Path  string
	Owner string
	Kind  FenceKind
}

// Boundary is everything under a gallery's root but its fences and
// ignored names. A folder belongs to the gallery with the closest root
// above it; monbooru's own folders belong to none.
type Boundary struct {
	root     string
	rootReal string
	fences   []fence
	byName   map[string]struct{}
	byReal   map[string]struct{}
	// Every other gallery's root, those above this one included: a link
	// into any of them leads out of this gallery.
	others []fence
	names  map[string]ignorePattern
	globs  []ignorePattern
	paths  []ignorePattern
}

type ignorePattern struct {
	match string
	text  string
	depth int
}

// name is the fence's path as a walk of the root reaches it, "" when no
// walk does.
type fence struct {
	Fence
	name string
	real string
}

// Fences and the root are compared as written and as resolved, so a
// gallery configured through a symlink still fences where the link
// points. ignore must already be normalised by the config.
func NewBoundary(root string, fences []Fence, ignore []string) *Boundary {
	b := &Boundary{root: filepath.Clean(root), byName: map[string]struct{}{}, byReal: map[string]struct{}{}, names: map[string]ignorePattern{}}
	for _, text := range ignore {
		p := ignorePattern{match: strings.ToLower(text), text: text}
		switch {
		case strings.Contains(p.match, "/"):
			p.match = strings.TrimPrefix(p.match, "/")
			p.depth = strings.Count(p.match, "/") + 1
			b.paths = append(b.paths, p)
		case strings.ContainsAny(p.match, `*?[\`):
			b.globs = append(b.globs, p)
		default:
			b.names[p.match] = p
		}
	}
	rootReal := resolvePath(b.root)
	b.rootReal = rootReal
	for _, f := range fences {
		if f.Path == "" {
			continue
		}
		f.Path = filepath.Clean(f.Path)
		fc := fence{Fence: f, real: resolvePath(f.Path)}
		if f.Kind == FenceGallery {
			b.others = append(b.others, fc)
		}
		if PathInside(fc.Path, b.root) || PathInside(fc.real, rootReal) {
			continue
		}
		if PathInside(b.root, fc.Path) {
			fc.name = fc.Path
		} else if rel, err := filepath.Rel(rootReal, fc.real); err == nil && PathInside(rootReal, fc.real) {
			fc.name = filepath.Join(b.root, rel)
		}
		if fc.name != "" {
			b.byName[pathKey(fc.name)] = struct{}{}
		}
		b.byReal[pathKey(fc.real)] = struct{}{}
		b.fences = append(b.fences, fc)
	}
	return b
}

// An unresolvable path stays as written: a folder on an unmounted drive
// is still where it was configured.
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func (b *Boundary) Root() string { return b.root }

func (b *Boundary) Excludes(path string) bool {
	_, out := b.excluded(filepath.Clean(path))
	return out
}

func (b *Boundary) excluded(p string) (Exclusion, bool) {
	for _, f := range b.fences {
		if f.name != "" && PathInside(f.name, p) {
			return b.exclusion(f, f.name, false), true
		}
	}
	if rel, err := filepath.Rel(b.root, p); err == nil && rel != "." && !climbsOut(rel) {
		if pattern, at, ok := b.ignored(filepath.ToSlash(rel)); ok {
			return Exclusion{Rel: at, Pattern: pattern}, true
		}
	}
	return Exclusion{}, false
}

func (b *Boundary) ignored(rel string) (pattern, at string, ok bool) {
	segs := strings.Split(rel, "/")
	lower := strings.Split(strings.ToLower(rel), "/")
	for i, seg := range lower {
		if p, ok := b.ignoresName(seg); ok {
			return p.text, strings.Join(segs[:i+1], "/"), true
		}
	}
	for _, p := range b.paths {
		if p.depth > len(lower) {
			continue
		}
		if m, _ := path.Match(p.match, strings.Join(lower[:p.depth], "/")); m {
			return p.text, strings.Join(segs[:p.depth], "/"), true
		}
	}
	return "", "", false
}

func (b *Boundary) ignoresName(name string) (ignorePattern, bool) {
	if p, ok := b.names[name]; ok {
		return p, true
	}
	for _, p := range b.globs {
		if m, _ := path.Match(p.match, name); m {
			return p, true
		}
	}
	return ignorePattern{}, false
}

// Check is Excludes for a write or an unlink. It also resolves the deepest
// existing folder, since a link inside the gallery can lead into a fence or
// another gallery. Containment in the root is the caller's check.
func (b *Boundary) Check(path string) error {
	path = filepath.Clean(path)
	if e, out := b.excluded(path); out {
		return e
	}
	dir := path
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil
	}
	rest, _ := filepath.Rel(dir, path)
	target := filepath.Join(real, rest)
	for _, f := range b.fences {
		if PathInside(f.real, target) {
			return b.exclusion(f, dir, true)
		}
	}
	// A gallery above this one holds its whole tree, so only a target
	// outside the root leads into it.
	if !PathInside(b.rootReal, target) {
		for _, f := range b.others {
			if PathInside(f.real, target) {
				return b.exclusion(f, dir, true)
			}
		}
	}
	return nil
}

func (b *Boundary) ResolveSubdir(folder string) (string, error) {
	dir, err := resolveSubdir(b.root, folder)
	if err != nil {
		return "", err
	}
	if err := b.Check(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func (b *Boundary) exclusion(f fence, at string, link bool) Exclusion {
	rel, err := filepath.Rel(b.root, at)
	if err != nil {
		rel = at
	}
	return Exclusion{Rel: filepath.ToSlash(rel), Owner: f.Owner, Kind: f.Kind, Link: link}
}

type Exclusion struct {
	Rel     string
	Owner   string
	Kind    FenceKind
	Link    bool
	Pattern string
}

func (e Exclusion) Error() string {
	if e.Pattern != "" {
		if strings.EqualFold(e.Pattern, path.Base(e.Rel)) {
			return fmt.Sprintf("%q is on the ignore list", e.Rel)
		}
		return fmt.Sprintf("%q is on the ignore list (%s)", e.Rel, e.Pattern)
	}
	what := e.OwnerName()
	switch {
	case e.Link:
		return fmt.Sprintf("%q leads into %s", e.Rel, what)
	case e.Kind == FenceGallery:
		return fmt.Sprintf("%q belongs to %s", e.Rel, what)
	}
	return fmt.Sprintf("%q is %s", e.Rel, what)
}

func (e Exclusion) OwnerName() string {
	switch e.Kind {
	case FenceData:
		return "monbooru's data folder"
	case FenceModels:
		return "monbooru's model folder"
	case FenceThemes:
		return "monbooru's theme folder"
	case FencePlugins:
		return "monbooru's plugin folder"
	}
	return "gallery " + e.Owner
}

func (b *Boundary) Fenced() []Exclusion {
	var out []Exclusion
	for _, f := range b.fences {
		if f.name == "" {
			continue
		}
		nested := slices.ContainsFunc(b.fences, func(g fence) bool {
			return g.name != "" && g.name != f.name && PathInside(g.name, f.name)
		})
		if !nested {
			out = append(out, b.exclusion(f, f.name, false))
		}
	}
	slices.SortFunc(out, func(x, y Exclusion) int { return strings.Compare(x.Rel, y.Rel) })
	return out
}

// Windows paths match without regard to case, as PathInside compares them there.
func pathKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// The walk has already asked about every ancestor, so only the entry
// itself is tested.
func (b *Boundary) skips(name, real string) bool {
	if _, ok := b.byName[pathKey(name)]; ok {
		return true
	}
	if _, ok := b.byReal[pathKey(real)]; ok {
		return true
	}
	if _, ok := b.ignoresName(strings.ToLower(filepath.Base(name))); ok {
		return true
	}
	if len(b.paths) == 0 {
		return false
	}
	rel, err := filepath.Rel(b.root, name)
	if err != nil {
		return false
	}
	rel = strings.ToLower(filepath.ToSlash(rel))
	depth := strings.Count(rel, "/") + 1
	for _, p := range b.paths {
		if p.depth != depth {
			continue
		}
		if m, _ := path.Match(p.match, rel); m {
			return true
		}
	}
	return false
}

func (b *Boundary) leadsOut(target string) bool {
	holds := func(f fence) bool { return PathInside(f.real, target) }
	return slices.ContainsFunc(b.fences, holds) || slices.ContainsFunc(b.others, holds)
}

// RemoveOwned keeps what b skips and every folder on the way to it. Links
// are removed, never followed, and the root stays.
func RemoveOwned(b *Boundary) (kept []string, err error) {
	keep := func(p string) {
		rel, _ := filepath.Rel(b.root, p)
		kept = append(kept, filepath.ToSlash(rel))
	}
	var sweep func(dir string) (bool, error)
	sweep = func(dir string) (bool, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if dir == b.root {
				return false, err
			}
			// A folder it cannot read, such as a mount's lost+found, stays whole.
			keep(dir)
			return false, nil
		}
		empty := true
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if b.skips(p, p) {
				keep(p)
				empty = false
				continue
			}
			if e.IsDir() {
				gone, err := sweep(p)
				if err != nil {
					return false, err
				}
				if !gone {
					empty = false
					continue
				}
			}
			if err := os.Remove(p); err != nil {
				return false, err
			}
		}
		return empty, nil
	}
	_, err = sweep(b.root)
	return kept, err
}

// SharedWith names another gallery configured on this same folder, or "".
// Its files are this gallery's too, so nothing may remove them.
func (b *Boundary) SharedWith() string {
	for _, f := range b.others {
		if SameFolder(f.Path, b.root) {
			return f.Owner
		}
	}
	return ""
}

func SameFolder(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b) || resolvePath(a) == resolvePath(b)
}

func Encloses(outer, inner string) bool {
	if o, i := filepath.Clean(outer), filepath.Clean(inner); o != i && PathInside(o, i) {
		return true
	}
	o, i := resolvePath(outer), resolvePath(inner)
	return o != i && PathInside(o, i)
}
