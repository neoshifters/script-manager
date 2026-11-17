package main

// 剪映草稿导出。
//
// 剪映没有公开的「导入工程」接口，draft_content.json / draft_meta_info.json 的字段
// 随版本变化。这里不凭记忆猜字段：每次导出前到草稿根目录里找一份用户自己保存过的
// 草稿当模板，复制它的目录结构与字段，只替换素材、时间轴与名称。
//
// 生成流程：
//   1. 在草稿根目录中挑一份最新、且含 draft_content.json 的草稿作为模板；
//   2. 在根目录下建隐藏临时目录，复制模板的附属文件（不含两份 json）；
//   3. 把镜头素材复制进 <草稿>/materials/，用 Windows 路径引用；
//   4. 按「模板里的原型对象」生成 materials / tracks / duration；
//   5. 全部成功后再重命名成正式目录，失败则整目录删除，绝不留下残稿。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultDraftRoot = "/drafts/JianyingPro Drafts"
	draftContentName = "draft_content.json"
	draftMetaName    = "draft_meta_info.json"
	draftTmpPrefix   = ".sm-tmp-"
)

// draftTemplate 描述一份可作为模板的已存在草稿。
type draftTemplate struct {
	Dir       string // 草稿目录
	ContentAt string // draft_content.json 相对草稿目录的路径（可能是 draft/draft_content.json）
	MetaAt    string // draft_meta_info.json 相对路径，可能为空
	ModTime   time.Time
}

type draftExportResult struct {
	Draft    string   `json:"draft"`
	NasPath  string   `json:"nasPath"`
	WinPath  string   `json:"winPath"`
	Clips    int      `json:"clips"`
	Texts    int      `json:"texts"`
	Warnings []string `json:"warnings,omitempty"`
}

// draftClip 是一个已复制进草稿目录的素材。
type draftClip struct {
	name string
	win  string
	in   float64
	dur  float64
}

// draftBlock 对应一个镜头：可选的视频素材 + 可选的文案，二者共用同一段起止时间。
type draftBlock struct {
	clip  *draftClip
	text  string
	start float64
	dur   float64
}

func failStage(w http.ResponseWriter, code int, stage, msg string) {
	writeJSON(w, code, map[string]string{"stage": stage, "error": msg})
}

func (a *App) jianyingConfig() (root, nas, win string) {
	a.envMu.Lock()
	s := a.settings
	a.envMu.Unlock()
	root = strings.TrimSpace(s.DraftRoot)
	if root == "" {
		root = strings.TrimSpace(os.Getenv("SM_DRAFT_ROOT"))
	}
	if root == "" {
		root = defaultDraftRoot
	}
	nas = strings.TrimSpace(s.NasPrefix)
	if nas == "" {
		nas = strings.TrimSpace(os.Getenv("SM_DRAFT_NAS"))
	}
	win = strings.TrimSpace(s.WinPrefix)
	if win == "" {
		win = strings.TrimSpace(os.Getenv("SM_DRAFT_WIN"))
	}
	// 直接跑在 Windows 上时没有容器路径要翻译，用盘符自身当前缀即可
	if nas == "" && win == "" && runtime.GOOS == "windows" {
		if vol := filepath.VolumeName(root); vol != "" {
			nas, win = vol, vol
		}
	}
	return root, nas, win
}

// exportTemplateInfo 是预检里展示给前端的一份草稿模板。
type exportTemplateInfo struct {
	Name     string `json:"name"`
	Usable   bool   `json:"usable"`
	Reason   string `json:"reason,omitempty"`
	HasVideo bool   `json:"hasVideo"`
	Modified string `json:"modified,omitempty"`
}

type draftTemplateCandidate struct {
	tpl     draftTemplate
	content map[string]any
	reason  string
}

const templateEncryptedHint = "新版剪映会给 draft_content.json 加密或附加二进制数据，本程序无法读取。请用较旧版本的剪映（例如 5.x）打开这份草稿后重新保存/另存一份，放到同一个草稿目录里作为模板；或者直接改用「下载 CSV + 时间码文案」兜底。"

// inspectDraftContent 读取并判断一份 draft_content.json 能否作为模板。
// 新版剪映可能在 JSON 之后附加二进制/加密数据，或整个文件已加密；
// 这里返回可读的中文原因，避免把 json.Unmarshal 的原始报错直接抛给用户。
func inspectDraftContent(path string) (map[string]any, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "读取失败：" + err.Error()
	}
	if len(b) == 0 {
		return nil, "文件是空的（0 字节）"
	}
	if !utf8.Valid(b) {
		return nil, "不是 UTF-8 文本（疑似被加密或压缩成二进制）"
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return nil, "文件只有空白字符"
	}
	if trimmed[0] != '{' {
		return nil, fmt.Sprintf("开头不是 JSON 对象（首字节 0x%02X），疑似被加密或压缩", trimmed[0])
	}
	if i := bytes.IndexByte(trimmed, 0); i >= 0 {
		return nil, fmt.Sprintf("第 %d 字节出现 NUL，疑似附加了二进制数据", i)
	}
	var m map[string]any
	if err := json.Unmarshal(trimmed, &m); err == nil {
		if m == nil {
			return nil, "顶层不是 JSON 对象"
		}
		return m, ""
	}
	// 用 Decoder 找出顶层值结束位置，区分「尾部附加数据」和普通语法错误
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, "JSON 解析失败：" + err.Error()
	}
	if rest := bytes.TrimSpace(trimmed[dec.InputOffset():]); len(rest) > 0 {
		return nil, fmt.Sprintf("JSON 顶层值之后还有 %d 字节多余数据（首个字节 0x%02X %q），这是新版剪映给草稿附加的二进制/加密数据", len(rest), rest[0], string(rune(rest[0])))
	}
	return nil, "JSON 结构异常，无法作为模板"
}

func hasTemplateVideo(content map[string]any) bool {
	m, _ := content["materials"].(map[string]any)
	if m == nil {
		return false
	}
	arr, _ := m["videos"].([]any)
	return len(arr) > 0
}

// scanDraftTemplates 列出草稿根目录下的所有草稿，并标注能否解析、原因。
func scanDraftTemplates(root string) ([]draftTemplateCandidate, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("草稿目录不存在：%s", root)
		}
		return nil, fmt.Errorf("草稿目录不可读：%v", err)
	}
	var list []draftTemplate
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		tpl := draftTemplate{Dir: dir}
		if info, err := os.Stat(filepath.Join(dir, draftContentName)); err == nil && !info.IsDir() {
			tpl.ContentAt = draftContentName
			tpl.ModTime = info.ModTime()
			if mi, err := os.Stat(filepath.Join(dir, draftMetaName)); err == nil && !mi.IsDir() {
				tpl.MetaAt = draftMetaName
			}
		} else if info, err := os.Stat(filepath.Join(dir, "draft", draftContentName)); err == nil && !info.IsDir() {
			tpl.ContentAt = filepath.Join("draft", draftContentName)
			tpl.ModTime = info.ModTime()
			if mi, err := os.Stat(filepath.Join(dir, "draft", draftMetaName)); err == nil && !mi.IsDir() {
				tpl.MetaAt = filepath.Join("draft", draftMetaName)
			}
		} else {
			continue
		}
		list = append(list, tpl)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ModTime.After(list[j].ModTime) })
	out := make([]draftTemplateCandidate, 0, len(list))
	for _, tpl := range list {
		content, reason := inspectDraftContent(filepath.Join(tpl.Dir, tpl.ContentAt))
		if reason == "" && content["materials"] == nil {
			reason = "缺少 materials 字段，不是有效的剪映草稿"
		}
		out = append(out, draftTemplateCandidate{tpl: tpl, content: content, reason: reason})
	}
	return out, nil
}

// chooseDraftTemplate 按名称或自动挑一份可解析的模板。
// want 为空时优先选最新且带视频的可用草稿；全部不可用时返回聚合原因。
func chooseDraftTemplate(root, want string) (draftTemplate, map[string]any, error) {
	cands, err := scanDraftTemplates(root)
	if err != nil {
		return draftTemplate{}, nil, err
	}
	if len(cands) == 0 {
		return draftTemplate{}, nil, fmt.Errorf("草稿目录里还没有草稿（需要含 %s）。请先在剪映里把一份草稿保存到该目录，再回来导出", draftContentName)
	}
	nameOf := func(c draftTemplateCandidate) string { return filepath.Base(c.tpl.Dir) }
	if want != "" {
		for _, c := range cands {
			if nameOf(c) != want {
				continue
			}
			if c.reason != "" {
				return draftTemplate{}, nil, fmt.Errorf("模板「%s」不可用：%s。\n\n%s", nameOf(c), c.reason, templateEncryptedHint)
			}
			return c.tpl, c.content, nil
		}
		names := make([]string, 0, len(cands))
		for _, c := range cands {
			names = append(names, nameOf(c))
		}
		return draftTemplate{}, nil, fmt.Errorf("找不到名为「%s」的模板草稿。当前草稿目录里有：%s", want, strings.Join(names, "、"))
	}
	var usable []draftTemplateCandidate
	var bad []string
	for _, c := range cands {
		switch {
		case c.reason == "":
			usable = append(usable, c)
		case len(bad) < 5:
			bad = append(bad, "· "+nameOf(c)+"："+c.reason)
		}
	}
	if len(usable) == 0 {
		return draftTemplate{}, nil, fmt.Errorf("草稿目录里没有能用作模板的草稿（共 %d 份，全部无法解析）：\n%s\n\n%s", len(cands), strings.Join(bad, "\n"), templateEncryptedHint)
	}
	// 优先最新且带视频素材的草稿，避免把上一次生成的空草稿当成模板
	for _, c := range usable {
		if hasTemplateVideo(c.content) {
			return c.tpl, c.content, nil
		}
	}
	return usable[0].tpl, usable[0].content, nil
}

func readJSONMap(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func deepCopyMap(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return map[string]any{}
	}
	return out
}

func copyAny(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func numOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f
	}
	return 0
}

// scaleTime 参照模板里同一字段的数量级，把秒换算成模板使用的单位（微秒 / 毫秒 / 秒）。
func scaleTime(proto any, sec float64) any {
	unit := 1.0
	switch proto.(type) {
	case string:
		unit = 1e6
	default:
		if n := numOf(proto); n >= 1e5 {
			unit = 1e6
		} else if n >= 1e3 {
			unit = 1e3
		}
	}
	v := sec * unit
	if _, ok := proto.(string); ok {
		return strconv.FormatInt(int64(v+0.5), 10)
	}
	if _, ok := proto.(float64); ok {
		return v
	}
	return int64(v + 0.5)
}

func newDraftID(proto any) string {
	s, _ := proto.(string)
	if len(s) == 32 {
		return strings.ToUpper(newID() + newID())
	}
	return newID() + newID()
}

func sanitizeDraftName(name string) string {
	r := strings.NewReplacer("\\", "-", "/", "-", ":", "-", "*", "-", "?", "-", "\"", "-", "<", "-", ">", "-", "|", "-")
	out := strings.TrimSpace(r.Replace(name))
	out = strings.TrimRight(out, ". ")
	if out == "" {
		out = "未命名草稿"
	}
	rs := []rune(out)
	if len(rs) > 80 {
		out = string(rs[:80])
	}
	return out
}

func uniqueDraftDir(root, base string) string {
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
	}
	return filepath.Join(root, base+"-"+strconv.FormatInt(time.Now().Unix(), 10))
}

func copyPlainFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := ensureDir(filepath.Dir(dst)); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	// 草稿里的素材文件也要能在宿主机/剪映里读到，固定 0664
	_ = out.Chmod(filePerm)
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTemplateSidecars 复制模板里的附属文件（跳过两份 json 与子目录）。
func copyTemplateSidecars(tpl draftTemplate, dstDir string, warnings *[]string) {
	rels := []string{"."}
	if dir := filepath.Dir(tpl.ContentAt); dir != "." {
		rels = append(rels, dir)
	}
	for _, rel := range rels {
		srcDir := filepath.Join(tpl.Dir, rel)
		entries, err := os.ReadDir(srcDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if name == draftContentName || name == draftMetaName {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Size() > 8<<20 {
				continue
			}
			dst := filepath.Join(dstDir, rel, name)
			if err := copyPlainFile(filepath.Join(srcDir, name), dst); err != nil {
				*warnings = append(*warnings, "附属文件复制失败："+name)
			}
		}
	}
}

// toWindowsPath 把草稿目录内的 NAS 路径翻译成运行剪映的 Windows 上可访问的路径。
func toWindowsPath(nasPath, nasPrefix, winPrefix string) (string, error) {
	// 用 ToSlash 统一分隔符；盘符（C:）不能走 filepath.Clean，否则会变成 "C:."
	raw := strings.ReplaceAll(strings.TrimSpace(nasPrefix), `\`, "/")
	if len(raw) == 2 && raw[1] == ':' {
		nasPrefix = strings.ToUpper(raw)
	} else {
		nasPrefix = filepath.ToSlash(filepath.Clean(raw))
	}
	winPrefix = strings.TrimRight(strings.TrimSpace(winPrefix), `\`)
	clean := filepath.ToSlash(filepath.Clean(nasPath))
	if nasPrefix == "" || nasPrefix == "." || winPrefix == "" {
		return "", errors.New("还没有配置「NAS 草稿路径 ↔ Windows 路径」映射")
	}
	compareClean, comparePrefix := clean, nasPrefix
	if runtime.GOOS == "windows" {
		compareClean, comparePrefix = strings.ToLower(clean), strings.ToLower(nasPrefix)
	}
	if compareClean != comparePrefix && !strings.HasPrefix(compareClean, comparePrefix+"/") {
		return "", fmt.Errorf("草稿目录 %s 不在映射前缀 %s 之下，无法翻译成 Windows 路径", clean, nasPrefix)
	}
	rel := strings.TrimPrefix(clean, nasPrefix)
	if rel == clean && compareClean != clean && strings.HasPrefix(compareClean, comparePrefix) {
		// 只有大小写不同（Windows 盘符）时按长度截取
		rel = clean[len(nasPrefix):]
	}
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return winPrefix, nil
	}
	// Windows 前缀可能已经包含了草稿目录（例如 Y:\草稿\JianyingPro Drafts），
	// 这里去掉重复的目录段，避免拼成 ...\JianyingPro Drafts\JianyingPro Drafts。
	relParts := strings.Split(rel, "/")
	winSlash := strings.ReplaceAll(winPrefix, `\`, "/")
	for k := len(relParts); k > 0; k-- {
		tail := strings.Join(relParts[:k], "/")
		if len(winSlash) < len(tail) || !strings.EqualFold(winSlash[len(winSlash)-len(tail):], tail) {
			continue
		}
		if len(winSlash) > len(tail) && winSlash[len(winSlash)-len(tail)-1] != '/' {
			continue
		}
		relParts = relParts[k:]
		break
	}
	if len(relParts) == 0 {
		return winPrefix, nil
	}
	return winPrefix + `\` + strings.Join(relParts, `\`), nil
}

// registerDraft 把新草稿登记进草稿根目录的 root_meta_info.json。
// 剪映的草稿列表以这份索引为准；文件不存在时返回 nil（部分版本靠扫描目录）。
// 条目字段同样从模板草稿的现有条目克隆，只替换 id / 名称 / 路径 / 时间。
func (a *App) registerDraft(root string, tpl draftTemplate, name, winDir, winRoot, draftID string, durSec float64) error {
	path := filepath.Join(root, "root_meta_info.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var meta map[string]any
	if err := json.Unmarshal(b, &meta); err != nil {
		return fmt.Errorf("root_meta_info.json 解析失败：%v", err)
	}
	arr, ok := meta["all_draft_store"].([]any)
	if !ok {
		return errors.New("root_meta_info.json 里没有 all_draft_store")
	}
	var proto map[string]any
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		p, _ := m["draft_fold_path"].(string)
		if strings.HasSuffix(p, `\`+filepath.Base(tpl.Dir)) || strings.HasSuffix(p, "/"+filepath.Base(tpl.Dir)) {
			proto = m
			break
		}
	}
	if proto == nil && len(arr) > 0 {
		proto, _ = arr[0].(map[string]any)
	}
	if proto == nil {
		return errors.New("root_meta_info.json 里没有可参考的草稿条目")
	}

	entry := deepCopyMap(proto)
	now := time.Now().UnixMicro()
	setString(entry, "draft_id", draftID)
	setString(entry, "draft_name", name)
	setString(entry, "draft_fold_path", winDir)
	if winRoot != "" {
		setString(entry, "draft_root_path", strings.TrimRight(winRoot, `\`)+`\`)
	}
	setTime(entry, "tm_draft_create", now)
	setTime(entry, "tm_draft_modified", now)
	setTime(entry, "tm_duration", int64(durSec*1e6))
	if got, _ := entry["draft_fold_path"].(string); got != winDir {
		return errors.New("模板条目缺少 draft_fold_path 字段，无法登记")
	}

	meta["all_draft_store"] = append([]any{entry}, arr...)
	out, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}

// buildDraft 在草稿根目录生成草稿目录，返回结果。任何一步失败都会删除临时目录。
// templateName 为空时自动挑选一份能解析的模板。
func (a *App) buildDraft(doc Doc, draftRoot, nasPrefix, winPrefix, baseName, templateName string) (draftExportResult, string, error) {
	res := draftExportResult{}
	if _, err := os.Stat(draftRoot); err != nil {
		return res, "配置", fmt.Errorf("草稿目录不存在：%s（点右上角 ffmpeg 状态按钮，在「设置 → 剪映草稿导出」里检查草稿根目录，并确认 compose 已把宿主机目录读写挂载到容器内该路径）", draftRoot)
	}
	if winPrefix == "" {
		return res, "配置", errors.New("还没有填「Windows 路径前缀」。点右上角 ffmpeg 状态按钮，打开「设置 → 剪映草稿导出 → Windows 路径前缀」填写，例如 Windows 上把草稿目录映射成 Z:，就填 Z:\\")
	}
	if len(doc.Shots) == 0 {
		return res, "脚本", errors.New("当前脚本还没有镜头：先在左侧脚本里添加镜头，再导出")
	}
	tpl, content, err := chooseDraftTemplate(draftRoot, templateName)
	if err != nil {
		return res, "模板", err
	}
	mats, _ := content["materials"].(map[string]any)
	videoProto := firstObject(mats, "videos")
	textProto := firstObject(mats, "texts")
	tracks, _ := content["tracks"].([]any)
	videoTrack := firstTrack(tracks, "video")
	textTrack := firstTrack(tracks, "text")

	finalDir := uniqueDraftDir(draftRoot, sanitizeDraftName(baseName))
	// 素材最终会随目录改名到 finalDir，Windows 路径必须按 finalDir 计算
	winDir, err := toWindowsPath(finalDir, nasPrefix, winPrefix)
	if err != nil {
		return res, "配置", err
	}
	tmpDir := filepath.Join(draftRoot, draftTmpPrefix+newID())
	if err := ensureDir(tmpDir); err != nil {
		return res, "落盘", fmt.Errorf("无法在草稿目录创建临时文件夹（检查挂载是否为读写）：%v", err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	copyTemplateSidecars(tpl, tmpDir, &res.Warnings)

	expectedClips := 0
	for _, shot := range doc.Shots {
		if shot.Video != nil && strings.TrimSpace(shot.Video.File) != "" {
			expectedClips++
		}
	}
	blocks, warns := a.collectDraftBlocks(doc, tmpDir, winDir)
	res.Warnings = append(res.Warnings, warns...)
	clipCount := 0
	textCount := 0
	for _, b := range blocks {
		if b.clip != nil {
			clipCount++
		}
		if b.text != "" {
			textCount++
		}
	}
	res.Clips = clipCount
	res.Texts = textCount
	if expectedClips > 0 && clipCount == 0 {
		reason := strings.Join(res.Warnings, "；")
		if reason == "" {
			reason = "素材文件不可读"
		}
		return res, "素材", fmt.Errorf("有 %d 个镜头引用了视频，但没有一个素材能复制进草稿：%s", expectedClips, reason)
	}
	if clipCount > 0 && videoProto == nil {
		return res, "模板", errors.New("模板草稿里没有视频素材，无法参考字段结构。请先在剪映里放一段视频并保存，或改用「兜底导出」")
	}
	if clipCount > 0 && videoTrack == nil {
		return res, "模板", errors.New("模板草稿里没有视频轨道，无法参考轨道结构。请在剪映里放一段视频并保存后再试")
	}
	if textCount > 0 && (textProto == nil || textTrack == nil) {
		res.Warnings = append(res.Warnings, "模板里没有文字素材/文字轨道，本脚本的文案没有写进草稿")
	}

	newContent, err := buildDraftContent(content, blocks, videoProto, textProto, videoTrack, textTrack, baseName)
	if err != nil {
		return res, "生成", err
	}
	contentAt := filepath.Join(tmpDir, tpl.ContentAt)
	if err := ensureDir(filepath.Dir(contentAt)); err != nil {
		return res, "落盘", err
	}
	if err := writeJSONFile(contentAt, newContent); err != nil {
		return res, "落盘", fmt.Errorf("写入 draft_content.json 失败：%v", err)
	}

	draftID := ""
	if tpl.MetaAt != "" {
		meta, merr := readJSONMap(filepath.Join(tpl.Dir, tpl.MetaAt))
		if merr == nil {
			now := time.Now().UnixMicro()
			draftID = newDraftID(meta["draft_id"])
			setString(meta, "draft_name", filepath.Base(finalDir))
			setString(meta, "draft_fold_path", winDir)
			setString(meta, "draft_id", draftID)
			setTime(meta, "tm_draft_create", now)
			setTime(meta, "tm_draft_modified", now)
			setTime(meta, "tm_duration", int64(blocksDuration(blocks)*1e6))
			metaAt := filepath.Join(tmpDir, tpl.MetaAt)
			if err := writeJSONFile(metaAt, meta); err != nil {
				res.Warnings = append(res.Warnings, "draft_meta_info.json 写入失败："+err.Error())
			}
		}
	}
	if draftID == "" {
		draftID = newID() + newID()
	}

	if err := os.Rename(tmpDir, finalDir); err != nil {
		return res, "落盘", fmt.Errorf("把临时草稿改名成正式目录失败：%v", err)
	}
	success = true

	// 剪映的草稿列表以根目录的 root_meta_info.json 为索引；能登记就登记，失败只提示
	winRoot, _ := toWindowsPath(draftRoot, nasPrefix, winPrefix)
	if rerr := a.registerDraft(draftRoot, tpl, filepath.Base(finalDir), winDir, winRoot, draftID, blocksDuration(blocks)); rerr != nil {
		res.Warnings = append(res.Warnings, "草稿已生成，但更新草稿索引 root_meta_info.json 失败，剪映里可能看不到它："+rerr.Error())
	}
	res.Draft = filepath.Base(finalDir)
	res.NasPath = finalDir
	res.WinPath = winDir
	return res, "", nil
}

func (a *App) collectDraftBlocks(doc Doc, tmpDir, winDir string) ([]draftBlock, []string) {
	var warnings []string
	scriptDir, _ := a.scriptDir(doc.Product, doc.Script, true)
	materialDir := filepath.Join(tmpDir, "materials")
	blocks := make([]draftBlock, 0, len(doc.Shots))
	start := 0.0
	idx := 0
	for _, shot := range doc.Shots {
		text := strings.TrimSpace(shot.Copy)
		if text == "" {
			text = strings.TrimSpace(shot.Narration)
		}
		b := draftBlock{text: text, start: start}
		duration := 0.0
		if shot.Video != nil && shot.Video.File != "" {
			abs := filepath.Join(scriptDir, shot.Video.File)
			if shot.Video.Src == "temp" {
				abs = filepath.Join(a.dataDir, tempDirName, shot.Video.File)
			}
			if _, err := os.Stat(abs); err != nil {
				warnings = append(warnings, fmt.Sprintf("镜头 %s 的素材文件找不到，已跳过：%s", shot.No, shot.Video.File))
			} else {
				in, out := shot.Video.In, shot.Video.Out
				if out <= in {
					if pr, err := a.probeFile(context.Background(), abs); err == nil && pr.Duration > 0 {
						in, out = 0, pr.Duration
					} else {
						in, out = 0, 5
						warnings = append(warnings, fmt.Sprintf("镜头 %s 的时长未知（ffprobe 不可用），按 5 秒处理", shot.No))
					}
				}
				idx++
				name := fmt.Sprintf("%02d-%s", idx, filepath.Base(abs))
				if err := copyPlainFile(abs, filepath.Join(materialDir, name)); err != nil {
					warnings = append(warnings, fmt.Sprintf("镜头 %s 的素材复制失败，已跳过：%v", shot.No, err))
				} else {
					b.clip = &draftClip{name: name, win: winDir + `\materials\` + name, in: in, dur: out - in}
					duration = out - in
				}
			}
		}
		if duration <= 0 {
			duration = 3.5 // 没有素材的镜头在草稿里占一个文案位
		}
		b.dur = duration
		start += duration
		blocks = append(blocks, b)
	}
	return blocks, warnings
}

func blocksDuration(blocks []draftBlock) float64 {
	t := 0.0
	for _, b := range blocks {
		t += b.dur
	}
	return t
}

func firstObject(mats map[string]any, key string) map[string]any {
	if mats == nil {
		return nil
	}
	arr, _ := mats[key].([]any)
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			return m
		}
	}
	return nil
}

func firstTrack(tracks []any, kind string) map[string]any {
	for _, it := range tracks {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == kind {
			if segs, _ := m["segments"].([]any); len(segs) > 0 {
				return m
			}
		}
	}
	return nil
}

func firstSegment(track map[string]any) map[string]any {
	segs, _ := track["segments"].([]any)
	for _, s := range segs {
		if m, ok := s.(map[string]any); ok {
			return m
		}
	}
	return nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func setString(m map[string]any, key, val string) {
	if _, ok := m[key]; ok {
		m[key] = val
	}
}

func setTime(m map[string]any, key string, val int64) {
	cur, ok := m[key]
	if !ok {
		return
	}
	if _, isStr := cur.(string); isStr {
		m[key] = strconv.FormatInt(val, 10)
		return
	}
	m[key] = val
}

func buildDraftContent(content map[string]any, blocks []draftBlock,
	videoProto, textProto, videoTrack, textTrack map[string]any, draftName string) (map[string]any, error) {

	out := deepCopyMap(content)
	out["id"] = newDraftID(content["id"])
	if _, ok := out["name"]; ok {
		out["name"] = draftName
	}
	if v, ok := content["duration"]; ok {
		out["duration"] = scaleTime(v, blocksDuration(blocks))
	}

	newMats := map[string]any{}
	if mats, ok := content["materials"].(map[string]any); ok {
		for k, v := range mats {
			if _, isArr := v.([]any); isArr {
				newMats[k] = []any{}
			} else {
				newMats[k] = copyAny(v)
			}
		}
	}
	newMats["videos"] = []any{}
	newMats["texts"] = []any{}

	var newTracks []any
	hasClip := false
	for _, b := range blocks {
		if b.clip != nil {
			hasClip = true
			break
		}
	}
	if hasClip && videoProto != nil && videoTrack != nil {
		segProto := firstSegment(videoTrack)
		if segProto == nil {
			return nil, errors.New("模板的视频轨道里没有片段，无法参考片段字段")
		}
		videos := make([]any, 0, len(blocks))
		segs := make([]any, 0, len(blocks))
		for _, b := range blocks {
			if b.clip == nil {
				continue
			}
			mat := deepCopyMap(videoProto)
			matID := newDraftID(mat["id"])
			mat["id"] = matID
			mat["path"] = b.clip.win
			mat["material_name"] = b.clip.name
			if _, ok := mat["duration"]; ok {
				mat["duration"] = scaleTime(mat["duration"], b.clip.dur)
			}
			videos = append(videos, mat)

			seg := deepCopyMap(segProto)
			seg["id"] = newDraftID(seg["id"])
			seg["material_id"] = matID
			setRange(seg, b.start, b.clip.dur, b.clip.in)
			if _, ok := seg["speed"]; ok {
				seg["speed"] = 1.0
			}
			segs = append(segs, seg)
		}
		newMats["videos"] = videos
		track := deepCopyMap(videoTrack)
		track["id"] = newDraftID(track["id"])
		track["segments"] = segs
		newTracks = append(newTracks, track)
	}

	if textProto != nil && textTrack != nil {
		segProto := firstSegment(textTrack)
		if segProto != nil {
			track := deepCopyMap(textTrack)
			segs := make([]any, 0, len(blocks))
			mats := make([]any, 0, len(blocks))
			for _, b := range blocks {
				if b.text == "" {
					continue
				}
				mat := deepCopyMap(textProto)
				matID := newDraftID(mat["id"])
				mat["id"] = matID
				if c, ok := mat["content"].(string); ok {
					var parsed map[string]any
					if json.Unmarshal([]byte(c), &parsed) == nil {
						parsed["text"] = b.text
						if raw, err := json.Marshal(parsed); err == nil {
							mat["content"] = string(raw)
						}
					}
				}
				mats = append(mats, mat)

				seg := deepCopyMap(segProto)
				seg["id"] = newDraftID(seg["id"])
				seg["material_id"] = matID
				setRange(seg, b.start, b.dur, 0)
				segs = append(segs, seg)
			}
			if len(mats) > 0 {
				newMats["texts"] = mats
				track["id"] = newDraftID(track["id"])
				track["segments"] = segs
				newTracks = append(newTracks, track)
			}
		}
	}

	out["materials"] = newMats
	if len(newTracks) == 0 {
		return nil, errors.New("没有生成任何轨道：模板里缺少视频 / 文字轨道")
	}
	out["tracks"] = newTracks
	return out, nil
}

// setRange 按模板里 target_timerange / source_timerange 的字段与单位写入起止时间。
func setRange(seg map[string]any, startSec, durSec, inSec float64) {
	tr, _ := seg["target_timerange"].(map[string]any)
	if tr == nil {
		tr = map[string]any{}
		seg["target_timerange"] = tr
	}
	tr["start"] = scaleTime(tr["start"], startSec)
	tr["duration"] = scaleTime(tr["duration"], durSec)

	if src, ok := seg["source_timerange"].(map[string]any); ok {
		src["start"] = scaleTime(src["start"], inSec)
		src["duration"] = scaleTime(src["duration"], durSec)
	}
}

/* ---------------- HTTP 接口 ---------------- */

func (a *App) handleExportDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product  string `json:"product"`
		Script   string `json:"script"`
		Template string `json:"template"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	dir, err := a.scriptDir(req.Product, req.Script, true)
	if err != nil {
		failStage(w, http.StatusNotFound, "配置", err.Error())
		return
	}
	var doc Doc
	if room := a.existingRoom(dir); room != nil {
		doc = room.docNow()
	} else {
		doc, err = readDoc(filepath.Join(dir, docFileName), req.Product, req.Script)
		if err != nil {
			failStage(w, http.StatusInternalServerError, "读取", err.Error())
			return
		}
	}
	if doc.Product == "" {
		doc.Product = req.Product
	}
	if doc.Script == "" {
		doc.Script = req.Script
	}

	root, nas, win := a.jianyingConfig()
	base := sanitizeDraftName(req.Product + "-" + req.Script)
	res, stage, err := a.buildDraft(doc, root, nas, win, base, strings.TrimSpace(req.Template))
	if err != nil {
		code := http.StatusInternalServerError
		switch stage {
		case "配置", "模板", "脚本", "素材":
			code = http.StatusBadRequest
		}
		failStage(w, code, stage, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

/* ---------------- 导出前预检 ---------------- */

// exportPrecheckItem 是预检里的单条结果。Blocking=false 表示只是提醒，不阻止导出。
type exportPrecheckItem struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	OK       bool   `json:"ok"`
	Blocking bool   `json:"blocking"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

type exportPrecheckResult struct {
	OK              bool                 `json:"ok"`
	Items           []exportPrecheckItem `json:"items"`
	Summary         string               `json:"summary"`
	Draft           map[string]string    `json:"draft"`
	Templates       []exportTemplateInfo `json:"templates,omitempty"`
	DefaultTemplate string               `json:"defaultTemplate,omitempty"`
}

func precheckOK(key, label, detail string) exportPrecheckItem {
	return exportPrecheckItem{Key: key, Label: label, OK: true, Blocking: true, Detail: detail}
}

func precheckBad(key, label, detail, hint string, blocking bool) exportPrecheckItem {
	return exportPrecheckItem{Key: key, Label: label, OK: false, Blocking: blocking, Detail: detail, Hint: hint}
}

// handleExportPrecheck 一次性检查导出所需条件，把「缺什么、去哪里补」全部返回给前端。
func (a *App) handleExportPrecheck(w http.ResponseWriter, r *http.Request) {
	product := strings.TrimSpace(r.URL.Query().Get("product"))
	script := strings.TrimSpace(r.URL.Query().Get("script"))
	root, nas, win := a.jianyingConfig()

	res := exportPrecheckResult{Draft: map[string]string{"root": root, "nas": nas, "win": win}}
	items := make([]exportPrecheckItem, 0, 7)

	// 1. 草稿根目录：存在、是目录、容器内可写
	rootUsable := false
	if root == "" {
		items = append(items, precheckBad("root", "草稿根目录", "没有配置草稿根目录",
			"点右上角 ffmpeg 状态按钮，在「设置 → 剪映草稿导出」里填写容器内的草稿目录，例如 /drafts/JianyingPro Drafts", true))
	} else if info, err := os.Stat(root); err != nil {
		items = append(items, precheckBad("root", "草稿根目录", "目录不存在或无法访问："+root,
			"确认 docker-compose 已把宿主机草稿目录挂载到容器内这个路径（只读挂载也会失败），再核对「设置」里的草稿根目录", true))
	} else if !info.IsDir() {
		items = append(items, precheckBad("root", "草稿根目录", "这个路径是一个文件，不是文件夹："+root,
			"在「设置 → 剪映草稿导出」改成剪映的草稿文件夹路径", true))
	} else if probe, perr := os.CreateTemp(root, ".sm-precheck-*"); perr != nil {
		items = append(items, precheckBad("root", "草稿根目录", "目录存在，但容器没有写入权限："+root,
			"在 NAS 上执行 sudo chown -R 1000:1000 <草稿目录> 和 sudo chmod -R u=rwX,g=rwX,o=rX <草稿目录>，并确认 compose 没有写 :ro", true))
	} else {
		name := probe.Name()
		_ = probe.Close()
		_ = os.Remove(name)
		rootUsable = true
		items = append(items, precheckOK("root", "草稿根目录", root+"（可写）"))
	}

	// 2. Windows 路径前缀
	if win == "" {
		items = append(items, precheckBad("winPrefix", "Windows 路径前缀", "没有配置",
			"点右上角 ffmpeg 状态按钮，打开「设置 → 剪映草稿导出 → Windows 路径前缀」填写运行剪映的电脑访问草稿目录的路径，例如 Z:\\", true))
	} else {
		items = append(items, precheckOK("winPrefix", "Windows 路径前缀", win))
	}

	// 3. NAS ↔ Windows 映射是否可用
	if nas == "" {
		items = append(items, precheckBad("mapping", "NAS ↔ Windows 路径映射", "NAS 路径前缀为空，无法把草稿目录翻译成 Windows 路径",
			"点右上角 ffmpeg 状态按钮，打开「设置 → 剪映草稿导出 → NAS 路径前缀」填写草稿目录在容器内的前缀，例如 /drafts", true))
	} else if win != "" {
		if mapped, err := toWindowsPath(root, nas, win); err != nil {
			items = append(items, precheckBad("mapping", "NAS ↔ Windows 路径映射", err.Error(),
				"把 NAS 路径前缀改成草稿根目录的上级路径（当前根目录："+root+"）", true))
		} else {
			detail := "草稿根目录映射为：" + mapped
			if product != "" && script != "" {
				name := sanitizeDraftName(product + "-" + script)
				if draftWin, e := toWindowsPath(filepath.Join(root, name), nas, win); e == nil {
					detail += "\n新草稿将生成在：" + draftWin + "（重名时自动加 -2）"
				}
			}
			items = append(items, precheckOK("mapping", "NAS ↔ Windows 路径映射", detail))
		}
	}

	// 4. 模板草稿：列出全部并标注能否解析，能用的自动挑一份
	if !rootUsable {
		items = append(items, precheckBad("template", "模板草稿", "草稿根目录不可用，暂时无法检查模板",
			"先解决上面的「草稿根目录」问题，再点「重新预检」", true))
	} else if cands, err := scanDraftTemplates(root); err != nil {
		items = append(items, precheckBad("template", "模板草稿", err.Error(),
			"确认草稿根目录可读后点「重新预检」", true))
	} else if len(cands) == 0 {
		items = append(items, precheckBad("template", "模板草稿", "草稿目录里还没有任何草稿",
			"在剪映里把一份带视频和文字、能正常打开的草稿保存到草稿根目录，作为模板", true))
	} else {
		infos := make([]exportTemplateInfo, 0, len(cands))
		var bad []string
		usableCount := 0
		firstUsable, firstWithVideo := "", ""
		for _, c := range cands {
			name := filepath.Base(c.tpl.Dir)
			infos = append(infos, exportTemplateInfo{
				Name:     name,
				Usable:   c.reason == "",
				Reason:   c.reason,
				HasVideo: c.reason == "" && hasTemplateVideo(c.content),
				Modified: c.tpl.ModTime.Format("2006-01-02 15:04"),
			})
			if c.reason == "" {
				usableCount++
				if firstUsable == "" {
					firstUsable = name
				}
				if firstWithVideo == "" && hasTemplateVideo(c.content) {
					firstWithVideo = name
				}
			} else if len(bad) < 5 {
				bad = append(bad, "· "+name+"："+c.reason)
			}
		}
		chosen := firstWithVideo
		if chosen == "" {
			chosen = firstUsable
		}
		res.Templates = infos
		res.DefaultTemplate = chosen
		if chosen == "" {
			items = append(items, precheckBad("template", "模板草稿",
				fmt.Sprintf("草稿目录里有 %d 份草稿，但都不是可解析的 JSON：\n%s", len(cands), strings.Join(bad, "\n")),
				"新版剪映会给草稿加密或附加二进制数据：用旧版剪映（如 5.x）另存一份模板放进草稿根目录；或直接用下面的「下载 CSV + 时间码文案」兜底", true))
		} else {
			detail := fmt.Sprintf("将使用：%s（共 %d 份可用、%d 份不可用）", chosen, usableCount, len(cands)-usableCount)
			if len(bad) > 0 {
				detail += "\n已跳过：\n" + strings.Join(bad, "\n")
			}
			items = append(items, precheckOK("template", "模板草稿", detail))
		}
	}

	// 5. 当前脚本与镜头
	var doc Doc
	var scriptDir string
	scriptUsable := false
	if product == "" || script == "" {
		items = append(items, precheckBad("script", "当前脚本", "没有选中脚本",
			"先在左侧脚本列表里点开一个脚本，再点导出", true))
	} else if dir, err := a.scriptDir(product, script, true); err != nil {
		items = append(items, precheckBad("script", "当前脚本", err.Error(),
			"重新在左侧选一个存在的脚本；如果脚本刚被删除，刷新页面", true))
	} else if d, rerr := func() (Doc, error) {
		if room := a.existingRoom(dir); room != nil {
			return room.docNow(), nil
		}
		return readDoc(filepath.Join(dir, docFileName), product, script)
	}(); rerr != nil {
		items = append(items, precheckBad("script", "当前脚本", "读取文案.json 失败："+rerr.Error(),
			"到 NAS 上检查该脚本目录与文案.json 是否可读", true))
	} else if len(d.Shots) == 0 {
		scriptDir = dir
		doc = d
		items = append(items, precheckBad("script", "当前脚本", "脚本里还没有镜头",
			"先在表格里添加镜头并填写口播 / 文案，再导出", true))
	} else {
		scriptDir = dir
		doc = d
		scriptUsable = true
		items = append(items, precheckOK("script", "当前脚本",
			product+" / "+script+"，共 "+strconv.Itoa(len(doc.Shots))+" 个镜头"))
	}

	// 6. 素材（提醒项，不阻止导出：没有素材时可以只导出文案轨道）
	if scriptUsable {
		hasVideo := 0
		missing := make([]string, 0, 5)
		for _, shot := range doc.Shots {
			if shot.Video == nil || strings.TrimSpace(shot.Video.File) == "" {
				continue
			}
			abs := filepath.Join(scriptDir, shot.Video.File)
			if shot.Video.Src == "temp" {
				abs = filepath.Join(a.dataDir, tempDirName, shot.Video.File)
			}
			if _, err := os.Stat(abs); err != nil {
				if len(missing) < 5 {
					missing = append(missing, "镜头 "+shot.No)
				}
				continue
			}
			hasVideo++
		}
		switch {
		case hasVideo == 0 && len(missing) == 0:
			items = append(items, precheckBad("materials", "视频素材", "当前脚本还没有视频素材，导出后只有文案轨道",
				"在镜头里上传视频，或先用剪辑面板切片后再导出", false))
		case len(missing) > 0:
			detail := "素材文件找不到：" + strings.Join(missing, "、")
			if hasVideo > 0 {
				detail += "（可用 " + strconv.Itoa(hasVideo) + " 段，其余会被跳过）"
			}
			items = append(items, precheckBad("materials", "视频素材", detail,
				"重新上传这些镜头的视频，或把它们从镜头里移除", false))
		default:
			items = append(items, precheckOK("materials", "视频素材", "可用素材 "+strconv.Itoa(hasVideo)+" 段"))
		}
	}

	blocking := 0
	problems := 0
	for _, it := range items {
		if it.OK {
			continue
		}
		problems++
		if it.Blocking {
			blocking++
		}
	}
	res.Items = items
	res.OK = blocking == 0
	switch {
	case blocking > 0:
		res.Summary = fmt.Sprintf("预检未通过：%d 个必须解决的问题、%d 个提醒。按每条的「怎么补」处理后再导出。", blocking, problems-blocking)
	case problems > 0:
		res.Summary = fmt.Sprintf("预检通过，可以导出；有 %d 个提醒（不影响导出）。", problems)
	default:
		res.Summary = "预检通过，可以导出。"
	}
	writeJSON(w, http.StatusOK, res)
}
