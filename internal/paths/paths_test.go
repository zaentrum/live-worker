package paths

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zaentrum/live-worker/api"
)

func TestCheck(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(base, "media")
	other := filepath.Join(base, "other")
	for _, d := range []string{media, other, filepath.Join(media, "dir")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(media, "a.mkv")
	outside := filepath.Join(other, "b.mkv")
	for _, f := range []string{inside, outside} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	escape := filepath.Join(media, "escape.mkv")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	r, err := New([]string{media})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.Check(inside); err != nil || got != inside {
		t.Fatalf("inside: %q %v", got, err)
	}
	for _, p := range []string{outside, escape, filepath.Join(media, "dir")} {
		_, err := r.Check(p)
		if !errors.Is(err, ErrOutside) || !errors.Is(err, api.ErrBadRequest) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := r.Check(filepath.Join(media, "missing.mkv")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, err := r.Check(media + "/../other/b.mkv"); !errors.Is(err, api.ErrBadRequest) {
		t.Errorf("unclean path: %v", err)
	}
	if _, err := New(nil); err == nil {
		t.Error("no roots accepted")
	}
	if got := Split("/a: /b ::"); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("split %q", got)
	}
}
