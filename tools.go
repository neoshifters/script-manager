package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const settingsFileName = "设置.json"

// Settings 是用户手动指定的外部工具路径与剪映草稿导出配置，保存到数据目录。
type Settings struct {
	FFmpeg  string `json:"ffmpeg,omitempty"`
	FFprobe string `json:"ffprobe,omitempty"`
	// DraftRoot 是 NAS 上的剪映草稿根目录（容器内路径）
	DraftRoot string `json:"draftRoot,omitempty"`
	// NasPrefix / WinPrefix 是「NAS 路径 → Windows 路径」映射，用于草稿内引用素材
	NasPrefix string `json:"nasPrefix,omitempty"`
	WinPrefix string `json:"winPrefix,omitempty"`
}

// ToolInfo 描述一次 ffmpeg / ffprobe 的查找结果。
type ToolInfo struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

type toolCandidate struct {
	path   string
	source string
}

func readSettings(path string) Settings {
	var s Settings
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, &s)
	return Settings{
		FFmpeg:    strings.TrimSpace(s.FFmpeg),
		FFprobe:   strings.TrimSpace(s.FFprobe),
		DraftRoot: strings.TrimSpace(s.DraftRoot),
		NasPrefix: strings.TrimRight(strings.TrimSpace(s.NasPrefix), "/"),
		WinPrefix: strings.TrimRight(strings.TrimSpace(s.WinPrefix), `\`),
	}
}

func (a *App) saveSettings(s Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.settingsPath, b)
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// locateTool 按「手动指定 → 环境变量 → 程序目录 → 数据目录 → PATH → 常见安装位置」顺序查找。
func (a *App) locateTool(name, explicit, envKey string) ToolInfo {
	cands := a.toolCandidates(name, explicit, envKey)
	var firstExisting *ToolInfo
	for _, c := range cands {
		info, statErr := os.Stat(c.path)
		if statErr != nil || info.IsDir() {
			continue
		}
		if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
			continue
		}
		checked := checkTool(name, c.path, c.source)
		if checked.OK {
			return checked
		}
		if firstExisting == nil {
			cp := checked
			firstExisting = &cp
		}
	}
	if firstExisting != nil {
		return *firstExisting
	}
	hint := fmt.Sprintf("未找到 %s。可以：① 在右上角「ffmpeg」设置里指定路径；② 放到程序目录或数据目录的 bin 下；③ 加入系统 PATH；④ 设置环境变量 %s。", name, envKey)
	if runtime.GOOS == "linux" {
		hint += " Docker 部署可在镜像或容器内安装 ffmpeg。"
	}
	return ToolInfo{Name: name, Hint: hint}
}

func (a *App) toolCandidates(name, explicit, envKey string) []toolCandidate {
	var out []toolCandidate
	seen := map[string]bool{}
	add := func(p, source string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		key := strings.ToLower(filepath.Clean(p))
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, toolCandidate{path: p, source: source})
	}

	add(explicit, "手动指定")
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		add(v, "环境变量 "+envKey)
	}

	dirs := []string{}
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		dirs = append(dirs, base, filepath.Join(base, "bin"))
	}
	dirs = append(dirs, filepath.Join(a.dataDir, "bin"), a.dataDir)
	names := []string{exeName(name), name}
	for _, dir := range dirs {
		for _, n := range names {
			src := "程序目录"
			if strings.HasPrefix(dir, a.dataDir) {
				src = "数据目录"
			}
			add(filepath.Join(dir, n), src)
		}
	}

	if p, err := exec.LookPath(name); err == nil {
		add(p, "系统 PATH")
	}
	if runtime.GOOS == "windows" {
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			add(filepath.Join(la, "Microsoft", "WinGet", "Links", name+".exe"), "WinGet")
		}
		add(`C:\ffmpeg\bin\`+name+".exe", "常见安装目录")
	} else {
		add("/usr/bin/"+name, "系统目录")
		add("/usr/local/bin/"+name, "系统目录")
	}
	return out
}

func checkTool(name, path, source string) ToolInfo {
	info := ToolInfo{Name: name, Path: path, Source: source}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		info.Hint = "无法执行该文件: " + err.Error()
		return info
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		info.Hint = "该文件不是可用的 " + name
		return info
	}
	info.OK = true
	info.Version = line
	return info
}

// envTools 返回 ffmpeg / ffprobe 的查找结果（带 10 秒缓存）。
func (a *App) envTools(force bool) (ToolInfo, ToolInfo) {
	a.envMu.Lock()
	defer a.envMu.Unlock()
	if !force && a.envCached && time.Since(a.envAt) < 10*time.Second {
		return a.envFFmpeg, a.envFFprobe
	}
	ff := a.locateTool("ffmpeg", a.settings.FFmpeg, "SM_FFMPEG")
	fp := a.locateTool("ffprobe", a.settings.FFprobe, "SM_FFPROBE")
	if !fp.OK && ff.Path != "" {
		sibling := filepath.Join(filepath.Dir(ff.Path), exeName("ffprobe"))
		if st, err := os.Stat(sibling); err == nil && !st.IsDir() {
			if checked := checkTool("ffprobe", sibling, "ffmpeg 同目录"); checked.OK {
				fp = checked
			} else if fp.Path == "" {
				fp = checked
			}
		}
	}
	a.envFFmpeg, a.envFFprobe = ff, fp
	a.envCached = true
	a.envAt = time.Now()
	return ff, fp
}

func (a *App) toolPath(name string) (string, error) {
	ff, fp := a.envTools(false)
	info := ff
	if name == "ffprobe" {
		info = fp
	}
	if info.OK {
		return info.Path, nil
	}
	if info.Hint != "" {
		return "", errors.New(info.Hint)
	}
	return "", fmt.Errorf("未找到 %s", name)
}

func (a *App) envPayload() map[string]any {
	ff, fp := a.envTools(true)
	a.envMu.Lock()
	settings := a.settings
	a.envMu.Unlock()
	root, nas, win := a.jianyingConfig()
	return map[string]any{
		"ffmpeg":   ff,
		"ffprobe":  fp,
		"ready":    ff.OK && fp.OK,
		"settings": settings,
		"draft":    map[string]string{"root": root, "nas": nas, "win": win},
		"platform": runtime.GOOS,
	}
}

func (a *App) handleEnv(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.envPayload())
}

func (a *App) handleSetEnv(w http.ResponseWriter, r *http.Request) {
	var req Settings
	if !decodeBody(w, r, &req) {
		return
	}
	req.FFmpeg = strings.TrimSpace(req.FFmpeg)
	req.FFprobe = strings.TrimSpace(req.FFprobe)
	req.DraftRoot = strings.TrimSpace(req.DraftRoot)
	req.NasPrefix = strings.TrimRight(strings.TrimSpace(req.NasPrefix), "/")
	req.WinPrefix = strings.TrimRight(strings.TrimSpace(req.WinPrefix), `\`)
	for label, p := range map[string]string{"ffmpeg": req.FFmpeg, "ffprobe": req.FFprobe} {
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			fail(w, http.StatusBadRequest, fmt.Sprintf("%s 路径不存在或不可用：%s", label, p))
			return
		}
	}
	a.envMu.Lock()
	a.settings = req
	a.envMu.Unlock()
	if err := a.saveSettings(req); err != nil {
		fail(w, http.StatusInternalServerError, "保存设置失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.envPayload())
}

// ---------- ffprobe ----------

type ProbeResult struct {
	FPS      float64 `json:"fps"`
	Duration float64 `json:"duration"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
}

func parseRate(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0/0" {
		return 0
	}
	if i := strings.IndexByte(s, '/'); i > 0 {
		num, err1 := strconv.ParseFloat(s[:i], 64)
		den, err2 := strconv.ParseFloat(s[i+1:], 64)
		if err1 == nil && err2 == nil && den != 0 {
			return num / den
		}
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func (a *App) probeFile(parent context.Context, path string) (ProbeResult, error) {
	fp, err := a.toolPath("ffprobe")
	if err != nil {
		return ProbeResult{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, fp,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=avg_frame_rate,r_frame_rate,width,height,duration:format=duration",
		"-of", "json",
		path,
	).Output()
	if err != nil {
		return ProbeResult{}, fmt.Errorf("读取视频信息失败（文件可能损坏或格式不支持）")
	}
	var parsed struct {
		Streams []struct {
			AvgFrameRate string `json:"avg_frame_rate"`
			RFrameRate   string `json:"r_frame_rate"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			Duration     string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return ProbeResult{}, errors.New("无法解析视频信息")
	}
	if len(parsed.Streams) == 0 {
		return ProbeResult{}, errors.New("文件里没有视频轨道")
	}
	st := parsed.Streams[0]
	fps := parseRate(st.AvgFrameRate)
	if fps <= 0 {
		fps = parseRate(st.RFrameRate)
	}
	res := ProbeResult{
		FPS:    math.Round(fps*1000) / 1000,
		Width:  st.Width,
		Height: st.Height,
	}
	if d, err := strconv.ParseFloat(parsed.Format.Duration, 64); err == nil {
		res.Duration = d
	} else if d, err := strconv.ParseFloat(st.Duration, 64); err == nil {
		res.Duration = d
	}
	res.Duration = math.Round(res.Duration*1000) / 1000
	return res, nil
}

func (a *App) resolveSource(src, product, script, file string) (string, error) {
	name, err := sanitizeName(file)
	if err != nil {
		return "", errors.New("文件名无效")
	}
	if !videoExts[strings.ToLower(filepath.Ext(name))] {
		return "", errors.New("不是支持的视频文件")
	}
	var path string
	if src == "script" {
		dir, err := a.scriptDir(product, script, true)
		if err != nil {
			return "", err
		}
		path = filepath.Join(dir, name)
	} else {
		path = filepath.Join(a.dataDir, tempDirName, name)
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", errors.New("文件不存在，可能已被清理")
	}
	return path, nil
}

func (a *App) handleProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Src     string `json:"src"`
		File    string `json:"file"`
		Product string `json:"product"`
		Script  string `json:"script"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	path, err := a.resolveSource(req.Src, req.Product, req.Script, req.File)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	res, err := a.probeFile(r.Context(), path)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 智能裁切（画面变化检测） ----------

type Segment struct {
	Index int     `json:"index"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type SplitJob struct {
	mu        sync.Mutex
	ID        string
	State     string
	Progress  float64
	Message   string
	Segments  []Segment
	Error     string
	CreatedAt time.Time
}

func (j *SplitJob) update(fn func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn()
}

func (j *SplitJob) view() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := map[string]any{
		"id":       j.ID,
		"state":    j.State,
		"progress": j.Progress,
		"message":  j.Message,
	}
	if j.Segments != nil {
		v["segments"] = j.Segments
	}
	if j.Error != "" {
		v["error"] = j.Error
	}
	return v
}

func (a *App) handleSplit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Src       string  `json:"src"`
		File      string  `json:"file"`
		Product   string  `json:"product"`
		Script    string  `json:"script"`
		Threshold float64 `json:"threshold"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if _, err := a.toolPath("ffmpeg"); err != nil {
		fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	path, err := a.resolveSource(req.Src, req.Product, req.Script, req.File)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	threshold := req.Threshold
	if threshold <= 0.05 || threshold > 0.95 {
		threshold = 0.3
	}
	job := &SplitJob{ID: newID(), State: "running", Message: "准备分析画面…", CreatedAt: time.Now()}
	a.jobsMu.Lock()
	a.purgeJobsLocked()
	a.jobs[job.ID] = job
	a.jobsMu.Unlock()
	go a.runSplit(job, path, threshold)
	writeJSON(w, http.StatusOK, map[string]string{"id": job.ID})
}

func (a *App) handleSplitStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.jobsMu.Lock()
	job := a.jobs[id]
	a.jobsMu.Unlock()
	if job == nil {
		fail(w, http.StatusNotFound, "任务不存在或已过期")
		return
	}
	writeJSON(w, http.StatusOK, job.view())
}

func (a *App) purgeJobsLocked() {
	for id, job := range a.jobs {
		if time.Since(job.CreatedAt) > 2*time.Hour {
			delete(a.jobs, id)
		}
	}
}

func (a *App) runSplit(job *SplitJob, path string, threshold float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	job.update(func() { job.Message = "读取视频信息…"; job.Progress = 0.02 })
	probe, err := a.probeFile(ctx, path)
	if err != nil {
		job.update(func() { job.State = "error"; job.Error = err.Error() })
		return
	}
	if probe.Duration <= 0.1 {
		job.update(func() { job.State = "error"; job.Error = "视频时长无效，无法分析" })
		return
	}

	job.update(func() { job.Message = "正在分析画面变化…"; job.Progress = 0.05 })
	cuts, err := a.detectCuts(ctx, path, threshold, probe.Duration, func(ratio float64) {
		job.update(func() {
			if ratio > 1 {
				ratio = 1
			}
			job.Progress = 0.05 + ratio*0.9
			job.Message = fmt.Sprintf("正在分析画面变化… %d%%", int(ratio*100))
		})
	})
	if err != nil {
		job.update(func() { job.State = "error"; job.Error = err.Error() })
		return
	}
	segs := buildSegments(cuts, probe.Duration)
	job.update(func() {
		job.State = "done"
		job.Progress = 1
		job.Segments = segs
		job.Message = fmt.Sprintf("识别到 %d 个片段", len(segs))
	})
}

// detectCuts 用 ffmpeg 的 scene 检测输出切点时间（秒）。
func (a *App) detectCuts(ctx context.Context, path string, threshold, duration float64, onProgress func(float64)) ([]float64, error) {
	ff, err := a.toolPath("ffmpeg")
	if err != nil {
		return nil, err
	}
	// scdet 会逐帧打分并让所有帧通过，因此 -progress 能反映真实进度。
	// threshold 取 0-100，与 API 的 0-1 灵敏度对应。
	expr := fmt.Sprintf("scdet=threshold=%d,metadata=print:key=lavfi.scd.time", int(math.Round(threshold*100)))
	cmd := exec.CommandContext(ctx, ff,
		"-hide_banner", "-nostdin", "-v", "info",
		"-i", path,
		"-filter:v", expr,
		"-an", "-sn",
		"-progress", "pipe:1", "-nostats",
		"-f", "null", "-",
	)

	var cutsMu sync.Mutex
	var cuts []float64
	progressOut := &lineWriter{onLine: func(line string) {
		if t, ok := parseProgressLine(line); ok && duration > 0 && onProgress != nil {
			onProgress(t / duration)
		}
	}}
	var lastErr []string
	errorOut := &lineWriter{onLine: func(line string) {
		if t, ok := parseSceneLine(line); ok {
			cutsMu.Lock()
			cuts = append(cuts, t)
			cutsMu.Unlock()
		}
		if len(lastErr) >= 30 {
			lastErr = lastErr[1:]
		}
		lastErr = append(lastErr, line)
	}}
	cmd.Stdout = progressOut
	cmd.Stderr = errorOut
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("分析超时，请换更短的视频或稍后重试")
		}
		msg := strings.TrimSpace(strings.Join(lastErr, "\n"))
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return nil, errors.New("画面分析失败: " + msg)
	}

	cutsMu.Lock()
	defer cutsMu.Unlock()
	sort.Float64s(cuts)
	// 去掉过于接近的切点，避免碎片段
	var out []float64
	for _, t := range cuts {
		if t < 0.1 || (duration > 0 && t > duration-0.1) {
			continue
		}
		if len(out) > 0 && t-out[len(out)-1] < 0.4 {
			continue
		}
		out = append(out, math.Round(t*1000)/1000)
	}
	return out, nil
}

func buildSegments(cuts []float64, duration float64) []Segment {
	points := []float64{0}
	for _, c := range cuts {
		if c <= 0.15 || c >= duration-0.15 {
			continue
		}
		if c-points[len(points)-1] < 0.5 {
			continue
		}
		points = append(points, c)
	}
	points = append(points, duration)
	var segs []Segment
	for i := 0; i < len(points)-1; i++ {
		start, end := points[i], points[i+1]
		if end-start < 0.3 {
			continue
		}
		segs = append(segs, Segment{
			Index: len(segs) + 1,
			Start: math.Round(start*1000) / 1000,
			End:   math.Round(end*1000) / 1000,
		})
		if len(segs) >= 300 {
			break
		}
	}
	if len(segs) == 0 && duration > 0 {
		segs = []Segment{{Index: 1, Start: 0, End: math.Round(duration*1000) / 1000}}
	}
	return segs
}

func parseTimecode(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "+"))
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.ParseFloat(parts[0], 64)
	m, err2 := strconv.ParseFloat(parts[1], 64)
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return h*3600 + m*60 + sec, true
}

// parsePtsLine 从 showinfo 的输出行里取 pts_time。
// 例：[Parsed_showinfo_1 @ 0x...] n:  12 pts:   7680 pts_time:4.004 flags:K ...
func parsePtsLine(line string) (float64, bool) {
	idx := strings.Index(line, "pts_time:")
	if idx < 0 {
		return 0, false
	}
	rest := line[idx+len("pts_time:"):]
	if end := strings.IndexAny(rest, " \t\r"); end > 0 {
		rest = rest[:end]
	}
	t, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil {
		return 0, false
	}
	return t, true
}

// parseSceneLine 取 scdet + metadata=print 输出的切点秒数。
// 例：[Parsed_metadata_1 @ 0x...] lavfi.scd.time=4.004
func parseSceneLine(line string) (float64, bool) {
	idx := strings.Index(line, "lavfi.scd.time=")
	if idx < 0 {
		return 0, false
	}
	rest := line[idx+len("lavfi.scd.time="):]
	if end := strings.IndexAny(rest, " \t\r"); end > 0 {
		rest = rest[:end]
	}
	t, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil {
		return 0, false
	}
	return t, true
}

// parseProgressLine 解析 -progress 输出的 out_time=HH:MM:SS.ffffff。
func parseProgressLine(line string) (float64, bool) {
	v, ok := strings.CutPrefix(strings.TrimSpace(line), "out_time=")
	if !ok {
		return 0, false
	}
	return parseTimecode(v)
}

// lineWriter 按行回调，用于边跑 ffmpeg 边解析进度与切点。
type lineWriter struct {
	onLine func(string)
	buf    []byte
	mu     sync.Mutex
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if w.onLine != nil {
			w.onLine(line)
		}
	}
	if len(w.buf) > 1<<20 {
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// ---------- 按镜头序号切片落盘 ----------

func (a *App) handleClip(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string  `json:"product"`
		Script  string  `json:"script"`
		Shot    string  `json:"shot"`
		Src     string  `json:"src"`
		File    string  `json:"file"`
		In      float64 `json:"in"`
		Out     float64 `json:"out"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	dir, err := a.scriptDir(req.Product, req.Script, true)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	base, err := sanitizeName(req.Shot)
	if err != nil {
		fail(w, http.StatusBadRequest, "镜头序号无效："+err.Error())
		return
	}
	srcPath, err := a.resolveSource(req.Src, req.Product, req.Script, req.File)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	ff, err := a.toolPath("ffmpeg")
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	probe, err := a.probeFile(r.Context(), srcPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	in, out := req.In, req.Out
	if in < 0 {
		in = 0
	}
	if probe.Duration > 0 {
		if in > probe.Duration {
			in = probe.Duration
		}
		if out > probe.Duration {
			out = probe.Duration
		}
	}
	if out <= in || out-in < 0.05 {
		fail(w, http.StatusBadRequest, "片段区间无效")
		return
	}
	length := out - in

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	tmp := filepath.Join(dir, fmt.Sprintf(".clip-%s.mp4", newID()))
	var errLines []string
	stderr := &lineWriter{onLine: func(line string) {
		if strings.TrimSpace(line) == "" {
			return
		}
		if len(errLines) >= 12 {
			errLines = errLines[1:]
		}
		errLines = append(errLines, line)
	}}
	cmd := exec.CommandContext(ctx, ff,
		"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-ss", strconv.FormatFloat(in, 'f', 3, 64),
		"-i", srcPath,
		"-t", strconv.FormatFloat(length, 'f', 3, 64),
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "160k",
		"-movflags", "+faststart",
		tmp,
	)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		detail := strings.TrimSpace(strings.Join(errLines, " / "))
		if len(detail) > 300 {
			detail = detail[len(detail)-300:]
		}
		if detail == "" {
			detail = err.Error()
		}
		fail(w, http.StatusInternalServerError, "切片失败: "+detail)
		return
	}
	name := base + ".mp4"
	dst := filepath.Join(dir, name)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		fail(w, http.StatusInternalServerError, "写入切片失败: "+err.Error())
		return
	}
	// ffmpeg 生成的文件权限跟随 umask，统一改成 0664
	_ = chmodFile(dst)
	for e := range videoExts {
		if e == ".mp4" {
			continue
		}
		_ = os.Remove(filepath.Join(dir, base+e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file":     name,
		"duration": math.Round(length*1000) / 1000,
		"fps":      probe.FPS,
	})
}
