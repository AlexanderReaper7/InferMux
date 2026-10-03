package muxui

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RoutingFile is this host's llama-swap routing: which models may run at the
// same time, as `routing.router` groups or a matrix (0006, 9). One file in the
// models directory, edited as text.
const RoutingFile = "routing.yaml"

// Routing is RoutingFile's text, empty when there is none.
func (s *Store) Routing() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(s.ModelsDir, RoutingFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(raw), err
}

// SaveRouting replaces RoutingFile with text after llama-swap's loader has
// accepted the result. Blank text removes the file: every model in one swap
// group, llama-swap's default.
func (s *Store) SaveRouting(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := readModelFiles(s.ModelsDir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) != "" {
		return s.apply(files, map[string][]byte{RoutingFile: []byte(text)})
	}
	for _, f := range files {
		if f.name == RoutingFile {
			return s.apply(files, map[string][]byte{RoutingFile: nil})
		}
	}
	return nil
}
