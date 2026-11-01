//go:build windows

package main

import (
	"io"
	"os"
	"path/filepath"
)

// Legacy: launcher v1.0.4 patched intro movies on disk (this crashed the game).
// Intro skipping now uses the game's own Client.DisableMovies setting (see gamecfg.go);
// this file only restores any movies patched by v1.0.4.

var introMovies = []string{"ea_logo_hd.res", "dolbydigital.res"}

func moviesDir() string {
	return filepath.Join(settings.GameData, "Output", "xenon", "movies")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// restorePatchedIntros puts back original movies saved by launcher v1.0.4.
func restorePatchedIntros() error {
	dir := moviesDir()
	if !dirExists(dir) {
		return nil
	}
	backup := filepath.Join(dir, "_original")
	var firstErr error
	for _, name := range introMovies {
		saved := filepath.Join(backup, name)
		if !fileExists(saved) {
			continue
		}
		if err := copyFile(saved, filepath.Join(dir, name)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_ = os.Remove(saved)
	}
	_ = os.Remove(backup) // only succeeds when empty
	return firstErr
}
