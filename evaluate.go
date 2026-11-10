package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Quality evaluation: converts the same videos on the CPU and on the GPU, with the same
// options as the real conversion, and measures how far each result is from the original.
// It never touches the phone: it uses the originals kept in the backup or the given files.

type sample struct {
	path string
	size int64
}

// backupSamples lists the originals kept in the backup of every device.
func backupSamples(backup string) ([]sample, error) {
	var files []sample
	dirs, _ := filepath.Glob(filepath.Join(backup, "*", "originais"))
	for _, d := range dirs {
		err := filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			ext := strings.ToLower(filepath.Ext(p))
			for _, v := range videoExts {
				if ext == v {
					if fi, err := e.Info(); err == nil {
						files = append(files, sample{p, fi.Size()})
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// pickSamples picks n files spread from the largest to the smallest.
func pickSamples(files []sample, n int) []sample {
	sort.Slice(files, func(i, j int) bool { return files[i].size > files[j].size })
	if n >= len(files) || n <= 0 {
		return files
	}
	if n == 1 {
		return []sample{files[len(files)/2]}
	}
	out := make([]sample, n)
	for i := range out {
		out[i] = files[i*(len(files)-1)/(n-1)]
	}
	return out
}

var (
	ssimRe = regexp.MustCompile(`SSIM .*All:([0-9.]+)`)
	psnrRe = regexp.MustCompile(`PSNR .*average:([0-9.]+|inf)`)
)

func parseQuality(log string) (ssim, psnr float64, err error) {
	ms, mp := ssimRe.FindStringSubmatch(log), psnrRe.FindStringSubmatch(log)
	if ms == nil || mp == nil {
		return 0, 0, errors.New("SSIM/PSNR not found in the ffmpeg output")
	}
	ssim, _ = strconv.ParseFloat(ms[1], 64)
	if mp[1] == "inf" {
		psnr = math.Inf(1)
	} else {
		psnr, _ = strconv.ParseFloat(mp[1], 64)
	}
	return ssim, psnr, nil
}

// quality compares the converted file with the original frame by frame.
// SSIM goes from 0 to 1 (1 = identical); PSNR is in dB (higher means closer).
func quality(ctx context.Context, conv, orig string) (ssim, psnr float64, err error) {
	const prep = "settb=AVTB,setpts=PTS-STARTPTS,format=yuv420p10le,split"
	graph := "[0:v]" + prep + "[a1][a2];[1:v]" + prep + "[b1][b2];[a1][b1]ssim;[a2][b2]psnr"
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-i", conv, "-i", orig,
		"-lavfi", graph, "-fps_mode", "passthrough", "-an", "-f", "null", "-")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		return 0, 0, fmt.Errorf("ffmpeg (comparison): %v: %s", err, lastLine(errb.String()))
	}
	return parseQuality(errb.String())
}

func verdict(ssim float64) string {
	switch {
	case ssim >= 0.98:
		return "practically identical"
	case ssim >= 0.95:
		return "loss hard to notice"
	case ssim >= 0.90:
		return "slight loss, visible on close inspection"
	}
	return "visible loss"
}

func fmtNum(v float64, decimals int) string {
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

func runEval(ctx context.Context, backup string, paths []string, n int, enc Encoder) error {
	for _, t := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(t); err != nil {
			return fmt.Errorf("%s not found. Install it with: sudo apt install ffmpeg", t)
		}
	}
	var files []sample
	if len(paths) > 0 {
		for _, p := range paths {
			fi, err := os.Stat(p)
			if err != nil {
				return err
			}
			files = append(files, sample{p, fi.Size()})
		}
	} else {
		all, err := backupSamples(backup)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return fmt.Errorf("no originals in %s; pass the video files on the command line", backup)
		}
		files = pickSamples(all, n)
		fmt.Printf("%d originals in the backup; evaluating %d, from the largest to the smallest\n", len(all), len(files))
	}

	tmp, err := os.MkdirTemp("", "wacompress-evaluate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	cpu, gpu := enc, enc
	cpu.GPU, gpu.GPU = false, true
	modes := []struct {
		name string
		enc  Encoder
	}{
		{fmt.Sprintf("CPU (libx265 crf %d %s)", enc.CRF, enc.Preset), cpu},
		{fmt.Sprintf("GPU (vaapi qp %d)", enc.QP), gpu},
	}
	if _, err := os.Stat(vaapiDevice); err != nil {
		fmt.Printf("GPU not found at %s: evaluating the CPU only\n", vaapiDevice)
		modes = modes[:1]
	}
	tty := false
	if fi, err := os.Stdout.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}

	type total struct {
		n          int
		orig, size int64
		ssim, psnr float64
		secs       float64
	}
	totals := make([]total, len(modes))
	for _, f := range files {
		src, err := probe(ctx, f.path)
		if err != nil {
			return err
		}
		kind := fmt.Sprintf("%s %d bits", src.VideoCodec, bitDepth(src.PixFmt))
		if src.isHDR() {
			kind += " HDR"
		}
		fmt.Printf("\n%s  %dx%d  %s  %s  %s\n", filepath.Base(f.path), src.Width, src.Height,
			fmtDur(time.Duration(src.Duration*float64(time.Second))), kind, fmtSize(f.size))
		for i, m := range modes {
			out := filepath.Join(tmp, "out"+strings.ToLower(filepath.Ext(f.path)))
			start := time.Now()
			err := convert(ctx, f.path, out, m.enc, src, func(frac float64, _ string) {
				if tty {
					fmt.Printf("\r\x1b[K  %-28s converting %d%%", m.name, int(frac*100))
				}
			})
			secs := time.Since(start).Seconds()
			if tty {
				fmt.Printf("\r\x1b[K  %-28s comparing with the original", m.name)
			}
			var ssim, psnr float64
			var res Probe
			if err == nil {
				res, err = probe(ctx, out)
			}
			if err == nil {
				ssim, psnr, err = quality(ctx, out, f.path)
			}
			if tty {
				fmt.Print("\r\x1b[K")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				fmt.Printf("  %-28s FAILED: %v\n", m.name, err)
				continue
			}
			fi, _ := os.Stat(out)
			note := verdict(ssim)
			if why := hdrProblem(src, &res); why != "" {
				note += "; " + why
			} else if src.isHDR() {
				note += "; HDR kept"
			}
			fmt.Printf("  %-28s %9s  %+4.0f%%  %6s s  SSIM %s  PSNR %s dB  %s\n", m.name, fmtSize(fi.Size()),
				100*float64(fi.Size()-f.size)/float64(f.size), fmtNum(secs, 1), fmtNum(ssim, 4), fmtNum(psnr, 1), note)
			t := &totals[i]
			t.n++
			t.orig += f.size
			t.size += fi.Size()
			t.ssim += ssim
			t.psnr += psnr
			t.secs += secs
		}
	}

	fmt.Printf("\nSummary (%d videos)\n", len(files))
	for i, m := range modes {
		t := totals[i]
		if t.n == 0 {
			continue
		}
		avg := t.ssim / float64(t.n)
		fmt.Printf("  %-28s %9s  %+4.0f%%  %6s s  mean SSIM %s  mean PSNR %s dB  %s\n", m.name, fmtSize(t.size),
			100*float64(t.size-t.orig)/float64(t.orig), fmtNum(t.secs, 1), fmtNum(avg, 4), fmtNum(t.psnr/float64(t.n), 1), verdict(avg))
	}
	fmt.Println("\nSSIM: similarity to the original, from 0 to 1 (1 = identical). PSNR: in dB, higher is better.")
	fmt.Println("The labels (\"slight loss\" etc.) are a rough guide; what counts in the end is watching the video.")
	return nil
}
