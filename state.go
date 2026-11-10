package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

// The status values and JSON keys are the on-disk format of registro.json and are kept
// as they are so existing registries stay readable.
const (
	statusConverted = "convertido"
	statusSkipped   = "pulado"
	statusFailed    = "falhou"
)

type Entry struct {
	Status   string    `json:"status"`
	OrigSize int64     `json:"tamanho_original"`
	NewSize  int64     `json:"tamanho_novo,omitempty"`
	MTime    int64     `json:"data_arquivo"`
	Reason   string    `json:"motivo,omitempty"`
	When     time.Time `json:"quando"`
}

// State is the registry of what has already been processed, keyed by path on the phone.
type State struct {
	path    string
	Entries map[string]Entry
}

func loadState(path string) (*State, error) {
	s := &State{path: path, Entries: map[string]Entry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.Entries); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *State) save() error {
	data, err := json.MarshalIndent(s.Entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// done reports whether the file was already handled and is still as we left it.
// Failures do not count: they are retried on the next run.
func (s *State) done(f RemoteFile) bool {
	e, ok := s.Entries[f.Path]
	if !ok {
		return false
	}
	switch e.Status {
	case statusConverted:
		return f.Size == e.NewSize
	case statusSkipped:
		return f.Size == e.OrigSize
	}
	return false
}
