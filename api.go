package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	docFileName    = "文案.json"
	tempDirName    = "_剪辑素材"
	statusFileName = "_脚本状态.json"
	maxUpload      = int64(4) << 30 // 4GB
	maxDocBytes    = int64(8) << 20
)

var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".m4v": true, ".webm": true,
	".mkv": true, ".avi": true, ".mpg": true, ".mpeg": true, ".ts": true,
}

// VideoRef 是镜头绑定的视频：src=script 时指向脚本目录内的文件，src=temp 时指向剪辑素材。
type VideoRef struct {
	Src   string  `json:"src"`
	File  string  `json:"file"`
	In    float64 `json:"in"`
	Out   float64 `json:"out"`
	Label string  `json:"label,omitempty"`
}

type Shot struct {
	ID        string    `json:"id"`
	No        string    `json:"no"`
	Content   string    `json:"content"`
	Narration string    `json:"narration"`
	Copy      string    `json:"copy"`
	Video     *VideoRef `json:"video,omitempty"`
}

type Doc struct {
	Version   int        `json:"version"`
	Product   string     `json:"product"`
	Script    string     `json:"script"`
	Shots     []Shot     `json:"shots"`
	Edit      *EditState `json:"edit,omitempty"`
	UpdatedAt string     `json:"updatedAt,omitempty"`
}

// EditState 保存剪辑面板的状态：原片、打点、时间轴位置，随脚本一起落盘。
type EditState struct {
	Source      string    `json:"source,omitempty"`     // _剪辑素材 内的原片文件名
	SourceName  string    `json:"sourceName,omitempty"` // 原片原始名称，仅用于显示
	FPS         float64   `json:"fps,omitempty"`        // ffprobe 读到的实际帧率
	Duration    float64   `json:"duration,omitempty"`   // 原片时长
	Cuts        []float64 `json:"cuts,omitempty"`       // S 打点 / 智能裁切的切点
	CurrentTime float64   `json:"currentTime,omitempty"`
}

type Product struct {
	Name    string   `json:"name"`
	Scripts []string `json:"scripts"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// sanitizeName 校验产品名 / 脚本名 / 文件名，禁止路径穿越与 Windows 非法字符。
func sanitizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		return "", errors.New("名称不能为空")
	}
	if name == "." || name == ".." {
		return "", errors.New("名称无效")
	}
	if len([]rune(name)) > 100 {
		return "", errors.New("名称过长（最多 100 个字符）")
	}
	if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
		return "", errors.New("名称不能以 _ 或 . 开头")
	}
	for _, r := range name {
		if r < 32 {
			return "", errors.New("名称包含非法字符")
		}
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return "", errors.New("名称不能包含 \\ / : * ? \" < > |")
		}
	}
	return name, nil
}

func (a *App) scriptDir(product, script string, mustExist bool) (string, error) {
	p, err := sanitizeName(product)
	if err != nil {
		return "", fmt.Errorf("产品名无效: %w", err)
	}
	s, err := sanitizeName(script)
	if err != nil {
		return "", fmt.Errorf("脚本名无效: %w", err)
	}
	dir := filepath.Join(a.dataDir, p, s)
	info, err := os.Stat(dir)
	if err != nil {
		if mustExist {
			return "", errors.New("产品或脚本不存在")
		}
		return "", fmt.Errorf("目录不可用: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("产品或脚本不存在")
	}
	return dir, nil
}

func defaultDoc(product, script string) Doc {
	return Doc{
		Version: 1,
		Product: product,
		Script:  script,
		Shots: []Shot{{
			ID: newID(),
			No: "1",
		}},
	}
}

func readDoc(path, product, script string) (Doc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultDoc(product, script), nil
		}
		return Doc{}, err
	}
	var doc Doc
	if err := json.Unmarshal(b, &doc); err != nil {
		return Doc{}, fmt.Errorf("文案.json 解析失败: %w", err)
	}
	if doc.Shots == nil {
		doc.Shots = []Shot{}
	}
	return doc, nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// CreateTemp 固定 0600，再被 umask 裁一次可能变 000；显式改成 0664
	_ = tmp.Chmod(filePerm)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (a *App) handleTree(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(a.dataDir)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取数据目录失败")
		return
	}
	products := make([]Product, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// bin 是放置 ffmpeg / ffprobe 的保留目录，不作为产品展示
		if strings.EqualFold(entry.Name(), "bin") {
			continue
		}
		p := Product{Name: entry.Name(), Scripts: []string{}}
		scripts, err := os.ReadDir(filepath.Join(a.dataDir, entry.Name()))
		if err == nil {
			for _, s := range scripts {
				if s.IsDir() && !strings.HasPrefix(s.Name(), "_") && !strings.HasPrefix(s.Name(), ".") {
					p.Scripts = append(p.Scripts, s.Name())
				}
			}
		}
		sort.Strings(p.Scripts)
		products = append(products, p)
	}
	sort.Slice(products, func(i, j int) bool { return products[i].Name < products[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"products": products, "used": a.usedScripts()})
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return false
	}
	return true
}

func (a *App) handleCreateProduct(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name, err := sanitizeName(req.Name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.EqualFold(name, "bin") {
		fail(w, http.StatusBadRequest, "「bin」是保留名称（用于放置 ffmpeg），请换一个产品名")
		return
	}
	dir := filepath.Join(a.dataDir, name)
	if _, err := os.Stat(dir); err == nil {
		fail(w, http.StatusConflict, "同名产品已存在")
		return
	}
	if err := ensureDir(dir); err != nil {
		fail(w, http.StatusInternalServerError, "创建产品目录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (a *App) handleCreateScript(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
		Name    string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	product, err := sanitizeName(req.Product)
	if err != nil {
		fail(w, http.StatusBadRequest, "产品名无效")
		return
	}
	name, err := sanitizeName(req.Name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	productDir := filepath.Join(a.dataDir, product)
	if info, err := os.Stat(productDir); err != nil || !info.IsDir() {
		fail(w, http.StatusNotFound, "产品不存在")
		return
	}
	dir := filepath.Join(productDir, name)
	if _, err := os.Stat(dir); err == nil {
		fail(w, http.StatusConflict, "同名脚本已存在")
		return
	}
	if err := ensureDir(dir); err != nil {
		fail(w, http.StatusInternalServerError, "创建脚本目录失败")
		return
	}
	doc := defaultDoc(product, name)
	doc.UpdatedAt = time.Now().Format(time.RFC3339)
	b, _ := json.MarshalIndent(doc, "", "  ")
	if err := writeFileAtomic(filepath.Join(dir, docFileName), b); err != nil {
		fail(w, http.StatusInternalServerError, "写入文案.json 失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"product": product, "name": name})
}

func (a *App) handleGetDoc(w http.ResponseWriter, r *http.Request) {
	product := r.URL.Query().Get("product")
	script := r.URL.Query().Get("script")
	dir, err := a.scriptDir(product, script, true)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	// 协作房间的内存状态优先，先落盘再读，保证接口看到的是最新内容
	if room := a.existingRoom(dir); room != nil {
		room.saveNow()
	}
	doc, err := readDoc(filepath.Join(dir, docFileName), product, script)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	doc.Product = product
	doc.Script = script
	// 首次打开时把默认文案落盘，保证脚本目录里始终有 文案.json
	docPath := filepath.Join(dir, docFileName)
	if _, statErr := os.Stat(docPath); errors.Is(statErr, os.ErrNotExist) {
		doc.UpdatedAt = time.Now().Format(time.RFC3339)
		if b, mErr := json.MarshalIndent(doc, "", "  "); mErr == nil {
			_ = writeFileAtomic(docPath, b)
		}
	}
	writeJSON(w, http.StatusOK, doc)
}

func (a *App) handlePutDoc(w http.ResponseWriter, r *http.Request) {
	product := r.URL.Query().Get("product")
	script := r.URL.Query().Get("script")
	dir, err := a.scriptDir(product, script, true)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	var doc Doc
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		fail(w, http.StatusBadRequest, "文案数据格式错误")
		return
	}
	if doc.Shots == nil {
		doc.Shots = []Shot{}
	}
	for i := range doc.Shots {
		if doc.Shots[i].ID == "" {
			doc.Shots[i].ID = newID()
		}
	}
	doc.Version = 1
	doc.Product = product
	doc.Script = script
	doc.UpdatedAt = time.Now().Format(time.RFC3339)
	// 已经有人在线协作时，整体保存按「导入」处理并通知在线客户端重新拉取
	if room := a.existingRoom(dir); room != nil {
		room.replaceWithDoc(doc)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updatedAt": doc.UpdatedAt})
		return
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fail(w, http.StatusBadRequest, "文案数据无法序列化")
		return
	}
	if err := writeFileAtomic(filepath.Join(dir, docFileName), b); err != nil {
		fail(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updatedAt": doc.UpdatedAt})
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	product := r.URL.Query().Get("product")
	script := r.URL.Query().Get("script")
	shot := r.URL.Query().Get("shot")
	dir, err := a.scriptDir(product, script, true)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	file, filename, err := receiveUpload(w, r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(filename))
	base, err := sanitizeName(shot)
	if err != nil || base == "" {
		base = "clip"
	}
	name := base + ext
	dst := filepath.Join(dir, name)
	if err := saveUpload(file, dst); err != nil {
		fail(w, http.StatusInternalServerError, "保存视频失败: "+err.Error())
		return
	}
	for e := range videoExts {
		if e == ext {
			continue
		}
		_ = os.Remove(filepath.Join(dir, base+e))
	}
	writeJSON(w, http.StatusOK, map[string]string{"file": name})
}

func (a *App) handleTempUpload(w http.ResponseWriter, r *http.Request) {
	file, filename, err := receiveUpload(w, r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(filename))
	dir := filepath.Join(a.dataDir, tempDirName)
	if err := ensureDir(dir); err != nil {
		fail(w, http.StatusInternalServerError, "创建剪辑素材目录失败")
		return
	}
	name := fmt.Sprintf("%d-%s%s", time.Now().UnixMilli(), newID()[:6], ext)
	if err := saveUpload(file, filepath.Join(dir, name)); err != nil {
		fail(w, http.StatusInternalServerError, "保存视频失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"file": name})
}

func receiveUpload(w http.ResponseWriter, r *http.Request) (io.ReadCloser, string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return nil, "", errors.New("上传失败或文件超过 4GB")
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, "", errors.New("缺少视频文件")
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !videoExts[ext] {
		file.Close()
		return nil, "", errors.New("仅支持 mp4 / mov / m4v / webm / mkv / avi 等视频文件")
	}
	return file, header.Filename, nil
}

func saveUpload(src io.Reader, dst string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 上传的临时文件同样固定成 0664，重命名后素材在宿主机上才可读
	_ = tmp.Chmod(filePerm)
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (a *App) handleScriptMedia(w http.ResponseWriter, r *http.Request) {
	product := r.PathValue("product")
	script := r.PathValue("script")
	file := r.PathValue("file")
	dir, err := a.scriptDir(product, script, true)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.serveMediaFile(w, r, dir, file)
}

func (a *App) handleTempMedia(w http.ResponseWriter, r *http.Request) {
	a.serveMediaFile(w, r, filepath.Join(a.dataDir, tempDirName), r.PathValue("file"))
}

func (a *App) serveMediaFile(w http.ResponseWriter, r *http.Request, dir, file string) {
	name, err := sanitizeName(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	if !videoExts[ext] {
		http.NotFound(w, r)
		return
	}
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	// 切片会按镜头序号覆盖同名文件，必须让浏览器每次重新校验，避免刷新后播到旧片段
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeFile(w, r, path)
}

/* ---------------- 脚本使用状态（使用 / 未使用） ---------------- */

type scriptStatus struct {
	Used bool   `json:"used"`
	At   string `json:"at,omitempty"`
}

type statusFile struct {
	Version int                     `json:"version"`
	Scripts map[string]scriptStatus `json:"scripts"`
}

func readStatus(path string) statusFile {
	var st statusFile
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Scripts == nil {
		st.Scripts = map[string]scriptStatus{}
	}
	st.Version = 1
	return st
}

func statusKey(product, script string) string {
	return product + "/" + script
}

func (a *App) usedScripts() []string {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	out := make([]string, 0, len(a.status.Scripts))
	for key, v := range a.status.Scripts {
		if v.Used {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func (a *App) saveStatusLocked() error {
	a.status.Version = 1
	b, err := json.MarshalIndent(a.status, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.statusPath, b)
}

func (a *App) setScriptUsed(product, script string, used bool) error {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	key := statusKey(product, script)
	if used {
		a.status.Scripts[key] = scriptStatus{Used: true, At: time.Now().Format(time.RFC3339)}
	} else {
		delete(a.status.Scripts, key)
	}
	return a.saveStatusLocked()
}

// renameStatus / dropStatus 在脚本改名、删除后同步状态表，避免留下孤儿记录。
func (a *App) renameStatus(product, oldName, newName string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	oldKey := statusKey(product, oldName)
	v, ok := a.status.Scripts[oldKey]
	if !ok {
		return
	}
	delete(a.status.Scripts, oldKey)
	v.At = time.Now().Format(time.RFC3339)
	a.status.Scripts[statusKey(product, newName)] = v
	_ = a.saveStatusLocked()
}

func (a *App) dropStatus(product, script string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	key := statusKey(product, script)
	if _, ok := a.status.Scripts[key]; !ok {
		return
	}
	delete(a.status.Scripts, key)
	_ = a.saveStatusLocked()
}

func (a *App) handleSetUsage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
		Script  string `json:"script"`
		Used    bool   `json:"used"`
		Name    string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	product, err := sanitizeName(req.Product)
	if err != nil {
		fail(w, http.StatusBadRequest, "产品名无效")
		return
	}
	script, err := sanitizeName(req.Script)
	if err != nil {
		fail(w, http.StatusBadRequest, "脚本名无效")
		return
	}
	if _, err := a.scriptDir(product, script, true); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err := a.setScriptUsed(product, script, req.Used); err != nil {
		fail(w, http.StatusInternalServerError, "保存使用状态失败: "+err.Error())
		return
	}
	by := cleanLabel(req.Name)
	if by == "" {
		by = "同事"
	}
	a.noteStatusHistory(product, script, req.Used, by)
	// 让所有在线同事刷新左侧列表，使用状态实时一致
	a.broadcastAll(map[string]any{"t": "tree"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "used": req.Used})
}

/* ---------------- 脚本重命名 / 删除 ---------------- */

func (a *App) handleRenameScript(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
		Script  string `json:"script"`
		Name    string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	product, err := sanitizeName(req.Product)
	if err != nil {
		fail(w, http.StatusBadRequest, "产品名无效")
		return
	}
	oldName, err := sanitizeName(req.Script)
	if err != nil {
		fail(w, http.StatusBadRequest, "脚本名无效")
		return
	}
	newName, err := sanitizeName(req.Name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if newName == oldName {
		writeJSON(w, http.StatusOK, map[string]string{"product": product, "name": newName})
		return
	}
	productDir := filepath.Join(a.dataDir, product)
	oldDir := filepath.Join(productDir, oldName)
	newDir := filepath.Join(productDir, newName)
	if info, err := os.Stat(oldDir); err != nil || !info.IsDir() {
		fail(w, http.StatusNotFound, "脚本不存在")
		return
	}
	if _, err := os.Stat(newDir); err == nil {
		fail(w, http.StatusConflict, "同名脚本已存在")
		return
	}
	// 先把协作房间的状态落盘并摘掉，避免重命名后仍往旧目录写文件
	a.evictRoom(oldDir, map[string]any{"t": "moved", "product": product, "from": oldName, "to": newName})
	if err := os.Rename(oldDir, newDir); err != nil {
		fail(w, http.StatusInternalServerError, "重命名失败: "+err.Error())
		return
	}
	a.renameStatus(product, oldName, newName)
	a.broadcastAll(map[string]any{"t": "tree"})
	writeJSON(w, http.StatusOK, map[string]string{"product": product, "name": newName})
}

func (a *App) handleDeleteScript(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
		Script  string `json:"script"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	dir, err := a.scriptDir(req.Product, req.Script, true)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	product, _ := sanitizeName(req.Product)
	script, _ := sanitizeName(req.Script)
	a.evictRoom(dir, map[string]any{"t": "gone", "product": product, "script": script})
	if err := os.RemoveAll(dir); err != nil {
		fail(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	a.dropStatus(product, script)
	a.broadcastAll(map[string]any{"t": "tree"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ---------------- 全文检索（镜头内容 / 口播 / 文案） ---------------- */

type searchHit struct {
	Product string `json:"product"`
	Script  string `json:"script"`
	ShotID  string `json:"shotId"`
	No      string `json:"no"`
	Field   string `json:"field"`
	Snippet string `json:"snippet"`
}

func snippetAround(text, q string) string {
	idx := strings.Index(strings.ToLower(text), strings.ToLower(q))
	if idx < 0 {
		return text
	}
	runes := []rune(text)
	startRune := len([]rune(text[:idx]))
	before, after := 18, 70
	from := startRune - before
	if from < 0 {
		from = 0
	}
	to := startRune + len([]rune(q)) + after
	if to > len(runes) {
		to = len(runes)
	}
	out := string(runes[from:to])
	if from > 0 {
		out = "…" + out
	}
	if to < len(runes) {
		out += "…"
	}
	return out
}

// docForSearch 优先读协作房间里的最新内容，没有房间再读 文案.json。
func (a *App) docForSearch(dir, product, script string) (Doc, error) {
	if room := a.existingRoom(dir); room != nil {
		return room.docNow(), nil
	}
	return readDoc(filepath.Join(dir, docFileName), product, script)
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len([]rune(q)) > 60 {
		writeJSON(w, http.StatusOK, map[string]any{"q": q, "items": []searchHit{}, "truncated": false})
		return
	}
	onlyProduct := strings.TrimSpace(r.URL.Query().Get("product"))
	if onlyProduct == "__all" {
		onlyProduct = ""
	}
	if onlyProduct != "" {
		if p, err := sanitizeName(onlyProduct); err == nil {
			onlyProduct = p
		} else {
			onlyProduct = ""
		}
	}
	maxPerScript := 5
	maxTotal := 80
	hits := make([]searchHit, 0, 16)
	truncated := false

	products, err := os.ReadDir(a.dataDir)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取数据目录失败")
		return
	}
	for _, entry := range products {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if strings.EqualFold(entry.Name(), "bin") {
			continue
		}
		if onlyProduct != "" && entry.Name() != onlyProduct {
			continue
		}
		scripts, err := os.ReadDir(filepath.Join(a.dataDir, entry.Name()))
		if err != nil {
			continue
		}
		for _, s := range scripts {
			if !s.IsDir() || strings.HasPrefix(s.Name(), "_") || strings.HasPrefix(s.Name(), ".") {
				continue
			}
			dir := filepath.Join(a.dataDir, entry.Name(), s.Name())
			doc, err := a.docForSearch(dir, entry.Name(), s.Name())
			if err != nil {
				continue
			}
			count := 0
			for _, shot := range doc.Shots {
				fields := []struct{ name, text string }{
					{"content", shot.Content},
					{"narration", shot.Narration},
					{"copy", shot.Copy},
				}
				for _, f := range fields {
					if f.text == "" || !strings.Contains(strings.ToLower(f.text), strings.ToLower(q)) {
						continue
					}
					hits = append(hits, searchHit{
						Product: entry.Name(),
						Script:  s.Name(),
						ShotID:  shot.ID,
						No:      shot.No,
						Field:   f.name,
						Snippet: snippetAround(f.text, q),
					})
					count++
					break // 同一镜头命中多个字段时只报一条，避免刷屏
				}
				if count >= maxPerScript {
					break
				}
			}
			if len(hits) >= maxTotal {
				truncated = true
				break
			}
		}
		if truncated {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"q": q, "items": hits, "truncated": truncated})
}
