package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Folders where WhatsApp keeps videos, depending on the Android and app version.
// Left out on purpose: ".Statuses" (temporary status cache).
var whatsappDirs = mediaDirs(
	"/sdcard/Android/media/com.whatsapp/WhatsApp/Media/WhatsApp",
	"/sdcard/Android/media/com.whatsapp.w4b/WhatsApp Business/Media/WhatsApp Business",
	"/sdcard/WhatsApp/Media/WhatsApp", // legacy path
)

// mediaDirs builds the video folders from each install prefix: chat videos, videos sent
// as documents, video notes and animated GIFs (which WhatsApp stores as .mp4).
func mediaDirs(prefixes ...string) []string {
	var dirs []string
	for _, p := range prefixes {
		for _, kind := range []string{"Video", "Documents", "Video Notes", "Animated Gifs"} {
			dirs = append(dirs, p+" "+kind)
		}
	}
	return dirs
}

// Accepted extensions. All of them can hold H.265, so the original extension is kept.
var videoExts = []string{".mp4", ".mov", ".mkv"}

const remoteTmpSuffix = ".wacompress.tmp"

type Device struct{ Serial string }

type RemoteFile struct {
	Path  string
	Size  int64
	MTime int64 // seconds since 1970
}

func (d Device) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "adb", append([]string{"-s", d.Serial}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("adb %s: %v: %s", args[0], err, lastLine(msg))
	}
	return out.String(), nil
}

func (d Device) shell(ctx context.Context, script string) (string, error) {
	return d.run(ctx, "shell", script)
}

// parseDevices reads the output of `adb devices` and returns serial -> state.
func parseDevices(out string) map[string]string {
	devs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.HasPrefix(line, "List of") || strings.HasPrefix(line, "*") {
			continue
		}
		devs[f[0]] = f[1]
	}
	return devs
}

func pickDevice(ctx context.Context, serial string) (Device, error) {
	out, err := exec.CommandContext(ctx, "adb", "devices").Output()
	if err != nil {
		return Device{}, fmt.Errorf("could not run `adb devices`: %v", err)
	}
	devs := parseDevices(string(out))
	check := func(s string) (Device, error) {
		switch devs[s] {
		case "device":
			return Device{s}, nil
		case "unauthorized":
			return Device{}, fmt.Errorf("device %s is unauthorized: accept the USB debugging prompt on the phone screen", s)
		case "":
			return Device{}, fmt.Errorf("device %s not found", s)
		default:
			return Device{}, fmt.Errorf("device %s is in state %q", s, devs[s])
		}
	}
	if serial != "" {
		return check(serial)
	}
	switch len(devs) {
	case 0:
		return Device{}, errors.New("no phone found: connect the cable and enable USB debugging")
	case 1:
		for s := range devs {
			return check(s)
		}
	}
	var list []string
	for s := range devs {
		list = append(list, s)
	}
	return Device{}, fmt.Errorf("more than one device connected (%s): pick one with -serial", strings.Join(list, ", "))
}

// folderName returns a stable folder name for the device: model and serial number.
// It uses the device's own serial number, which is the same over cable and Wi-Fi adb.
func (d Device) folderName(ctx context.Context) string {
	out, _ := d.shell(ctx, "getprop ro.product.model; getprop ro.serialno")
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	model, serial := "", ""
	if len(lines) > 0 {
		model = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		serial = strings.TrimSpace(lines[1])
	}
	if serial == "" {
		serial = d.Serial
	}
	if model == "" {
		return sanitizeName(serial)
	}
	return sanitizeName(model + "-" + serial)
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
}

// connected reports whether the device still answers adb.
func (d Device) connected(ctx context.Context) bool {
	out, err := d.run(ctx, "get-state")
	return err == nil && strings.TrimSpace(out) == "device"
}

// shQuote quotes a string for the Android shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseListing reads "size|mtime|path" lines.
func parseListing(out string) []RemoteFile {
	var files []RemoteFile
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		size, err1 := strconv.ParseInt(parts[0], 10, 64)
		mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil || parts[2] == "" {
			continue
		}
		files = append(files, RemoteFile{Path: parts[2], Size: size, MTime: mtime})
	}
	return files
}

func (d Device) listVideos(ctx context.Context, dirs []string) ([]RemoteFile, error) {
	var names []string
	for _, e := range videoExts {
		names = append(names, "-iname "+shQuote("*"+e))
	}
	var files []RemoteFile
	seen := map[string]bool{}
	for _, dir := range dirs {
		script := fmt.Sprintf(`if [ -d %s ]; then find %s -type f \( %s \) -exec stat -c '%%s|%%Y|%%n' {} +; fi`,
			shQuote(dir), shQuote(dir), strings.Join(names, " -o "))
		out, err := d.shell(ctx, script)
		if err != nil && strings.TrimSpace(out) == "" {
			return nil, fmt.Errorf("listing %s: %v", dir, err)
		}
		for _, f := range parseListing(out) {
			if !seen[f.Path] { // configured folders may overlap
				seen[f.Path] = true
				files = append(files, f)
			}
		}
	}
	return files, nil
}

func (d Device) pull(ctx context.Context, remote, local string) error {
	_, err := d.run(ctx, "pull", "-a", remote, local)
	return err
}

func (d Device) push(ctx context.Context, local, remote string) error {
	_, err := d.run(ctx, "push", local, remote)
	return err
}

// stat returns the size and modification time of a file on the phone.
func (d Device) stat(ctx context.Context, remote string) (size, mtime int64, err error) {
	out, err := d.shell(ctx, "stat -c '%s|%Y' "+shQuote(remote))
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected stat output: %q", out)
	}
	size, err1 := strconv.ParseInt(parts[0], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("unexpected stat output: %q", out)
	}
	return size, mtime, nil
}

// replace uploads local to the phone in place of remote, with modification time mtime.
// The upload goes to a temporary name; the original is only swapped (by rename) after
// the size and time have been checked.
func (d Device) replace(ctx context.Context, local, remote string, localSize, mtime int64) (err error) {
	tmp := remote + remoteTmpSuffix
	defer func() {
		if err != nil {
			c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			d.shell(c, "rm -f "+shQuote(tmp))
		}
	}()
	if err = d.push(ctx, local, tmp); err != nil {
		return err
	}
	size, mt, err := d.stat(ctx, tmp)
	if err != nil {
		return err
	}
	if size != localSize {
		return fmt.Errorf("incomplete upload: %d of %d bytes", size, localSize)
	}
	if mt != mtime {
		d.shell(ctx, fmt.Sprintf("touch -m -d @%d %s", mtime, shQuote(tmp)))
		if _, mt, err = d.stat(ctx, tmp); err != nil {
			return err
		}
		if mt != mtime {
			return errors.New("could not preserve the file time on the phone")
		}
	}
	_, err = d.shell(ctx, fmt.Sprintf("mv -f %s %s", shQuote(tmp), shQuote(remote)))
	return err
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
