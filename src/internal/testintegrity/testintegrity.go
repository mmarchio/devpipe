// Package testintegrity fingerprints a work tree's test suite (the
// git-gated TDD workflow): the QA role's committed tests are hashed once,
// and every later fingerprint of the coder's work tree must match it
// before the overseer runs validation.
//
// The fingerprint is a crypto/md5 digest over every test file, visited in
// sorted slash-separated path order (filepath.Walk's order is not relied
// on). Each file contributes its path, its length and its content, so an
// added, deleted, renamed or edited test file always changes the digest.
package testintegrity

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Patterns are the test file name patterns (path.Match against the base
// name): Go tests and the JavaScript test / spec conventions.
var Patterns = []string{"*_test.go", "*.test.js", "*.spec.js"}

// skippedDirs are never searched: repository metadata and dependency trees.
var skippedDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true}

// Fingerprint is a test suite's digest and the files it covers.
type Fingerprint struct {
	MD5   string
	Files []string // slash-separated, relative to the root, sorted
}

// IsTest reports whether the base name of p is a test file name.
func IsTest(p string) bool {
	base := path.Base(filepath.ToSlash(p))
	for _, pat := range Patterns {
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// Compute fingerprints the test files under root. exclude are root-relative
// paths (files or directories) that are never part of the suite, such as
// the worker's reserved paths.
func Compute(root string, exclude []string) (Fingerprint, error) {
	skip := map[string]bool{}
	for _, e := range exclude {
		skip[strings.TrimSuffix(filepath.ToSlash(filepath.Clean(e)), "/")] = true
	}
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (skippedDirs[d.Name()] || skip[rel]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !skip[rel] && d.Type().IsRegular() && IsTest(rel) {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return Fingerprint{}, fmt.Errorf("testintegrity: walk %s: %w", root, err)
	}
	sort.Strings(files)
	h := md5.New()
	for _, rel := range files {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return Fingerprint{}, fmt.Errorf("testintegrity: %w", err)
		}
		info, err := f.Stat()
		if err == nil {
			fmt.Fprintf(h, "%s\x00%d\x00", rel, info.Size())
			_, err = io.Copy(h, f)
		}
		f.Close()
		if err != nil {
			return Fingerprint{}, fmt.Errorf("testintegrity: %s: %w", rel, err)
		}
	}
	return Fingerprint{MD5: hex.EncodeToString(h.Sum(nil)), Files: files}, nil
}
