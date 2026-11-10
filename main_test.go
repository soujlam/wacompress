package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseListing(t *testing.T) {
	out := "1048576|1710000000|/sdcard/WhatsApp/Media/WhatsApp Video/VID-20240312-WA0007.mp4\r\n" +
		"2048|1710000001|/sdcard/WhatsApp/Media/WhatsApp Video/Sent/a|b.mp4\n" +
		"junk\n\nx|y|z\n"
	got := parseListing(out)
	want := []RemoteFile{
		{"/sdcard/WhatsApp/Media/WhatsApp Video/VID-20240312-WA0007.mp4", 1048576, 1710000000},
		{"/sdcard/WhatsApp/Media/WhatsApp Video/Sent/a|b.mp4", 2048, 1710000001},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseDevices(t *testing.T) {
	out := "* daemon started successfully\nList of devices attached\nR58M123\tdevice\nemulator-5554\tunauthorized\n\n"
	got := parseDevices(out)
	want := map[string]string{"R58M123": "device", "emulator-5554": "unauthorized"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestShQuote(t *testing.T) {
	if got := shQuote("a b'c"); got != `'a b'\''c'` {
		t.Fatalf("got %s", got)
	}
}

func TestParseProbe(t *testing.T) {
	js := `{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac"}],
		"format":{"duration":"12.500000","tags":{"creation_time":"2024-03-12T10:00:00.000000Z"}}}`
	p, err := parseProbe([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if p.VideoCodec != "h264" || p.AudioStreams != 1 || p.Duration != 12.5 || p.Tags["creation_time"] == "" {
		t.Fatalf("got %+v", p)
	}
}

func TestCheckResult(t *testing.T) {
	tags := map[string]string{"creation_time": "2024-03-12T10:00:00.000000Z", "encoder": "Lavf58", "major_brand": "mp42"}
	orig := Probe{VideoCodec: "h264", AudioStreams: 1, Duration: 10, Tags: tags}
	good := Probe{VideoCodec: "hevc", AudioStreams: 1, Duration: 10.1,
		Tags: map[string]string{"CREATION_TIME": "2024-03-12T10:00:00.000000Z", "encoder": "Lavf61", "major_brand": "isom"}}

	if ok, reason := checkResult(orig, good, 1000, 500); !ok {
		t.Fatalf("should be accepted: %s", reason)
	}
	cases := []struct {
		name    string
		out     Probe
		newSize int64
		want    string
	}{
		{"small reduction", good, 950, "only 5% smaller"},
		{"got bigger", good, 1200, "got 20% bigger"},
		{"codec", Probe{VideoCodec: "h264", AudioStreams: 1, Duration: 10, Tags: good.Tags}, 500, "codec"},
		{"duration", Probe{VideoCodec: "hevc", AudioStreams: 1, Duration: 8, Tags: good.Tags}, 500, "duration"},
		{"audio", Probe{VideoCodec: "hevc", Duration: 10, Tags: good.Tags}, 500, "audio"},
		{"date lost", Probe{VideoCodec: "hevc", AudioStreams: 1, Duration: 10}, 500, "creation_time"},
		{"date changed", Probe{VideoCodec: "hevc", AudioStreams: 1, Duration: 10,
			Tags: map[string]string{"creation_time": "2026-11-10T00:00:00.000000Z"}}, 500, "creation_time"},
	}
	for _, c := range cases {
		ok, reason := checkResult(orig, c.out, 1000, c.newSize)
		if ok || !strings.Contains(reason, c.want) {
			t.Errorf("%s: ok=%v reason=%q", c.name, ok, reason)
		}
	}
}

func TestFfmpegArgs(t *testing.T) {
	cpu := Encoder{CRF: 26, Preset: "medium"}
	mp4 := strings.Join(ffmpegArgs("in.mp4", "out.mp4", cpu, Probe{}), " ")
	if !strings.Contains(mp4, "use_metadata_tags") || !strings.Contains(mp4, "-map_metadata 0") ||
		!strings.Contains(mp4, "libx265 -crf 26") || !strings.HasSuffix(mp4, "out.mp4") {
		t.Errorf("mp4: %s", mp4)
	}
	mkv := strings.Join(ffmpegArgs("in.mkv", "out.mkv", cpu, Probe{}), " ")
	if strings.Contains(mkv, "movflags") || !strings.Contains(mkv, "-map_metadata 0") {
		t.Errorf("mkv: %s", mkv)
	}
	gpu := strings.Join(ffmpegArgs("in.mp4", "out.mp4", Encoder{GPU: true, QP: 33}, Probe{PixFmt: "yuv420p"}), " ")
	if !strings.Contains(gpu, "-vaapi_device "+vaapiDevice+" -i in.mp4") || !strings.Contains(gpu, "hevc_vaapi -rc_mode CQP -qp 33") ||
		strings.Contains(gpu, "libx265") || !strings.Contains(gpu, "-map_metadata 0") {
		t.Errorf("gpu: %s", gpu)
	}
}

func TestStateDoneAndRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registro.json")
	s, err := loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Entries["/a.mp4"] = Entry{Status: statusConverted, OrigSize: 100, NewSize: 40, When: time.Now()}
	s.Entries["/b.mp4"] = Entry{Status: statusSkipped, OrigSize: 100}
	s.Entries["/c.mp4"] = Entry{Status: statusFailed, OrigSize: 100}
	s.Entries["/hdr.mp4"] = Entry{Status: statusSkipped, OrigSize: 100, Reason: "HDR metadata lost: hdr dynamic metadata"}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s, err = loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		f    RemoteFile
		want bool
	}{
		{RemoteFile{"/a.mp4", 40, 0}, true},
		{RemoteFile{"/a.mp4", 100, 0}, false}, // the file is a different one again
		{RemoteFile{"/b.mp4", 100, 0}, true},
		{RemoteFile{"/c.mp4", 100, 0}, false},   // a failure is retried
		{RemoteFile{"/hdr.mp4", 100, 0}, false}, // skipped for HDR10+ is retried
		{RemoteFile{"/novo.mp4", 1, 0}, false},
	}
	for _, c := range cases {
		if got := s.done(c.f); got != c.want {
			t.Errorf("%+v: got %v", c.f, got)
		}
	}
}

func TestSizeAndDuration(t *testing.T) {
	for in, want := range map[string]int64{"1MB": 1 << 20, "500k": 500 << 10, "2G": 2 << 30, "123": 123, "1.5M": 3 << 19} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := parseSize("abc"); err == nil {
		t.Error("parseSize(abc) should fail")
	}
	if got := fmtSize(50437529); got != "48.1 MB" {
		t.Errorf("fmtSize = %q", got)
	}
	if got := fmtDur(41 * time.Second); got != "0:41" {
		t.Errorf("fmtDur = %q", got)
	}
	if got := fmtDur(130 * time.Minute); got != "2h10" {
		t.Errorf("fmtDur = %q", got)
	}
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName("Moto G (5) Plus-192.168.0.5:5555"); got != "Moto_G__5__Plus-192.168.0.5_5555" {
		t.Errorf("got %q", got)
	}
}

func TestMigrateLegacy(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "SM-X-123")

	if moved, err := migrateLegacy(root, dev); moved || err != nil {
		t.Fatalf("no old backup: moved=%v err=%v", moved, err)
	}

	os.WriteFile(filepath.Join(root, "registro.json"), []byte("{}"), 0o644)
	os.MkdirAll(filepath.Join(root, "originais", "sdcard"), 0o755)
	os.WriteFile(filepath.Join(root, "originais", "sdcard", "a.mp4"), []byte("x"), 0o644)

	// Old conversion still running: touch nothing.
	os.Mkdir(filepath.Join(root, ".work"), 0o755)
	if moved, err := migrateLegacy(root, dev); moved || err == nil {
		t.Fatalf("with .work: moved=%v err=%v", moved, err)
	}
	if !exists(filepath.Join(root, "registro.json")) {
		t.Fatal("registry was moved while a conversion was running")
	}
	os.Remove(filepath.Join(root, ".work"))

	if moved, err := migrateLegacy(root, dev); !moved || err != nil {
		t.Fatalf("moved=%v err=%v", moved, err)
	}
	if !exists(filepath.Join(dev, "registro.json")) || !exists(filepath.Join(dev, "originais", "sdcard", "a.mp4")) ||
		exists(filepath.Join(root, "registro.json")) || exists(filepath.Join(root, "originais")) {
		t.Fatal("files were not moved into the device folder")
	}

	// Old backup shows up again with the device folder already in use: refuse.
	os.WriteFile(filepath.Join(root, "registro.json"), []byte("{}"), 0o644)
	if moved, err := migrateLegacy(root, dev); moved || err == nil {
		t.Fatalf("conflict: moved=%v err=%v", moved, err)
	}
}

func TestParallelFor(t *testing.T) {
	// Uses the limits from the code, so it keeps holding when they are tuned.
	for size, want := range map[int64]int{
		1: 4, smallBelow - 1: 4, smallBelow: 2, mediumBelow - 1: 2, mediumBelow: 1, 100 * mediumBelow: 1,
	} {
		if got := parallelFor(size); got != want {
			t.Errorf("parallelFor(%d) = %d, want %d", size, got, want)
		}
		if maxParallel%parallelFor(size) != 0 {
			t.Errorf("slots do not divide evenly for %d", size)
		}
	}
}

func TestHDR(t *testing.T) {
	for f, want := range map[string]int{"yuv420p": 8, "nv12": 8, "yuv420p10le": 10, "p010le": 10, "yuv422p12le": 12, "": 8} {
		if got := bitDepth(f); got != want {
			t.Errorf("bitDepth(%q) = %d", f, got)
		}
	}
	hdr := Probe{VideoCodec: "hevc", PixFmt: "yuv420p10le", ColorTransfer: "smpte2084", ColorPrimaries: "bt2020",
		ColorSpace: "bt2020nc", SideData: []string{"Mastering display metadata", "Content light level metadata"}}

	gpu := strings.Join(ffmpegArgs("in.mp4", "out.mp4", Encoder{GPU: true, QP: 33}, hdr), " ")
	for _, want := range []string{"format=p010,hwupload", "-profile:v main10", "-color_trc smpte2084", "-color_primaries bt2020", "-colorspace bt2020nc"} {
		if !strings.Contains(gpu, want) {
			t.Errorf("gpu HDR without %q: %s", want, gpu)
		}
	}
	if sdr := strings.Join(ffmpegArgs("in.mp4", "out.mp4", Encoder{GPU: true, QP: 33}, Probe{PixFmt: "yuv420p"}), " "); strings.Contains(sdr, "main10") || strings.Contains(sdr, "color_trc") {
		t.Errorf("sdr: %s", sdr)
	}

	same := hdr
	if why := hdrProblem(hdr, &same); why != "" {
		t.Errorf("kept HDR should pass: %s", why)
	}
	eight := hdr
	eight.PixFmt = "yuv420p"
	sdrOut := hdr
	sdrOut.ColorTransfer = "bt709"
	noMeta := hdr
	noMeta.SideData = nil
	for name, c := range map[string]struct {
		out  Probe
		want string
	}{"8 bits": {eight, "10 bits -> 8 bits"}, "curve": {sdrOut, "HDR lost"}, "metadata": {noMeta, "HDR metadata lost"}} {
		if why := hdrProblem(hdr, &c.out); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q", name, why)
		}
	}
	dv := Probe{PixFmt: "yuv420p10le", SideData: []string{"DOVI configuration record"}}
	if hdrProblem(dv, nil) == "" {
		t.Error("Dolby Vision without a compatible base layer should be refused")
	}
	dv.ColorTransfer = "arib-std-b67"
	if why := hdrProblem(dv, nil); why != "" {
		t.Errorf("Dolby Vision with HLG should pass: %s", why)
	}
}

func TestParseQuality(t *testing.T) {
	log := "[Parsed_ssim_4 @ 0x1] SSIM Y:0.93 (11.6) U:0.97 (15.1) V:0.97 (15.5) All:0.940634 (12.264628)\n" +
		"[Parsed_psnr_5 @ 0x2] PSNR y:36.1 u:42.0 v:42.5 average:37.412345 min:30.1 max:45.2\n"
	ssim, psnr, err := parseQuality(log)
	if err != nil || ssim != 0.940634 || psnr != 37.412345 {
		t.Fatalf("ssim=%v psnr=%v err=%v", ssim, psnr, err)
	}
	if _, _, err := parseQuality("nothing"); err == nil {
		t.Error("should fail without the SSIM/PSNR lines")
	}
}

func TestPickSamples(t *testing.T) {
	files := []sample{{"a", 1}, {"b", 2}, {"c", 3}, {"d", 4}, {"e", 5}, {"f", 6}, {"g", 7}, {"h", 8}, {"i", 9}}
	got := pickSamples(files, 3)
	if len(got) != 3 || got[0].path != "i" || got[1].path != "e" || got[2].path != "a" {
		t.Errorf("got %+v", got)
	}
	if got := pickSamples(files[:2], 5); len(got) != 2 {
		t.Errorf("got %+v", got)
	}
}

func TestFreedBytes(t *testing.T) {
	st := &State{Entries: map[string]Entry{
		"/a": {Status: statusConverted, OrigSize: 100, NewSize: 40},
		"/b": {Status: statusConverted, OrigSize: 50, NewSize: 20},
		"/c": {Status: statusSkipped, OrigSize: 100},
		"/d": {Status: statusFailed, OrigSize: 100},
	}}
	if n, freed := freedBytes(st); n != 2 || freed != 90 {
		t.Errorf("n=%d freed=%d", n, freed)
	}
}

func TestOtherFolders(t *testing.T) {
	files := []RemoteFile{
		{"/sdcard/DCIM/Camera/a.mp4", 100, 0}, {"/sdcard/DCIM/Camera/b.mp4", 50, 0},
		{"/sdcard/Movies/c.mp4", 500, 0},
		{"/sdcard/WhatsApp/Media/WhatsApp Video/Sent/d.mp4", 900, 0},
		{"/sdcard/WhatsApp/Media/WhatsApp Videos Antigos/e.mp4", 10, 0},
		{"/sdcard/Download/x/f.mp4", 70, 0},
		{"/sdcard/Android/data/com.jogo/files/g.mp4", 800, 0},
		{"/sdcard/Android/media/com.whatsapp/WhatsApp/Media/.Statuses/h.mp4", 800, 0},
		{"/sdcard/DCIM/.thumbnails/i.mp4", 800, 0},
	}
	got := otherFolders(files, []string{"/sdcard/WhatsApp/Media/WhatsApp Video", "/sdcard/Download/"})
	want := []folderInfo{{"/sdcard/Movies", 1, 500}, {"/sdcard/DCIM/Camera", 2, 150}, {"/sdcard/WhatsApp/Media/WhatsApp Videos Antigos", 1, 10}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestParseSelection(t *testing.T) {
	for in, want := range map[string][]int{
		"": nil, "\n": nil, "n": nil, "all": {0, 1, 2, 3, 4}, "1": {0}, "1, 3": {0, 2}, "2-4 1 2": {1, 2, 3, 0},
	} {
		got, err := parseSelection(in, 5)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseSelection(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"0", "6", "abc", "3-2", "1-9"} {
		if _, err := parseSelection(in, 5); err == nil {
			t.Errorf("parseSelection(%q) should fail", in)
		}
	}
}

func TestDeviceConfigRoundTrip(t *testing.T) {
	p := configPath(t.TempDir())
	c, err := loadDeviceConfig(p)
	if err != nil || len(c.Folders) != 0 {
		t.Fatalf("no file: %+v %v", c, err)
	}
	c.Folders = []string{"/sdcard/DCIM/Camera"}
	if err := c.save(p); err != nil {
		t.Fatal(err)
	}
	if c2, err := loadDeviceConfig(p); err != nil || !reflect.DeepEqual(c2, c) {
		t.Fatalf("got %+v %v", c2, err)
	}
}

func TestGPUFallbackForTinyVideo(t *testing.T) {
	gpu := Encoder{GPU: true, QP: 33, CRF: 26, Preset: "medium"}
	tiny := strings.Join(ffmpegArgs("in.mp4", "out.mp4", gpu, Probe{Width: 200, Height: 112, PixFmt: "yuv420p"}), " ")
	if strings.Contains(tiny, "vaapi") || !strings.Contains(tiny, "libx265 -crf 26") {
		t.Errorf("a 200x112 video should go through the CPU: %s", tiny)
	}
	ok := strings.Join(ffmpegArgs("in.mp4", "out.mp4", gpu, Probe{Width: 128, Height: 128, PixFmt: "yuv420p"}), " ")
	if !strings.Contains(ok, "hevc_vaapi") {
		t.Errorf("a 128x128 video should go through the GPU: %s", ok)
	}
}

func TestAudioProblem(t *testing.T) {
	amr := Probe{AudioCodecs: []string{"amr_nb"}}
	if audioProblem(amr, ".mp4") == "" {
		t.Error("amr_nb in .mp4 should be refused")
	}
	if why := audioProblem(amr, ".mkv"); why != "" {
		t.Errorf("amr_nb in .mkv: %s", why)
	}
	if why := audioProblem(Probe{AudioCodecs: []string{"aac"}}, ".mp4"); why != "" {
		t.Errorf("aac in .mp4: %s", why)
	}
	if got := firstLine("\n  real cause\nrest\n"); got != "real cause" {
		t.Errorf("firstLine = %q", got)
	}
}

func TestBackupConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cfg", "config.json")

	if got, configured := resolveBackup("", appConfig{}, "/home/x"); got != "/home/x/wacompress-backup" || configured {
		t.Errorf("default: %q %v", got, configured)
	}
	want := filepath.Join(dir, "disco", "backup")
	abs, err := setBackup(cfgPath, want)
	if err != nil || abs != want || !exists(want) {
		t.Fatalf("setBackup: %q %v", abs, err)
	}
	cfg, err := loadAppConfig(cfgPath)
	if err != nil || cfg.Backup != want {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if got, configured := resolveBackup("", cfg, "/home/x"); got != want || !configured {
		t.Errorf("configured: %q %v", got, configured)
	}
	if got, configured := resolveBackup("/outra", cfg, "/home/x"); got != "/outra" || configured {
		t.Errorf("the -backup option must win: %q %v", got, configured)
	}
	if err := checkBackupDir(want); err != nil {
		t.Errorf("existing folder: %v", err)
	}
	os.RemoveAll(filepath.Join(dir, "disco"))
	if err := checkBackupDir(want); err == nil {
		t.Error("a configured folder that vanished (disk unplugged) should be refused")
	}
}

func TestHDR10Plus(t *testing.T) {
	p := Probe{VideoCodec: "hevc", SideData: []string{"HDR Dynamic Metadata SMPTE2094-40 (HDR10+)"}}
	if !p.hasHDR10Plus() {
		t.Fatal("HDR10+ side data not detected")
	}
	if why := hdr10PlusProblem(p, ""); why != "" {
		t.Errorf("hevc with tools: %s", why)
	}
	if why := hdr10PlusProblem(p, "mkvmerge"); !strings.Contains(why, "mkvmerge") {
		t.Errorf("missing tool: %q", why)
	}
	p.VideoCodec = "av1"
	if hdr10PlusProblem(p, "") == "" {
		t.Error("HDR10+ in av1 should be refused")
	}

	raw := strings.Join(encodeArgs("in.mp4", "v.hevc", Encoder{GPU: true, QP: 33}, Probe{PixFmt: "yuv420p10le", ColorTransfer: "smpte2084"}, true), " ")
	for _, want := range []string{"-fps_mode passthrough", "-f hevc v.hevc", "main10", "-color_trc smpte2084"} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw args without %q: %s", want, raw)
		}
	}
	for _, bad := range []string{"0:a?", "-c:a", "movflags"} {
		if strings.Contains(raw, bad) {
			t.Errorf("raw args with %q: %s", bad, raw)
		}
	}
}

func TestTimestamps(t *testing.T) {
	js := `{"packets":[{"pts":3000},{"pts":0},{"pts":1500},{"pts":7500}],"streams":[{"time_base":"1/90000"}]}`
	ms, err := parseTimestamps([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0, 1500.0 / 90, 3000.0 / 90, 7500.0 / 90}
	for i := range want {
		if d := ms[i] - want[i]; d > 1e-9 || d < -1e-9 {
			t.Fatalf("got %v want %v", ms, want)
		}
	}
	if got := timestampFile(ms[:2]); got != "# timestamp format v2\n0.0000\n16.6667\n" {
		t.Errorf("timestampFile = %q", got)
	}
	if _, err := parseTimestamps([]byte(`{"packets":[{}],"streams":[{"time_base":"1/90000"}]}`)); err == nil {
		t.Error("a frame without pts should fail")
	}
}
