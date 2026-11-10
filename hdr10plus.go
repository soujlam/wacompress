package main

// HDR10+ keeps per-scene brightness data inside the HEVC bitstream. Neither encoder used
// here writes it, so HDR10+ sources take the long way:
//  1. extract the HDR10+ metadata from the original with hdr10plus_tool;
//  2. encode the video alone into a raw HEVC stream, frame for frame;
//  3. inject the metadata into the new stream with hdr10plus_tool;
//  4. wrap it in Matroska with mkvmerge, giving each frame its original timestamp
//     (raw HEVC has no timestamps, and phone videos often have a variable frame rate);
//  5. remux into the final file with the original audio and metadata.
// The result then goes through the same checks as any other conversion, including
// that the HDR10+ metadata is present.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const hdr10PlusTool = "hdr10plus_tool"

// hasHDR10Plus reports whether the video carries HDR10+ dynamic metadata.
func (p Probe) hasHDR10Plus() bool { return p.hasSideData("hdr dynamic metadata") }

// hdr10PlusMissing lists the tools needed for HDR10+ that are not installed, or "".
func hdr10PlusMissing() string {
	var missing []string
	for _, t := range []string{hdr10PlusTool, "mkvmerge"} {
		if _, err := exec.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	return strings.Join(missing, " and ")
}

// hdr10PlusProblem returns why an HDR10+ video cannot be converted keeping HDR10+, or "".
func hdr10PlusProblem(src Probe, missing string) string {
	if missing != "" {
		return "keeping HDR10+ needs " + missing + " installed (see README)"
	}
	if src.VideoCodec != "hevc" {
		return "HDR10+ in " + src.VideoCodec + " video is not supported"
	}
	return ""
}

// runTool runs an external program and returns its last output line on failure.
func runTool(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	if name == "mkvmerge" && errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil // mkvmerge exits with 1 for warnings; the file is still written
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %v: %s", name, err, lastLine(out.String()))
	}
	return nil
}

// parseTimestamps reads ffprobe JSON with the stream time base and packet pts values and
// returns the frame times in milliseconds from the first frame, in display order.
func parseTimestamps(data []byte) ([]float64, error) {
	var raw struct {
		Streams []struct {
			TimeBase string `json:"time_base"`
		} `json:"streams"`
		Packets []struct {
			PTS *int64 `json:"pts"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("ffprobe: invalid output: %v", err)
	}
	if len(raw.Streams) == 0 || len(raw.Packets) == 0 {
		return nil, errors.New("no video frames found")
	}
	tb, ok := new(big.Rat).SetString(raw.Streams[0].TimeBase)
	if !ok || tb.Sign() <= 0 {
		return nil, fmt.Errorf("invalid time base %q", raw.Streams[0].TimeBase)
	}
	msPerTick, _ := new(big.Rat).Mul(tb, big.NewRat(1000, 1)).Float64()
	pts := make([]int64, 0, len(raw.Packets))
	for _, p := range raw.Packets {
		if p.PTS == nil {
			return nil, errors.New("a video frame has no timestamp")
		}
		pts = append(pts, *p.PTS)
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })
	ms := make([]float64, len(pts))
	for i, p := range pts {
		ms[i] = float64(p-pts[0]) * msPerTick
	}
	return ms, nil
}

func frameTimestamps(ctx context.Context, path string) ([]float64, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=time_base:packet=pts", "-print_format", "json", path).Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ffprobe (timestamps): %v", err)
	}
	return parseTimestamps(out)
}

// timestampFile formats frame times (ms) as an mkvmerge "timestamp format v2" file.
func timestampFile(ms []float64) string {
	var b strings.Builder
	b.WriteString("# timestamp format v2\n")
	for _, v := range ms {
		b.WriteString(strconv.FormatFloat(v, 'f', 4, 64))
		b.WriteByte('\n')
	}
	return b.String()
}

// countFrames returns the number of video packets in path.
func countFrames(ctx context.Context, path string) (int, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-count_packets",
		"-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe (frame count): %v", err)
	}
	return strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(out)), ",")))
}

// convertHDR10Plus converts an HDR10+ video keeping its dynamic metadata (see top of file).
func convertHDR10Plus(ctx context.Context, in, out string, enc Encoder, src Probe, onProgress func(frac float64, speed string)) error {
	dir, err := os.MkdirTemp(filepath.Dir(out), "hdr10plus-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	p := func(name string) string { return filepath.Join(dir, name) }
	ff := []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error"}

	// 1. Metadata from the original bitstream.
	if err := runTool(ctx, "ffmpeg", append(ff, "-i", in, "-map", "0:v:0", "-c:v", "copy",
		"-bsf:v", "hevc_mp4toannexb", "-f", "hevc", p("src.hevc"))...); err != nil {
		return err
	}
	if err := runTool(ctx, hdr10PlusTool, "extract", p("src.hevc"), "-o", p("meta.json")); err != nil {
		return err
	}
	os.Remove(p("src.hevc"))

	// 2. Encode the video alone.
	if err := runFFmpeg(ctx, encodeArgs(in, p("video.hevc"), enc, src, true), src.Duration, onProgress); err != nil {
		return err
	}

	// 3. Put the metadata back.
	if err := runTool(ctx, hdr10PlusTool, "inject", "-i", p("video.hevc"), "-j", p("meta.json"), "-o", p("inj.hevc")); err != nil {
		return err
	}
	os.Remove(p("video.hevc"))

	// 4. Original frame timestamps.
	ts, err := frameTimestamps(ctx, in)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p("ts.txt"), []byte(timestampFile(ts)), 0o644); err != nil {
		return err
	}
	if err := runTool(ctx, "mkvmerge", "-q", "-o", p("video.mkv"), "--timestamps", "0:"+p("ts.txt"), p("inj.hevc")); err != nil {
		return err
	}
	os.Remove(p("inj.hevc"))
	if n, err := countFrames(ctx, p("video.mkv")); err != nil {
		return err
	} else if n != len(ts) {
		return fmt.Errorf("HDR10+: frame count changed (%d -> %d)", len(ts), n)
	}

	// 5. Final file with the original audio and metadata.
	args := append(ff, "-i", p("video.mkv"), "-i", in, "-map", "0:v:0", "-map", "1:a?", "-c", "copy", "-map_metadata", "1")
	args = append(args, containerArgs(out)...)
	return runTool(ctx, "ffmpeg", append(args, out)...)
}
