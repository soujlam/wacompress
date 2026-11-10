package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// appConfig is the program configuration on this PC (shared by all devices).
type appConfig struct {
	Backup string `json:"backup"` // backup folder chosen with -set-backup
}

func appConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wacompress", "config.json"), nil
}

func loadAppConfig(p string) (appConfig, error) {
	var c appConfig
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("reading %s: %v", p, err)
	}
	return c, nil
}

func (c appConfig) save(p string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// resolveBackup picks the backup folder: the -backup option, else the configured one,
// else ~/wacompress-backup. configured reports whether it came from the configuration;
// in that case the folder must exist (see checkBackupDir).
func resolveBackup(flagValue string, cfg appConfig, home string) (dir string, configured bool) {
	switch {
	case flagValue != "":
		return flagValue, false
	case cfg.Backup != "":
		return cfg.Backup, true
	}
	return filepath.Join(home, "wacompress-backup"), false
}

// checkBackupDir rejects a configured folder that does not exist. It usually lives on an
// external disk: if the disk is not connected, creating an empty backup somewhere else
// would make the program forget what was already converted and convert everything again.
func checkBackupDir(dir string) error {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("backup folder %s not found. Is the disk connected?\n"+
			"To use another folder just this once: -backup <folder>. To change it for good: -set-backup <folder>", dir)
	}
	return nil
}

// setBackup saves the backup folder in the configuration, creating the folder if needed.
func setBackup(cfgPath, dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	cfg, err := loadAppConfig(cfgPath)
	if err != nil {
		return "", err
	}
	cfg.Backup = abs
	return abs, cfg.save(cfgPath)
}
