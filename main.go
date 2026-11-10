// wacompress re-encodes to H.265 the WhatsApp videos of an Android phone connected by
// cable (adb) and puts the smaller file in place of the original, keeping the original
// in a backup folder on the PC.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	backup string
	enc    Encoder
}

func main() { os.Exit(run()) }

func run() int {
	home, _ := os.UserHomeDir()
	backupFlag := flag.String("backup", "", "backup folder for this run only (default: the one saved with -set-backup, or ~/wacompress-backup)")
	setBackupDir := flag.String("set-backup", "", "save the backup folder to use from now on (e.g. on an external disk) and exit")
	crf := flag.Int("crf", 26, "H.265 quality on the CPU (lower = better quality, bigger file)")
	preset := flag.String("preset", "medium", "x265 speed (ultrafast ... veryslow)")
	gpu := flag.Bool("gpu", false, "convert on the GPU (VAAPI): much faster, slightly bigger file")
	qp := flag.Int("qp", 33, "quality on the GPU (lower = better quality, bigger file)")
	minSizeStr := flag.String("min-size", "0", "ignore videos smaller than this")
	limit := flag.Int("limit", 0, "process only N videos (0 = all)")
	dryRun := flag.Bool("dry-run", false, "only list what would be done")
	serial := flag.String("serial", "", "device serial, if more than one is connected")
	evaluate := flag.Bool("evaluate", false, "do not touch the phone: compare original, CPU and GPU on videos from the backup (or on the given files)")
	samples := flag.Int("samples", 5, "how many backup videos to use with -evaluate")
	folders := flag.Bool("folders", false, "list the other phone folders that hold videos and ask which ones to add to the device configuration")
	freed := flag.Bool("freed", false, "only show the space already freed on each device, from the registry")
	flag.Parse()

	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
		return 1
	}

	cfgPath, err := appConfigPath()
	if err != nil {
		return fail("%v", err)
	}
	if *setBackupDir != "" {
		dir, err := setBackup(cfgPath, *setBackupDir)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Printf("Backup folder: %s (saved to %s)\n", dir, cfgPath)
		return 0
	}
	appCfg, err := loadAppConfig(cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	backupDir, configured := resolveBackup(*backupFlag, appCfg, home)
	if configured {
		if err := checkBackupDir(backupDir); err != nil {
			return fail("%v", err)
		}
	}
	backup := &backupDir

	minSize, err := parseSize(*minSizeStr)
	if err != nil {
		return fail("-min-size: %v", err)
	}
	enc := Encoder{GPU: *gpu, CRF: *crf, Preset: *preset, QP: *qp}
	if *freed {
		if err := showFreed(*backup); err != nil {
			return fail("%v", err)
		}
		return 0
	}
	if *evaluate {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runEval(ctx, *backup, flag.Args(), *samples, enc); err != nil {
			return fail("%v", err)
		}
		return 0
	}
	tools := []string{"adb"}
	if !*dryRun {
		tools = append(tools, "ffmpeg", "ffprobe")
	}
	for _, t := range tools {
		if _, err := exec.LookPath(t); err != nil {
			return fail("%s not found. Install it with: sudo apt install %s", t, map[string]string{"adb": "adb", "ffmpeg": "ffmpeg", "ffprobe": "ffmpeg"}[t])
		}
	}
	if !*dryRun {
		if out, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output(); !strings.Contains(string(out), enc.codec()) {
			return fail("the installed ffmpeg does not have the %s encoder", enc.codec())
		}
		if enc.GPU {
			if runtime.GOOS != "linux" {
				return fail("-gpu uses VAAPI and only works on Linux; run without -gpu to convert on the CPU")
			}
			if _, err := os.Stat(vaapiDevice); err != nil {
				return fail("GPU not found at %s", vaapiDevice)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dev, err := pickDevice(ctx, *serial)
	if err != nil {
		return fail("%v", err)
	}
	// Each device has its own folder inside the backup: originals and registry never mix.
	devDir := filepath.Join(*backup, dev.folderName(ctx))
	if *folders {
		if err := runFolders(ctx, dev, devDir); err != nil {
			return fail("%v", err)
		}
		return 0
	}
	devCfg, err := loadDeviceConfig(configPath(devDir))
	if err != nil {
		return fail("%v", err)
	}
	files, err := dev.listVideos(ctx, append(append([]string{}, whatsappDirs...), devCfg.Folders...))
	if err != nil {
		return fail("%v", err)
	}
	statePath := filepath.Join(devDir, "registro.json")
	if *dryRun {
		// Touch nothing: if the backup is still in the old layout, just read the registry from there.
		if _, err := os.Stat(statePath); err != nil {
			if old := filepath.Join(*backup, "registro.json"); exists(old) {
				statePath = old
			}
		}
	} else {
		moved, err := migrateLegacy(*backup, devDir)
		if err != nil {
			return fail("%v", err)
		}
		if moved {
			fmt.Printf("Old backup moved to %s\n", devDir)
		}
		if err := os.MkdirAll(devDir, 0o755); err != nil {
			return fail("%v", err)
		}
	}
	fmt.Printf("Device: %s | backup: %s\n", filepath.Base(devDir), devDir)
	if len(devCfg.Folders) > 0 {
		fmt.Printf("Extra folders configured: %d (see or change them with -folders)\n", len(devCfg.Folders))
	}
	st, err := loadState(statePath)
	if err != nil {
		return fail("reading the registry: %v", err)
	}

	var pending []RemoteFile
	var already, small int
	var allBytes int64
	for _, f := range files {
		allBytes += f.Size
		switch {
		case st.done(f):
			already++
		case f.Size < minSize:
			small++
		default:
			pending = append(pending, f)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Size > pending[j].Size })
	if *limit > 0 && len(pending) > *limit {
		pending = pending[:*limit]
	}
	var total int64
	for _, f := range pending {
		total += f.Size
	}

	fmt.Printf("%d videos on the phone, %s (%d already processed, %d too small)\n",
		len(files), fmtSize(allBytes), already, small)
	fmt.Printf("To process: %d videos, %s\n", len(pending), fmtSize(total))
	var byPar [maxParallel + 1]int
	for _, f := range pending {
		byPar[parallelFor(f.Size)]++
	}
	fmt.Printf("  %d of %s or more (1 at a time), %d below %s (2 at a time), %d below %s (4 at a time)\n",
		byPar[1], fmtSize(mediumBelow), byPar[2], fmtSize(mediumBelow), byPar[4], fmtSize(smallBelow))
	if *dryRun {
		for i, f := range pending {
			fmt.Printf("[%d/%d] %s  %s\n", i+1, len(pending), path.Base(f.Path), fmtSize(f.Size))
		}
		return 0
	}
	if len(pending) == 0 {
		return 0
	}

	cfg := config{backup: devDir, enc: enc}
	os.RemoveAll(filepath.Join(devDir, ".work"))
	u := newUI(len(pending), total)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu                         sync.Mutex // guards the registry and the counters
		wg                         sync.WaitGroup
		converted, skipped, failed int
		saved                      int64
		lost                       bool
		saveErr                    error
	)
	// Each video takes slots according to its size: a large one takes them all (runs alone),
	// medium ones run 2 at a time and small ones 4 at a time.
	sem := make(chan struct{}, maxParallel)
dispatch:
	for i, f := range pending {
		w := maxParallel / parallelFor(f.Size)
		for k := 0; k < w; k++ {
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				for ; k > 0; k-- {
					<-sem
				}
				break dispatch
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				for k := 0; k < w; k++ {
					<-sem
				}
			}()
			j := u.begin(i+1, f)
			e := processOne(runCtx, dev, f, cfg, u, j)

			mu.Lock()
			defer mu.Unlock()
			name := path.Base(f.Path)
			var freed int64
			defer func() { u.finish(j, freed) }()
			// Interrupted: do not record it, retry next time. A video that was already
			// replaced on the phone is recorded anyway.
			if runCtx.Err() != nil && e.Status != statusConverted {
				return
			}
			if e.Status == statusFailed && !dev.connected(ctx) {
				// Phone disconnected: stop instead of marking the whole queue as failed.
				if !lost {
					u.line("[%d/%d] %s  FAILED: %s", i+1, len(pending), name, e.Reason)
				}
				lost = true
				cancel()
				return
			}
			st.Entries[f.Path] = e
			if err := st.save(); err != nil {
				saveErr = err
				cancel()
				return
			}
			switch e.Status {
			case statusConverted:
				converted++
				freed = e.OrigSize - e.NewSize
				saved += freed
				u.line("[%d/%d] %s  %s -> %s (-%d%%)", i+1, len(pending), name,
					fmtSize(e.OrigSize), fmtSize(e.NewSize), 100-int(100*e.NewSize/e.OrigSize))
			case statusSkipped:
				skipped++
				u.line("[%d/%d] %s  skipped: %s", i+1, len(pending), name, e.Reason)
			default:
				failed++
				u.line("[%d/%d] %s  FAILED: %s", i+1, len(pending), name, e.Reason)
			}
		}()
	}
	wg.Wait()
	u.clear()
	os.RemoveAll(filepath.Join(devDir, ".work"))
	if saveErr != nil {
		return fail("saving the registry: %v", saveErr)
	}

	if ctx.Err() != nil {
		fmt.Println("Interrupted. Run again to continue where it stopped.")
	}
	if lost {
		fmt.Println("The phone was disconnected. Reconnect it and run again to continue where it stopped.")
	}
	fmt.Printf("Converted: %d | skipped: %d | failed: %d | space freed: %s | time: %s\n",
		converted, skipped, failed, fmtSize(saved), fmtDur(time.Since(u.start)))
	fmt.Printf("Originals kept in %s\n", filepath.Join(devDir, "originais"))
	if failed > 0 || lost || ctx.Err() != nil {
		return 1
	}
	return 0
}

// Parallelism by size: small videos convert fast and several can run at once.
const (
	maxParallel = 4
	mediumBelow = 15 << 20 // below this: 2 at a time
	smallBelow  = 5 << 20  // below this: 4 at a time
)

// parallelFor returns how many videos of this size may be processed at the same time.
func parallelFor(size int64) int {
	switch {
	case size < smallBelow:
		return 4
	case size < mediumBelow:
		return 2
	}
	return 1
}

// freedBytes adds up the space freed by the converted videos of a registry.
func freedBytes(st *State) (n int, freed int64) {
	for _, e := range st.Entries {
		if e.Status == statusConverted {
			n++
			freed += e.OrigSize - e.NewSize
		}
	}
	return n, freed
}

// showFreed shows the space freed on each device that has a registry in the backup.
// It does not need the phone connected.
func showFreed(backup string) error {
	paths, _ := filepath.Glob(filepath.Join(backup, "*", "registro.json"))
	if len(paths) == 0 {
		return fmt.Errorf("no registry found in %s", backup)
	}
	for _, p := range paths {
		st, err := loadState(p)
		if err != nil {
			return fmt.Errorf("reading %s: %v", p, err)
		}
		n, freed := freedBytes(st)
		fmt.Printf("%s: %s freed (%d videos converted)\n", filepath.Base(filepath.Dir(p)), fmtSize(freed), n)
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// migrateLegacy moves a backup in the old layout (originals and registry directly at the
// root, from before there was one folder per device) into the connected device's folder.
func migrateLegacy(root, devDir string) (moved bool, err error) {
	names := []string{"registro.json", "originais"}
	var found []string
	for _, n := range names {
		if exists(filepath.Join(root, n)) {
			found = append(found, n)
		}
	}
	if len(found) == 0 {
		return false, nil
	}
	if exists(filepath.Join(root, ".work")) {
		return false, fmt.Errorf("there is a backup in the old layout in %s and another conversion seems to be running; "+
			"wait for it to finish (if it was killed, delete %s)", root, filepath.Join(root, ".work"))
	}
	for _, n := range names {
		if exists(filepath.Join(devDir, n)) {
			return false, fmt.Errorf("there is a backup in the old layout in %s and also one in %s; merge them by hand", root, devDir)
		}
	}
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		return false, err
	}
	for _, n := range found {
		if err := os.Rename(filepath.Join(root, n), filepath.Join(devDir, n)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// processOne handles one video. The original on the phone is only replaced if the
// converted file passes every check.
func processOne(ctx context.Context, dev Device, f RemoteFile, cfg config, u *ui, j *job) Entry {
	e := Entry{OrigSize: f.Size, MTime: f.MTime, When: time.Now()}
	failed := func(err error) Entry {
		e.Status, e.Reason = statusFailed, err.Error()
		return e
	}
	skipped := func(reason string) Entry {
		e.Status, e.Reason = statusSkipped, reason
		return e
	}

	ext := strings.ToLower(path.Ext(f.Path))                        // the original extension is kept
	work := filepath.Join(cfg.backup, ".work", strconv.Itoa(j.idx)) // one folder per video in progress
	defer os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return failed(err)
	}
	in := filepath.Join(work, "in"+ext)
	out := filepath.Join(work, "out"+ext)

	u.status(j, "downloading", -1, "")
	if err := dev.pull(ctx, f.Path, in); err != nil {
		return failed(err)
	}
	if fi, err := os.Stat(in); err != nil || fi.Size() != f.Size {
		return failed(errors.New("incomplete download"))
	}
	orig, err := probe(ctx, in)
	if err != nil {
		return failed(err)
	}
	switch orig.VideoCodec {
	case "":
		return skipped("no video stream")
	case "av1":
		return skipped("already in av1")
	}
	if why := hdrProblem(orig, nil); why != "" {
		return skipped(why)
	}
	if why := audioProblem(orig, ext); why != "" {
		return skipped(why)
	}

	// Store the original in the backup before anything else.
	bk := filepath.Join(cfg.backup, "originais", filepath.FromSlash(strings.TrimPrefix(f.Path, "/")))
	replaced, keepBackup := false, false
	if fi, err := os.Stat(bk); err == nil && fi.Size() != f.Size {
		// The video on the phone was converted before and the backup is the true original:
		// convert from it, so quality loss does not pile up.
		bp, err := probe(ctx, bk)
		if err != nil || math.Abs(bp.Duration-orig.Duration) > 0.5 {
			return failed(fmt.Errorf("a different backup already exists at %s", bk))
		}
		orig, keepBackup = bp, true
		os.Remove(in)
	} else {
		if err := os.MkdirAll(filepath.Dir(bk), 0o755); err != nil {
			return failed(err)
		}
		if err := os.Rename(in, bk); err != nil {
			return failed(err)
		}
	}
	defer func() {
		if !replaced && !keepBackup { // the original is still on the phone; the backup is not needed
			os.Remove(bk)
		}
	}()

	convStart := time.Now()
	err = convert(ctx, bk, out, cfg.enc, orig, func(frac float64, speed string) {
		u.convProgress(j, frac, speed, time.Since(convStart))
	})
	if err != nil {
		return failed(err)
	}
	res, err := probe(ctx, out)
	if err != nil {
		return failed(err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		return failed(err)
	}
	if ok, reason := checkResult(orig, res, f.Size, fi.Size()); !ok {
		return skipped(reason)
	}

	mt := time.Unix(f.MTime, 0)
	if err := os.Chtimes(out, mt, mt); err != nil {
		return failed(err)
	}
	u.status(j, "uploading", -1, "")
	if err := dev.replace(ctx, out, f.Path, fi.Size(), f.MTime); err != nil {
		return failed(err)
	}
	replaced = true
	e.Status, e.NewSize = statusConverted, fi.Size()
	if keepBackup {
		// Reconversion from the backup: the true original size is the backup's, so the
		// freed space counts from the first original.
		if bfi, err := os.Stat(bk); err == nil {
			e.OrigSize = bfi.Size()
		}
	}
	return e
}

// ---- on-screen progress ----

// job is a video in progress.
type job struct {
	idx    int
	f      RemoteFile
	stage  string
	frac   float64 // converted fraction; < 0 when the stage has no percentage
	detail string
}

type ui struct {
	mu        sync.Mutex
	tty       bool
	start     time.Time
	n         int
	total     int64 // bytes to process
	doneBytes int64 // bytes of the videos already finished
	saved     int64
	jobs      []*job
	lastDraw  time.Time
}

func newUI(n int, total int64) *ui {
	fi, err := os.Stdout.Stat()
	return &ui{tty: err == nil && fi.Mode()&os.ModeCharDevice != 0, start: time.Now(), n: n, total: total}
}

func (u *ui) begin(idx int, f RemoteFile) *job {
	u.mu.Lock()
	defer u.mu.Unlock()
	j := &job{idx: idx, f: f, stage: "queued", frac: -1}
	u.jobs = append(u.jobs, j)
	u.draw()
	return j
}

// finish removes the video from the progress line and adds up the totals.
func (u *ui) finish(j *job, freed int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, x := range u.jobs {
		if x == j {
			u.jobs = append(u.jobs[:i], u.jobs[i+1:]...)
			break
		}
	}
	u.doneBytes += j.f.Size
	u.saved += freed
	u.draw()
}

func (u *ui) clear() {
	if u.tty {
		fmt.Print("\r\x1b[K")
	}
}

// line prints a permanent line.
func (u *ui) line(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clear()
	fmt.Printf(format+"\n", a...)
	u.draw()
}

func (u *ui) convProgress(j *job, frac float64, speed string, elapsed time.Duration) {
	detail := fmt.Sprintf("%d%%", int(frac*100))
	if speed != "" && speed != "N/A" {
		detail += "  " + speed
	}
	if frac > 0.01 {
		detail += "  left " + fmtDur(time.Duration(float64(elapsed)*(1-frac)/frac))
	}
	u.status(j, "converting", frac, detail)
}

// status updates the stage of a video. frac < 0 when the stage has no percentage.
func (u *ui) status(j *job, stage string, frac float64, detail string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	changed := j.stage != stage
	j.stage, j.frac, j.detail = stage, frac, detail
	if !changed && time.Since(u.lastDraw) < 200*time.Millisecond {
		return
	}
	u.draw()
}

// draw repaints the progress line. Called with u.mu held.
func (u *ui) draw() {
	if !u.tty || len(u.jobs) == 0 {
		return
	}
	u.lastDraw = time.Now()
	done := float64(u.doneBytes)
	for _, j := range u.jobs {
		if j.frac > 0 {
			done += j.frac * float64(j.f.Size)
		}
	}
	var s string
	if len(u.jobs) == 1 {
		j := u.jobs[0]
		s = fmt.Sprintf("[%d/%d] %s  %s", j.idx, u.n, shorten(path.Base(j.f.Path), 32), j.stage)
		if j.detail != "" {
			s += " " + j.detail
		}
	} else {
		// Several at once: show only the number and the stage (or percentage) of each.
		parts := make([]string, len(u.jobs))
		for i, j := range u.jobs {
			st := j.stage
			if j.frac >= 0 {
				st = fmt.Sprintf("%d%%", int(j.frac*100))
			}
			parts[i] = fmt.Sprintf("[%d] %s", j.idx, st)
		}
		s = fmt.Sprintf("%d at once: %s", len(u.jobs), strings.Join(parts, "  "))
	}
	s += fmt.Sprintf(" | total %s of %s, freed %s", fmtSize(int64(done)), fmtSize(u.total), fmtSize(u.saved))
	if done > float64(u.total)*0.01 && done < float64(u.total) {
		rest := time.Duration(float64(time.Since(u.start)) * (float64(u.total) - done) / done)
		s += ", left ~" + fmtDur(rest)
	}
	// No auto-wrap, so the line can be repainted in place.
	fmt.Print("\r\x1b[K\x1b[?7l" + s + "\x1b[?7h")
}

// ---- formatting ----

func shorten(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func fmtSize(n int64) string {
	const k = 1024.0
	v := float64(n)
	switch {
	case v >= k*k*k:
		return fmt.Sprintf("%.1f GB", v/(k*k*k))
	case v >= k*k:
		return fmt.Sprintf("%.1f MB", v/(k*k))
	case v >= k:
		return fmt.Sprintf("%.0f KB", v/k)
	}
	return fmt.Sprintf("%d B", n)
}

func fmtDur(d time.Duration) string {
	sec := int(d.Seconds() + 0.5)
	if sec >= 3600 {
		return fmt.Sprintf("%dh%02d", sec/3600, sec%3600/60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// parseSize reads sizes such as "500K", "1MB", "2G" or a number of bytes.
func parseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(t, "B")
	mult := int64(1)
	switch {
	case strings.HasSuffix(t, "K"):
		mult, t = 1<<10, strings.TrimSuffix(t, "K")
	case strings.HasSuffix(t, "M"):
		mult, t = 1<<20, strings.TrimSuffix(t, "M")
	case strings.HasSuffix(t, "G"):
		mult, t = 1<<30, strings.TrimSuffix(t, "G")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size: %q", s)
	}
	return int64(v * float64(mult)), nil
}
