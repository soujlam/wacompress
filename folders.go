package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// deviceConfig is the configuration of one device, stored as pastas.json in its folder.
// The file name and JSON key are the on-disk format and are kept as they are.
type deviceConfig struct {
	// Extra phone folders to process besides the WhatsApp ones. Subfolders are included.
	Folders []string `json:"pastas"`
}

func configPath(devDir string) string { return filepath.Join(devDir, "pastas.json") }

func loadDeviceConfig(p string) (deviceConfig, error) {
	var c deviceConfig
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

func (c deviceConfig) save(p string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// isUnder reports whether p is the folder dir or is inside it.
func isUnder(p, dir string) bool {
	dir = strings.TrimRight(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// internalDir reports whether the folder is internal to the system or to apps: app and game
// data under /sdcard/Android (replacing those files can break the app), trash and hidden folders.
func internalDir(dir string) bool {
	if isUnder(dir, "/sdcard/Android") {
		return true
	}
	for _, part := range strings.Split(dir, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

type folderInfo struct {
	Dir   string
	Count int
	Size  int64
}

// otherFolders groups the videos by folder, leaving out folders that are already covered.
// The result is sorted from the heaviest folder to the lightest.
func otherFolders(files []RemoteFile, covered []string) []folderInfo {
	by := map[string]*folderInfo{}
next:
	for _, f := range files {
		dir := path.Dir(f.Path)
		if internalDir(dir) {
			continue
		}
		for _, c := range covered {
			if isUnder(dir, c) {
				continue next
			}
		}
		fi := by[dir]
		if fi == nil {
			fi = &folderInfo{Dir: dir}
			by[dir] = fi
		}
		fi.Count++
		fi.Size += f.Size
	}
	out := make([]folderInfo, 0, len(by))
	for _, fi := range by {
		out = append(out, *fi)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

// parseSelection reads the user's answer: numbers (1 3 5), ranges (2-4), "all" or empty.
// It returns the chosen indexes, starting at 0.
func parseSelection(s string, n int) ([]int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "n", "no", "none":
		return nil, nil
	case "all", "a":
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		return all, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		lo, hi, isRange := strings.Cut(tok, "-")
		a, err := strconv.Atoi(lo)
		b := a
		if err == nil && isRange {
			b, err = strconv.Atoi(hi)
		}
		if err != nil || a < 1 || b > n || a > b {
			return nil, fmt.Errorf("could not understand %q: use numbers from 1 to %d, ranges like 2-4, or \"all\"", tok, n)
		}
		for i := a; i <= b; i++ {
			if !seen[i] {
				seen[i] = true
				out = append(out, i-1)
			}
		}
	}
	return out, nil
}

// runFolders looks for the other folders on the phone that hold videos and asks which
// ones should be added to the device configuration.
func runFolders(ctx context.Context, dev Device, devDir string) error {
	cfg, err := loadDeviceConfig(configPath(devDir))
	if err != nil {
		return err
	}
	fmt.Printf("Device: %s\n", filepath.Base(devDir))
	if len(cfg.Folders) > 0 {
		fmt.Println("Extra folders already configured:")
		for _, p := range cfg.Folders {
			fmt.Println("  " + p)
		}
	}
	fmt.Println("Looking for videos on the phone...")
	files, err := dev.listVideos(ctx, []string{"/sdcard/"})
	if err != nil {
		return err
	}
	folders := otherFolders(files, append(append([]string{}, whatsappDirs...), cfg.Folders...))
	if len(folders) == 0 {
		fmt.Println("There are no other folders with videos besides the ones already processed.")
		return nil
	}
	var total int64
	fmt.Println("\nOther folders with videos (app-internal data, trash and hidden folders left out):")
	for i, f := range folders {
		total += f.Size
		fmt.Printf("%3d) %9s  %5d videos  %s\n", i+1, fmtSize(f.Size), f.Count, f.Dir)
	}
	fmt.Printf("     %9s  in total\n", fmtSize(total))
	fmt.Println("\nVideos in the chosen folders are converted and replaced on the phone, like the WhatsApp ones")
	fmt.Println("(the original goes to the backup). Subfolders are included.")
	fmt.Print("Which ones to add? (numbers, ranges like 2-4, \"all\", or Enter for none): ")

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if ctx.Err() != nil {
		return ctx.Err()
	}
	sel, err := parseSelection(line, len(folders))
	if err != nil {
		return err
	}
	if len(sel) == 0 {
		fmt.Println("No folder added.")
		return nil
	}
	for _, i := range sel {
		cfg.Folders = append(cfg.Folders, folders[i].Dir)
	}
	sort.Strings(cfg.Folders)
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		return err
	}
	if err := cfg.save(configPath(devDir)); err != nil {
		return err
	}
	fmt.Printf("%d folders added. Configuration saved to %s\n", len(sel), configPath(devDir))
	fmt.Println("To remove a folder, delete its line from that file.")
	return nil
}
