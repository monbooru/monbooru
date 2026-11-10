package plugins

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestPicksTheWindowsLine(t *testing.T) {
	m := Manifest{Command: "python3", Args: []string{"main.py"}, CommandWindows: "py"}
	if cmd, args := m.launchFor("windows"); cmd != "py" || len(args) != 1 || args[0] != "main.py" {
		t.Errorf("windows launch = %q %v", cmd, args)
	}
	if cmd, _ := m.launchFor("linux"); cmd != "python3" {
		t.Errorf("linux launch = %q", cmd)
	}
	if got := resolveCommand("/plugins/x", "bin/run"); got != filepath.Join("/plugins/x", "bin/run") {
		t.Errorf("relative command = %q", got)
	}
	if got := resolveCommand("/plugins/x", "python3"); got != "python3" {
		t.Errorf("bare command = %q", got)
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	write := func(name, manifest string) {
		t.Helper()
		folder := filepath.Join(dir, name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		if manifest == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(folder, "plugin.toml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good", "command = \"python3\"\nargs = [\"main.py\"]\ncontract = 1\n")
	write("too-new", "command = \"python3\"\ncontract = 99\n")
	write("relative", "command = \"bin/run\"\n")
	write("no-manifest", "")
	write("no-command", "args = [\"main.py\"]\n")
	write("broken", "command = \n")
	write("reserved", "command = \"python3\"\n")
	if err := os.WriteFile(filepath.Join(dir, "loose.toml"), []byte("command = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, refused := Discover(dir, func(name string) (string, bool) { return "reserved name", name == "reserved" })
	names := make(map[string]Launch, len(got))
	for _, l := range got {
		names[l.Name] = l
	}
	if len(got) != 2 {
		t.Fatalf("discovered %v, want only good and relative (too-new needs a later contract)", got)
	}
	if l := names["good"]; l.Command != "python3" || len(l.Args) != 1 || l.Args[0] != "main.py" || l.Dir != filepath.Join(dir, "good") {
		t.Errorf("good = %+v", l)
	}

	if l := names["relative"]; l.Command != filepath.Join(dir, "relative", "bin/run") {
		t.Errorf("relative command = %q", l.Command)
	}

	reasons := make(map[string]string, len(refused))
	for _, r := range refused {
		reasons[r.Name] = r.Reason
	}
	if len(refused) != 4 || reasons["too-new"] == "" || reasons["no-command"] == "" || reasons["broken"] == "" || reasons["reserved"] != "reserved name" {
		t.Errorf("refused = %+v, want too-new, no-command, broken and reserved", refused)
	}

	if got, refused := Discover("", nil); got != nil || refused != nil {
		t.Errorf("no plugin dir configured = %v %v, want nil", got, refused)
	}
	if got, refused := Discover(filepath.Join(dir, "missing"), nil); got != nil || refused != nil {
		t.Errorf("unreadable dir = %v %v, want nil", got, refused)
	}
}

func TestDiscoverFollowsASymlinkedFolder(t *testing.T) {
	dir, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "plugin.toml"), []byte("command = \"python3\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got, _ := Discover(dir, nil)
	if len(got) != 1 || got[0].Name != "linked" || got[0].Dir != link {
		t.Errorf("discovered %+v, want one plugin in %s", got, link)
	}
}

func TestDiscoverReportsAFolderAboveTheContract(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "future")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("command = \"python3\"\ncontract = %d\n", Contract+1)
	if err := os.WriteFile(filepath.Join(folder, "plugin.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	got, refused := Discover(dir, nil)
	if len(got) != 0 || len(refused) != 1 || refused[0].Name != "future" {
		t.Fatalf("discovered %+v, refused %+v, want future refused", got, refused)
	}
	want := fmt.Sprintf("it needs plugin contract %d, this monbooru speaks %d", Contract+1, Contract)
	if refused[0].Reason != want {
		t.Errorf("reason = %q, want %q", refused[0].Reason, want)
	}
}

func TestDiscoverWarnsOnceAboutARefusal(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dir := t.TempDir()
	folder := filepath.Join(dir, "nocmd")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(manifest string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(folder, "plugin.toml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("args = [\"main.py\"]\n")
	for range 3 {
		Discover(dir, nil)
	}
	if n := strings.Count(buf.String(), "skipping"); n != 1 {
		t.Errorf("three scans of one refused folder logged %d warnings, want 1:\n%s", n, buf.String())
	}
	write("contract = 99\n")
	Discover(dir, nil)
	if n := strings.Count(buf.String(), "skipping"); n != 2 {
		t.Errorf("a new reason logged %d warnings in all, want 2:\n%s", n, buf.String())
	}
}
