package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Probe struct {
	VideoCodec     string
	Width, Height  int
	PixFmt         string
	ColorTransfer  string
	ColorPrimaries string
	ColorSpace     string
	AudioCodecs    []string
	SideData       []string // side data types of the video stream (e.g. HDR metadata, Dolby Vision)
	AudioStreams   int
	Duration       float64 // seconds
	Tags           map[string]string
}

func probe(ctx context.Context, path string) (Probe, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", path)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return Probe{}, ctx.Err()
		}
		return Probe{}, fmt.Errorf("ffprobe: %v: %s", err, lastLine(errb.String()))
	}
	p, err := parseProbe(out)
	if err != nil {
		return p, err
	}
	if bitDepth(p.PixFmt) > 8 {
		// In 10-bit video the HDR metadata may live only inside the frames.
		p.SideData = append(p.SideData, frameSideData(ctx, path)...)
	}
	return p, nil
}

// frameSideData returns the side data types of the first video frame.
func frameSideData(ctx context.Context, path string) []string {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-read_intervals", "%+#1", "-show_frames", "-print_format", "json", path).Output()
	if err != nil {
		return nil
	}
	var raw struct {
		Frames []struct {
			SideData []struct {
				Type string `json:"side_data_type"`
			} `json:"side_data_list"`
		} `json:"frames"`
	}
	if json.Unmarshal(out, &raw) != nil {
		return nil
	}
	var types []string
	for _, f := range raw.Frames {
		for _, sd := range f.SideData {
			types = append(types, sd.Type)
		}
	}
	return types
}

func parseProbe(data []byte) (Probe, error) {
	var raw struct {
		Streams []struct {
			CodecType      string `json:"codec_type"`
			CodecName      string `json:"codec_name"`
			Width          int    `json:"width"`
			Height         int    `json:"height"`
			PixFmt         string `json:"pix_fmt"`
			ColorTransfer  string `json:"color_transfer"`
			ColorPrimaries string `json:"color_primaries"`
			ColorSpace     string `json:"color_space"`
			SideData       []struct {
				Type string `json:"side_data_type"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Probe{}, fmt.Errorf("ffprobe: invalid output: %v", err)
	}
	p := Probe{Tags: raw.Format.Tags}
	p.Duration, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if p.VideoCodec == "" {
				p.VideoCodec = s.CodecName
				p.Width, p.Height, p.PixFmt = s.Width, s.Height, s.PixFmt
				p.ColorTransfer, p.ColorPrimaries, p.ColorSpace = s.ColorTransfer, s.ColorPrimaries, s.ColorSpace
				for _, sd := range s.SideData {
					p.SideData = append(p.SideData, sd.Type)
				}
			}
		case "audio":
			p.AudioStreams++
			p.AudioCodecs = append(p.AudioCodecs, s.CodecName)
		}
	}
	return p, nil
}

var bitsRe = regexp.MustCompile(`(1\d)(le|be)?$`)

// bitDepth returns the bits per channel of a pixel format (yuv420p -> 8, yuv420p10le -> 10).
func bitDepth(pixFmt string) int {
	if strings.HasPrefix(pixFmt, "nv") { // nv12, nv21...: the number is not the bit depth
		return 8
	}
	if m := bitsRe.FindStringSubmatch(pixFmt); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 8
}

// isHDR reports whether the video uses an HDR transfer curve (PQ/HDR10 or HLG).
func (p Probe) isHDR() bool {
	return p.ColorTransfer == "smpte2084" || p.ColorTransfer == "arib-std-b67"
}

func (p Probe) hasSideData(sub string) bool {
	for _, t := range p.SideData {
		if strings.Contains(strings.ToLower(t), sub) {
			return true
		}
	}
	return false
}

// HDR metadata that must still be present in the converted file.
// "hdr dynamic metadata" is HDR10+ (common on Samsung cameras).
var hdrSideData = []string{"mastering display", "content light level", "hdr dynamic metadata"}

// hdrProblem returns why the conversion would lose (or lost) HDR, or "".
// With out == nil it only checks whether the original can be converted without losing HDR.
func hdrProblem(orig Probe, out *Probe) string {
	if out == nil {
		if orig.hasSideData("dovi") && !orig.isHDR() {
			return "Dolby Vision without a compatible HDR base layer: converting would break the colors"
		}
		return ""
	}
	if ob, nb := bitDepth(orig.PixFmt), bitDepth(out.PixFmt); ob > 8 && nb <= 8 {
		return fmt.Sprintf("bit depth lost (%d bits -> %d bits)", ob, nb)
	}
	if orig.isHDR() && (out.ColorTransfer != orig.ColorTransfer || out.ColorPrimaries != orig.ColorPrimaries) {
		return fmt.Sprintf("HDR lost (%s/%s -> %s/%s)", orig.ColorTransfer, orig.ColorPrimaries, out.ColorTransfer, out.ColorPrimaries)
	}
	for _, sd := range hdrSideData {
		if orig.hasSideData(sd) && !out.hasSideData(sd) {
			return "HDR metadata lost: " + sd
		}
	}
	return ""
}

// audioProblem returns why the original audio cannot be copied unchanged into a file
// with extension ext, or "". Audio is never re-encoded.
func audioProblem(src Probe, ext string) string {
	if ext != ".mp4" {
		return ""
	}
	for _, c := range src.AudioCodecs {
		if strings.HasPrefix(c, "amr_") { // audio of an old 3GP video stored with a .mp4 name
			return fmt.Sprintf("%s audio cannot be copied into .mp4 without re-encoding", c)
		}
	}
	return ""
}

// firstLine returns the first non-empty line: with ffmpeg, that is the one with the cause.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// Tags that describe the generated file itself and are expected to change.
var volatileTags = map[string]bool{
	"encoder": true, "major_brand": true, "minor_version": true, "compatible_brands": true,
}

// lostTags returns the tags of the original that are missing or changed in the converted file.
func lostTags(orig, out map[string]string) []string {
	got := map[string]string{}
	for k, v := range out {
		got[strings.ToLower(k)] = v
	}
	var lost []string
	for k, v := range orig {
		lk := strings.ToLower(k)
		if volatileTags[lk] {
			continue
		}
		if g, ok := got[lk]; !ok || g != v {
			lost = append(lost, k)
		}
	}
	sort.Strings(lost)
	return lost
}

// checkResult decides whether the converted file may replace the original.
func checkResult(orig, out Probe, origSize, newSize int64) (ok bool, reason string) {
	if out.VideoCodec != "hevc" {
		return false, fmt.Sprintf("unexpected codec in the result: %q", out.VideoCodec)
	}
	if math.Abs(orig.Duration-out.Duration) > 0.5 {
		return false, fmt.Sprintf("duration changed: %.2fs -> %.2fs", orig.Duration, out.Duration)
	}
	if out.AudioStreams != orig.AudioStreams {
		return false, "audio stream lost"
	}
	if why := hdrProblem(orig, &out); why != "" {
		return false, why
	}
	if lost := lostTags(orig.Tags, out.Tags); len(lost) > 0 {
		return false, "metadata lost: " + strings.Join(lost, ", ")
	}
	if float64(newSize) > float64(origSize)*0.9 {
		pct := 100 * float64(origSize-newSize) / float64(origSize)
		if pct < 0 {
			return false, fmt.Sprintf("got %.0f%% bigger (%s -> %s)", -pct, fmtSize(origSize), fmtSize(newSize))
		}
		return false, fmt.Sprintf("only %.0f%% smaller, below the 10%% minimum (%s -> %s)", pct, fmtSize(origSize), fmtSize(newSize))
	}
	return true, ""
}

// Encoder defines how the video is encoded: CPU (libx265, CRF) or Intel/AMD GPU (VAAPI, QP).
type Encoder struct {
	GPU    bool
	CRF    int    // CPU
	Preset string // CPU
	QP     int    // GPU
}

// The GPU cannot encode video narrower or shorter than this (the case of many GIFs).
const gpuMinSide = 128

// forSource adapts the encoder to the video: those too small for the GPU go through the
// CPU, which is fast on them anyway.
func (e Encoder) forSource(src Probe) Encoder {
	if e.GPU && src.Width > 0 && (src.Width < gpuMinSide || src.Height < gpuMinSide) {
		e.GPU = false
	}
	return e
}

const vaapiDevice = "/dev/dri/renderD128"

func (e Encoder) codec() string {
	if e.GPU {
		return "hevc_vaapi"
	}
	return "libx265"
}

// ffmpegArgs builds the conversion from in to out. src is the probe of the original:
// 10-bit (HDR) videos stay 10-bit and the color information is passed through.
func ffmpegArgs(in, out string, enc Encoder, src Probe) []string {
	return encodeArgs(in, out, enc, src, false)
}

// encodeArgs builds the ffmpeg arguments for the conversion. With raw set, only the video
// is encoded, frame for frame, into a bare HEVC stream (used by the HDR10+ path).
func encodeArgs(in, out string, enc Encoder, src Probe, raw bool) []string {
	enc = enc.forSource(src)
	deep := bitDepth(src.PixFmt) > 8
	args := []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error"}
	if enc.GPU {
		args = append(args, "-vaapi_device", vaapiDevice)
	}
	args = append(args, "-i", in, "-map", "0:v:0")
	if raw {
		args = append(args, "-fps_mode", "passthrough")
	} else {
		args = append(args, "-map", "0:a?")
	}
	switch {
	case enc.GPU && deep:
		args = append(args, "-vf", "format=p010,hwupload",
			"-c:v", "hevc_vaapi", "-profile:v", "main10", "-rc_mode", "CQP", "-qp", strconv.Itoa(enc.QP))
	case enc.GPU:
		args = append(args, "-vf", "format=nv12,hwupload",
			"-c:v", "hevc_vaapi", "-rc_mode", "CQP", "-qp", strconv.Itoa(enc.QP))
	default:
		args = append(args, "-c:v", "libx265", "-crf", strconv.Itoa(enc.CRF), "-preset", enc.Preset,
			"-x265-params", "log-level=error")
	}
	for _, c := range [][2]string{
		{"-color_primaries", src.ColorPrimaries}, {"-color_trc", src.ColorTransfer}, {"-colorspace", src.ColorSpace},
	} {
		if c[1] != "" && c[1] != "unknown" && c[1] != "reserved" {
			args = append(args, c[0], c[1])
		}
	}
	if raw {
		return append(args, "-progress", "pipe:1", "-nostats", "-f", "hevc", out)
	}
	args = append(args, "-c:a", "copy", "-map_metadata", "0")
	args = append(args, containerArgs(out)...)
	return append(args, "-progress", "pipe:1", "-nostats", out)
}

// containerArgs returns the muxer options for the output file type.
func containerArgs(out string) []string {
	switch strings.ToLower(filepath.Ext(out)) {
	case ".mp4", ".mov":
		// hvc1 so Apple devices play it; use_metadata_tags keeps non-standard tags.
		return []string{"-tag:v", "hvc1", "-movflags", "+faststart+use_metadata_tags"}
	}
	return nil
}

// convert re-encodes in to H.265 at out. onProgress receives the completed fraction
// (0 to 1) and the speed reported by ffmpeg (e.g. "2.3x").
func convert(ctx context.Context, in, out string, enc Encoder, src Probe, onProgress func(frac float64, speed string)) error {
	if src.hasHDR10Plus() && hdr10PlusProblem(src, hdr10PlusMissing()) == "" {
		return convertHDR10Plus(ctx, in, out, enc, src, onProgress)
	}
	return runFFmpeg(ctx, ffmpegArgs(in, out, enc, src), src.Duration, onProgress)
}

// runFFmpeg runs ffmpeg with args that include "-progress pipe:1" and reports progress
// against the given duration in seconds.
func runFFmpeg(ctx context.Context, args []string, duration float64, onProgress func(frac float64, speed string)) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var frac float64
	speed := ""
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "out_time_us":
			if us, err := strconv.ParseFloat(v, 64); err == nil && duration > 0 {
				frac = math.Max(0, math.Min(1, us/1e6/duration))
			}
		case "speed":
			speed = strings.TrimSpace(v)
		case "progress":
			if onProgress != nil {
				onProgress(frac, speed)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: %v: %s", err, firstLine(errb.String()))
	}
	return nil
}
