// Package paths keeps a worker's sources inside its media roots: a request
// names a file by its path, and nothing but a regular file under one of the
// roots (after symbolic links) is opened.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zaentrum/live-worker/api"
)

// Roots are the folders sources may be read from.
type Roots struct {
	roots []string // resolved, clean
}

// New checks that every root is a folder and resolves it.
func New(roots []string) (*Roots, error) {
	if len(roots) == 0 {
		return nil, errors.New("no media roots")
	}
	r := &Roots{}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("media root %q is not absolute", root)
		}
		res, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return nil, fmt.Errorf("media root %q: %w", root, err)
		}
		st, err := os.Stat(res)
		if err != nil {
			return nil, fmt.Errorf("media root %q: %w", root, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("media root %q is no folder", root)
		}
		r.roots = append(r.roots, res)
	}
	return r, nil
}

// Split reads a list of roots separated by the OS list separator (":").
func Split(s string) []string {
	var out []string
	for _, p := range filepath.SplitList(s) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ErrOutside is a path that is not a file under a root.
var ErrOutside = errors.New("not a file under the media roots")

// Check is p resolved, when it is a plain absolute path (api.CheckPath) to a
// regular file under one of the roots; an error wraps api.ErrBadRequest and
// ErrOutside for one that is not, or the stat's error for one that is
// missing.
func (r *Roots) Check(p string) (string, error) {
	if err := api.CheckPath(p); err != nil {
		return "", err
	}
	res, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(res)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %w: %q is no regular file", api.ErrBadRequest, ErrOutside, p)
	}
	for _, root := range r.roots {
		if strings.HasPrefix(res, root+string(filepath.Separator)) {
			return res, nil
		}
	}
	return "", fmt.Errorf("%w: %w: %q", api.ErrBadRequest, ErrOutside, p)
}

// List is the resolved roots.
func (r *Roots) List() []string { return append([]string(nil), r.roots...) }
