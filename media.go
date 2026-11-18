package main

// 视频文件清理：工具里删掉的视频，磁盘上也要跟着删，避免数据目录越积越多。
//
// 两条路径：
//   1. 联动删除：删除镜头 / 清空视频 / 替换视频 / 更换剪辑原片后，对比改动前后的
//      视频引用，把「本次不再被引用」的路径拿去全库查一遍（其它脚本的 文案.json、
//      在线房间的内存状态都算），确认零引用才删文件。
//   2. 孤儿清理：扫描 _剪辑素材 与各产品/脚本目录里的视频，列出没有被任何脚本
//      引用的文件，默认只报告；用户勾选并二次确认后才删除。
//
// 安全边界：
//   - 只删除数据目录内、「_剪辑素材」或「产品/脚本」这一层里的普通视频文件；
//   - 绝不删除剪映草稿目录（/drafts 或设置里的草稿根目录）下的任何文件；
//   - 删除失败（占用、权限）只记日志并回传，不阻断主流程；
//   - 刚上传不久的文件不算孤儿，避免误删正在拖拽中的素材。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	cleanupLogName  = "_清理记录.json"
	cleanupLogLimit = 50
	// 上传后 10 分钟内的文件先不当作孤儿：剪辑面板里可能刚导入、还没拖到镜头。
	minOrphanAge = 10 * time.Minute
	// 上传 / 原子写入被打断留下的临时文件，超过这个时间才算残留。
	residueAge = 2 * time.Hour
	// 全库引用索引的最长有效期：正常靠写入即时失效，只兜住手工改 文案.json 的情况。
	mediaIndexTTL = 60 * time.Second
	// 联动清理的最小间隔：编辑再密集，全库扫描也只这么多秒跑一次。
	mediaScanInterval = 4 * time.Second
)

// mediaCandidate 是一条待清理候选：路径 + 是哪份脚本、谁改动的（写历史用）。
type mediaCandidate struct {
	path string
	dir  string
	by   string
	size int64
}

// cleanupLogEntry 是全局清理日志的一条记录（_清理记录.json）。
type cleanupLogEntry struct {
	ID    string   `json:"id"`
	At    string   `json:"at"`
	By    string   `json:"by"`
	Kind  string   `json:"kind"` // linked（联动删除）/ orphan（孤儿清理）
	Text  string   `json:"text"`
	Files []string `json:"files,omitempty"`
	Freed int64    `json:"freed"`
}

type cleanupLogFile struct {
	Version int               `json:"version"`
	Entries []cleanupLogEntry `json:"entries"`
}

// orphanFile 是扫描结果里的一个未被引用的视频。
type orphanFile struct {
	Path     string `json:"path"` // 相对数据目录，统一用 /
	Name     string `json:"name"`
	Scope    string `json:"scope"` // temp / script
	Product  string `json:"product,omitempty"`
	Script   string `json:"script,omitempty"`
	Size     int64  `json:"size"`
	Modified string `json:"modified,omitempty"`
}

type deletedMedia struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type failedMedia struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

/* ---------------- 路径与引用 ---------------- */

func isVideoFileName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, ".tmp-") || strings.HasPrefix(name, ".upload-") {
		return false
	}
	return videoExts[strings.ToLower(filepath.Ext(name))]
}

// isTempResidue 判断是否是上传（.upload-*）或原子写入（.tmp-*）中途被打断留下的临时文件。
func isTempResidue(name string) bool {
	return strings.HasPrefix(name, ".upload-") || strings.HasPrefix(name, ".tmp-")
}

// videoRefPath 把一个视频引用换算成绝对路径；引用无效时返回空串。
func (a *App) videoRefPath(dir string, v *VideoRef) string {
	if v == nil || v.File == "" {
		return ""
	}
	var p string
	if v.Src == "temp" {
		p = a.safeTempFile(v.File)
	} else {
		p = safeScriptFile(dir, v.File)
	}
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// safeScriptFile 拼出脚本目录下的直接子文件；不接受带路径分隔符的名字。
func safeScriptFile(dir, file string) string {
	file = strings.TrimSpace(file)
	if file == "" || file == "." || file == ".." || strings.ContainsAny(file, `/\`) {
		return ""
	}
	return filepath.Join(dir, file)
}

func (a *App) safeTempFile(file string) string {
	file = strings.TrimSpace(file)
	if file == "" || file == "." || file == ".." || strings.ContainsAny(file, `/\`) {
		return ""
	}
	return filepath.Join(a.dataDir, tempDirName, file)
}

// docRefPathList 列出这篇文档里的每一处视频引用（同一文件被多处引用会出现多次）。
func (a *App) docRefPathList(dir string, doc Doc) []string {
	out := make([]string, 0, len(doc.Shots))
	for _, shot := range doc.Shots {
		if p := a.videoRefPath(dir, shot.Video); p != "" {
			out = append(out, p)
		}
	}
	if doc.Edit != nil && doc.Edit.Source != "" {
		if p := a.videoRefPath(dir, &VideoRef{Src: "temp", File: doc.Edit.Source}); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (a *App) docRefPaths(dir string, doc Doc) map[string]bool {
	list := a.docRefPathList(dir, doc)
	out := make(map[string]bool, len(list))
	for _, p := range list {
		out[p] = true
	}
	return out
}

// eachScriptDoc 遍历数据目录里的每份脚本；正在协作的房间以内存状态为准，
// 避免落盘延迟导致刚移除的引用又被当成「仍被引用」。
func (a *App) eachScriptDoc(fn func(dir string, doc Doc)) {
	entries, err := os.ReadDir(a.dataDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.EqualFold(name, "bin") || strings.EqualFold(name, "drafts") {
			continue
		}
		productDir := filepath.Join(a.dataDir, name)
		scripts, err := os.ReadDir(productDir)
		if err != nil {
			continue
		}
		for _, sc := range scripts {
			sname := sc.Name()
			if !sc.IsDir() || strings.HasPrefix(sname, "_") || strings.HasPrefix(sname, ".") {
				continue
			}
			if strings.EqualFold(sname, "drafts") {
				continue
			}
			dir := filepath.Join(productDir, sname)
			var doc Doc
			if room := a.existingRoom(dir); room != nil {
				doc = room.docNow()
			} else {
				doc, err = readDoc(filepath.Join(dir, docFileName), name, sname)
				if err != nil {
					continue
				}
			}
			fn(dir, doc)
		}
	}
}

// mediaRefCounts 汇总全库的视频引用计数（绝对路径 -> 被引用处数）。
// 结果带一份短缓存：任何一次落盘写入都会让它失效，手工改文件的情况靠 TTL 兜底。
// 调用方拿到的是副本，可以随意读。
func (a *App) mediaRefCounts() map[string]int {
	a.mediaIdxMu.Lock()
	defer a.mediaIdxMu.Unlock()
	if a.mediaIdx == nil || time.Since(a.mediaIdxAt) >= mediaIndexTTL {
		idx := make(map[string]int)
		a.eachScriptDoc(func(dir string, doc Doc) {
			for _, p := range a.docRefPathList(dir, doc) {
				idx[p]++
			}
		})
		a.mediaIdx = idx
		a.mediaIdxAt = time.Now()
	}
	out := make(map[string]int, len(a.mediaIdx))
	for k, v := range a.mediaIdx {
		out[k] = v
	}
	return out
}

// invalidateMediaIndex 让引用索引作废：任何改动文档、增删脚本、删除文件的地方都要调。
func (a *App) invalidateMediaIndex() {
	a.mediaIdxMu.Lock()
	a.mediaIdx = nil
	a.mediaIdxMu.Unlock()
}

// countMediaRefs 统计某个文件在全库被引用的次数（按镜头 + 剪辑原片逐处计数）。
func (a *App) countMediaRefs(path string) int {
	return a.mediaRefCounts()[filepath.Clean(path)]
}

// scriptRefsTemp 判断某份脚本当前是否还引用着这个 _剪辑素材 文件。
func (a *App) scriptRefsTemp(dir, file string) bool {
	target := a.safeTempFile(file)
	if target == "" {
		return false
	}
	var doc Doc
	if room := a.existingRoom(dir); room != nil {
		doc = room.docNow()
	} else {
		var err error
		doc, err = readDoc(filepath.Join(dir, docFileName), filepath.Base(filepath.Dir(dir)), filepath.Base(dir))
		if err != nil {
			return false
		}
	}
	_, ok := a.docRefPaths(dir, doc)[filepath.Clean(target)]
	return ok
}

/*
deletableMediaPath 校验一个绝对路径能不能删：

	数据目录/_剪辑素材/文件
	数据目录/产品/脚本/文件

不允许更深层级、不允许符号链接、不允许草稿目录。
*/
func (a *App) deletableMediaPath(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	base, err := filepath.Abs(a.dataDir)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	if a.isUnderDrafts(abs) {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return "", false
	}
	name := parts[len(parts)-1]
	if !isVideoFileName(name) && !isTempResidue(name) {
		return "", false
	}
	if len(parts) == 2 && parts[0] == tempDirName {
		return filepath.ToSlash(rel), true
	}
	if len(parts) == 3 {
		if strings.HasPrefix(parts[0], "_") || strings.HasPrefix(parts[0], ".") ||
			strings.HasPrefix(parts[1], "_") || strings.HasPrefix(parts[1], ".") {
			return "", false
		}
		if strings.EqualFold(parts[0], "drafts") || strings.EqualFold(parts[1], "drafts") {
			return "", false
		}
		return filepath.ToSlash(rel), true
	}
	return "", false
}

// isUnderDrafts 判断路径是否落在剪映草稿目录里（绝对不允许删除）。
func (a *App) isUnderDrafts(path string) bool {
	slashed := filepath.ToSlash(path)
	if slashed == "/drafts" || strings.HasPrefix(slashed, "/drafts/") {
		return true
	}
	root, _, _ := a.jianyingConfig()
	root = strings.TrimSpace(root)
	if root == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// containsDrafts 判断 dir 是否把草稿目录包在里面：删除整个目录会连带删掉草稿，
// 这种情况必须拒绝（草稿根目录理论上不该放在产品 / 脚本目录下）。
func (a *App) containsDrafts(dir string) bool {
	if filepath.ToSlash(dir) == "/" {
		return true
	}
	root, _, _ := a.jianyingConfig()
	root = strings.TrimSpace(root)
	if root == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), absRoot)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// removeMediaFile 只删普通文件；符号链接一律跳过（避免顺着链接删到别处）。
func removeMediaFile(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return 0, fmt.Errorf("是目录，不处理")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("是符号链接，不处理")
	}
	if err := os.Remove(path); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n) / 1024
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func mediaHistoryText(prefix string, files []string, freed int64) string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, filepath.Base(filepath.FromSlash(f)))
		if len(names) >= 8 {
			names = append(names, "…")
			break
		}
	}
	text := fmt.Sprintf("%s %d 个，释放 %s", prefix, len(files), humanBytes(freed))
	if len(names) > 0 {
		text += "：" + strings.Join(names, "、")
	}
	return truncText(text, 160)
}

func newMediaHistoryEntry(by, text string) historyEntry {
	now := time.Now().UnixMilli()
	return historyEntry{
		ID:   newID(),
		T:    now,
		At:   time.UnixMilli(now).Format(time.RFC3339),
		By:   by,
		Kind: "media",
		Text: text,
	}
}

// appendMediaHistory 在没有在线房间时直接改 _历史.json；有房间时走 appendHistory。
func (a *App) appendMediaHistory(dir, by, text string) {
	entry := newMediaHistoryEntry(by, text)
	if room := a.existingRoom(dir); room != nil {
		room.appendHistory([]historyEntry{entry})
		room.saveNow() // 清理记录立即落盘，避免进程很快退出时丢失
		return
	}
	path := historyPath(dir)
	list := append(readHistoryFile(path), entry)
	_ = writeHistoryFile(path, trimHistory(list))
}

/* ---------------- 全局清理日志 ---------------- */

func (a *App) cleanupLogPath() string {
	return filepath.Join(a.dataDir, cleanupLogName)
}

func (a *App) readCleanupLog() []cleanupLogEntry {
	b, err := os.ReadFile(a.cleanupLogPath())
	if err != nil {
		return nil
	}
	var f cleanupLogFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return f.Entries
}

func (a *App) writeCleanupLog(entry cleanupLogEntry) {
	list := append(a.readCleanupLog(), entry)
	if len(list) > cleanupLogLimit {
		list = list[len(list)-cleanupLogLimit:]
	}
	f := cleanupLogFile{Version: 1, Entries: list}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(a.cleanupLogPath(), b); err != nil {
		log.Printf("写清理记录失败: %v", err)
	}
}

func (a *App) recentCleanupLog(limit int) []cleanupLogEntry {
	list := a.readCleanupLog()
	if len(list) == 0 {
		return []cleanupLogEntry{}
	}
	if len(list) > limit {
		list = list[len(list)-limit:]
	}
	out := make([]cleanupLogEntry, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i])
	}
	return out
}

/* ---------------- 联动删除（异步队列） ---------------- */

// lostMediaPaths 对比一次整篇改动的视频引用，返回这次不再被引用的路径。
func (a *App) lostMediaPaths(dir string, before, after Doc) []string {
	beforeRefs := a.docRefPaths(dir, before)
	afterRefs := a.docRefPaths(dir, after)
	var lost []string
	for p := range beforeRefs {
		if !afterRefs[p] {
			lost = append(lost, p)
		}
	}
	return lost
}

// cleanupMediaAfterChange 把整篇保存（旧接口）后失去引用的路径丢进异步清理队列。
// 房间状态本身就在内存里，核对引用时会读到，不需要先落盘。
func (a *App) cleanupMediaAfterChange(dir string, room *Room, before, after Doc, by string) {
	lost := a.lostMediaPaths(dir, before, after)
	if len(lost) == 0 {
		return
	}
	a.enqueueMediaCleanup(dir, lost, by)
}

// enqueueMediaCleanup 登记待清理候选。真正的删除交给 runMediaCleaner 批量做，
// 这样密集编辑时全库扫描最多每 mediaScanInterval 秒跑一次，不跟着按键次数线性增长。
func (a *App) enqueueMediaCleanup(dir string, paths []string, by string) {
	if len(paths) == 0 {
		return
	}
	a.mediaQueueMu.Lock()
	if a.mediaQueue == nil {
		a.mediaQueue = make(map[string]*mediaCandidate)
	}
	for _, p := range paths {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || p == "." {
			continue
		}
		if _, ok := a.mediaQueue[p]; !ok {
			a.mediaQueue[p] = &mediaCandidate{path: p, dir: dir, by: by}
		}
	}
	a.mediaQueueMu.Unlock()
}

// runMediaCleaner 是后台清理循环；进程退出前还会再 drain 一次。
func (a *App) runMediaCleaner() {
	ticker := time.NewTicker(mediaScanInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.drainMediaQueue()
	}
}

func (a *App) drainMediaQueue() {
	a.mediaQueueMu.Lock()
	batch := a.mediaQueue
	a.mediaQueue = nil
	a.mediaQueueMu.Unlock()
	a.cleanupLostMedia(batch)
}

// cleanupLostMedia 对一批候选做一次全库引用核对，零引用的才删除。
func (a *App) cleanupLostMedia(cands map[string]*mediaCandidate) {
	if len(cands) == 0 {
		return
	}
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()

	// 删除是不可逆的：这里强制重建一次索引，确保核对的是最新引用而不是缓存。
	a.invalidateMediaIndex()
	refs := a.mediaRefCounts()
	deleted := make([]string, 0, len(cands))
	byDir := make(map[string][]string)
	whoByDir := make(map[string]string)
	freedByDir := make(map[string]int64)
	var freed int64

	for path, cand := range cands {
		if refs[path] > 0 {
			continue
		}
		rel, ok := a.deletableMediaPath(path)
		if !ok {
			continue
		}
		size, err := removeMediaFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("删除素材失败（已跳过）: %s: %v", path, err)
			}
			continue
		}
		a.invalidateMediaIndex()
		deleted = append(deleted, rel)
		freed += size
		if cand.dir != "" {
			byDir[cand.dir] = append(byDir[cand.dir], rel)
			freedByDir[cand.dir] += size
			whoByDir[cand.dir] = cand.by
		}
	}
	if len(deleted) == 0 {
		return
	}
	sort.Strings(deleted)
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		files := byDir[dir]
		sort.Strings(files)
		a.appendMediaHistory(dir, whoByDir[dir], mediaHistoryText("清理已无引用的素材", files, freedByDir[dir]))
	}
	by := ""
	if len(dirs) > 0 {
		by = whoByDir[dirs[0]]
	}
	a.writeCleanupLog(cleanupLogEntry{
		ID:    newID(),
		At:    time.Now().Format(time.RFC3339),
		By:    by,
		Kind:  "linked",
		Text:  mediaHistoryText("清理已无引用的素材", deleted, freed),
		Files: deleted,
		Freed: freed,
	})
}

/* ---------------- 孤儿扫描 / 删除 ---------------- */

// orphanCandidate 组装一条可清理项；residue 为 true 表示上传 / 原子写入留下的临时文件。
func (a *App) orphanCandidate(path, product, script string, residue bool) (orphanFile, bool) {
	rel, ok := a.deletableMediaPath(path)
	if !ok {
		return orphanFile{}, false
	}
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return orphanFile{}, false
	}
	if !strings.Contains(rel, "/") {
		return orphanFile{}, false
	}
	scope := "script"
	if residue {
		scope = "residue"
	}
	if strings.HasPrefix(rel, tempDirName+"/") {
		product, script = "", ""
		if !residue {
			scope = "temp"
		}
	}
	return orphanFile{
		Path:     rel,
		Name:     filepath.Base(filepath.FromSlash(rel)),
		Scope:    scope,
		Product:  product,
		Script:   script,
		Size:     info.Size(),
		Modified: info.ModTime().Format(time.RFC3339),
	}, true
}

// scanOrphans 找出没有被任何脚本引用的视频文件，以及遗留的上传临时文件。只读，不删。
func (a *App) scanOrphans() ([]orphanFile, int64, int, error) {
	refs := a.mediaRefCounts()
	files := make([]orphanFile, 0)
	var total int64
	recent := 0
	now := time.Now()

	check := func(path, product, script string, residue bool) {
		item, ok := a.orphanCandidate(path, product, script, residue)
		if !ok || refs[filepath.Clean(path)] > 0 {
			return
		}
		info, err := os.Lstat(path)
		if err != nil {
			return
		}
		age := now.Sub(info.ModTime())
		if residue {
			if age < residueAge {
				return // 可能正在写入，先不动
			}
		} else if age < minOrphanAge {
			recent++
			return
		}
		files = append(files, item)
		total += item.Size
	}

	tempDir := filepath.Join(a.dataDir, tempDirName)
	if entries, err := os.ReadDir(tempDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			residue := isTempResidue(name)
			if !residue && !isVideoFileName(name) {
				continue
			}
			check(filepath.Join(tempDir, name), "", "", residue)
		}
	}

	products, err := os.ReadDir(a.dataDir)
	if err != nil {
		return nil, 0, 0, err
	}
	for _, entry := range products {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.EqualFold(name, "bin") || strings.EqualFold(name, "drafts") {
			continue
		}
		productDir := filepath.Join(a.dataDir, name)
		scripts, err := os.ReadDir(productDir)
		if err != nil {
			continue
		}
		for _, sc := range scripts {
			sname := sc.Name()
			if !sc.IsDir() || strings.HasPrefix(sname, "_") || strings.HasPrefix(sname, ".") {
				continue
			}
			if strings.EqualFold(sname, "drafts") {
				continue
			}
			dir := filepath.Join(productDir, sname)
			filesInDir, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, f := range filesInDir {
				if f.IsDir() {
					continue
				}
				fname := f.Name()
				residue := isTempResidue(fname)
				if !residue && !isVideoFileName(fname) {
					continue
				}
				check(filepath.Join(dir, fname), name, sname, residue)
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, total, recent, nil
}

func (a *App) handleOrphanScan(w http.ResponseWriter, r *http.Request) {
	files, total, recent, err := a.scanOrphans()
	if err != nil {
		fail(w, http.StatusInternalServerError, "扫描数据目录失败: "+err.Error())
		return
	}
	root, _, _ := a.jianyingConfig()
	residue := 0
	for _, f := range files {
		if f.Scope == "residue" {
			residue++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files":          files,
		"count":          len(files),
		"totalSize":      total,
		"recentCount":    recent,
		"residueCount":   residue,
		"minAgeMinutes":  int(minOrphanAge / time.Minute),
		"residueHours":   int(residueAge / time.Hour),
		"draftRoot":      root,
		"draftsExcluded": true,
		"recent":         a.recentCleanupLog(5),
		"hint":           "默认只报告、不删除。删除后无法恢复；回滚到旧版本也不会恢复已删除的视频文件。剪映草稿目录不会被扫描或删除。带 .upload- / .tmp- 前缀的是上传或写入中断留下的临时文件。",
	})
}

func (a *App) handleOrphanDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths   []string `json:"paths"`
		Confirm bool     `json:"confirm"`
		Name    string   `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !req.Confirm {
		fail(w, http.StatusBadRequest, "删除前需要二次确认")
		return
	}
	if len(req.Paths) == 0 {
		fail(w, http.StatusBadRequest, "没有选中任何文件")
		return
	}
	if len(req.Paths) > 5000 {
		fail(w, http.StatusBadRequest, "一次最多删除 5000 个文件，请分批操作")
		return
	}
	by := cleanLabel(req.Name)
	if by == "" {
		by = "同事"
	}

	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()

	a.invalidateMediaIndex() // 删除前按最新引用核对
	refs := a.mediaRefCounts()
	deleted := make([]deletedMedia, 0, len(req.Paths))
	failed := make([]failedMedia, 0)
	skipped := make([]failedMedia, 0)
	byScript := make(map[string][]string)
	var freed int64
	now := time.Now()

	for _, raw := range req.Paths {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		path := filepath.FromSlash(raw)
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.dataDir, path)
		}
		path = filepath.Clean(path)
		rel, ok := a.deletableMediaPath(path)
		if !ok {
			skipped = append(skipped, failedMedia{Path: raw, Error: "不在允许清理的范围内（草稿目录 / 非视频 / 目录层级不对）"})
			continue
		}
		if refs[path] > 0 {
			skipped = append(skipped, failedMedia{Path: rel, Error: "仍被脚本引用，已跳过"})
			continue
		}
		// 扫描到删除之间可能又上传了新素材：10 分钟内动过的视频文件一律不删。
		if isVideoFileName(filepath.Base(path)) {
			if info, err := os.Lstat(path); err == nil && now.Sub(info.ModTime()) < minOrphanAge {
				skipped = append(skipped, failedMedia{Path: rel, Error: "上传不到 10 分钟，可能正在使用，已跳过"})
				continue
			}
		}
		size, err := removeMediaFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				skipped = append(skipped, failedMedia{Path: rel, Error: "文件已不存在"})
				continue
			}
			log.Printf("清理孤儿素材失败: %s: %v", path, err)
			failed = append(failed, failedMedia{Path: rel, Error: err.Error()})
			continue
		}
		deleted = append(deleted, deletedMedia{Path: rel, Size: size})
		freed += size
		if parts := strings.Split(rel, "/"); len(parts) == 3 && parts[0] != tempDirName {
			key := parts[0] + "/" + parts[1]
			byScript[key] = append(byScript[key], rel)
		}
	}
	if len(deleted) > 0 {
		a.invalidateMediaIndex()
	}

	sort.Slice(deleted, func(i, j int) bool { return deleted[i].Path < deleted[j].Path })
	paths := make([]string, 0, len(deleted))
	for _, d := range deleted {
		paths = append(paths, d.Path)
	}
	if len(deleted) > 0 {
		text := mediaHistoryText("清理未引用视频", paths, freed)
		a.writeCleanupLog(cleanupLogEntry{
			ID:    newID(),
			At:    time.Now().Format(time.RFC3339),
			By:    by,
			Kind:  "orphan",
			Text:  text,
			Files: paths,
			Freed: freed,
		})
		// 按脚本分别记进修改历史，便于回溯
		keys := make([]string, 0, len(byScript))
		for key := range byScript {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts := strings.SplitN(key, "/", 2)
			dir := filepath.Join(a.dataDir, parts[0], parts[1])
			a.appendMediaHistory(dir, by, mediaHistoryText("清理未引用视频", byScript[key], 0))
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"deleted": deleted,
		"failed":  failed,
		"skipped": skipped,
		"count":   len(deleted),
		"freed":   freed,
		"message": fmt.Sprintf("已删除 %d 个文件，释放 %s", len(deleted), humanBytes(freed)),
	})
}

/* ---------------- 删除产品 / 脚本目录 ---------------- */

type dirStat struct {
	Files  int
	Videos int
	Size   int64
}

func scanDirStats(root string) dirStat {
	var st dirStat
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		st.Files++
		st.Size += info.Size()
		if isVideoFileName(info.Name()) {
			st.Videos++
		}
		return nil
	})
	return st
}

// removeDirSafely 先重命名到同级的隐藏临时名，再递归删除。
// 删除失败时把临时目录改回原名，避免留下半删状态。
func removeDirSafely(dir string) error {
	parent := filepath.Dir(dir)
	tmp := filepath.Join(parent, "."+filepath.Base(dir)+".deleting-"+newID())
	if err := os.Rename(dir, tmp); err != nil {
		return fmt.Errorf("无法重命名目录（可能被占用或没有权限）: %w", err)
	}
	if err := os.RemoveAll(tmp); err != nil {
		if rbErr := os.Rename(tmp, dir); rbErr != nil {
			return fmt.Errorf("删除失败: %v；回滚重命名也失败: %v（请手工检查 %s）", err, rbErr, tmp)
		}
		return fmt.Errorf("删除失败（已回滚，目录保持不变）: %w", err)
	}
	return nil
}

func (a *App) scriptDirsOfProduct(product string) []string {
	root := filepath.Join(a.dataDir, product)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, filepath.Join(root, name))
	}
	return out
}

// tempRefsForDirs 收集这些脚本目录下的所有 _剪辑素材 引用（镜头 + 剪辑原片）。
func (a *App) tempRefsForDirs(dirs []string) map[string]bool {
	out := make(map[string]bool)
	tempDir := filepath.Join(a.dataDir, tempDirName)
	for _, dir := range dirs {
		var doc Doc
		if room := a.existingRoom(dir); room != nil {
			doc = room.docNow()
		} else {
			doc, _ = readDoc(filepath.Join(dir, docFileName), filepath.Base(filepath.Dir(dir)), filepath.Base(dir))
		}
		for p := range a.docRefPaths(dir, doc) {
			if filepath.Dir(p) == tempDir {
				out[p] = true
			}
		}
	}
	return out
}

func (a *App) handleDeletePrecheck(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	product, err := sanitizeName(query.Get("product"))
	if err != nil {
		fail(w, http.StatusBadRequest, "产品名无效: "+err.Error())
		return
	}
	typ := query.Get("type")
	var dir string
	script := ""
	switch typ {
	case "script":
		dir, err = a.scriptDir(product, query.Get("script"), true)
		if err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		script, _ = sanitizeName(query.Get("script"))
	case "product":
		if strings.EqualFold(product, "bin") || strings.EqualFold(product, "drafts") {
			fail(w, http.StatusBadRequest, "该目录是保留目录，不能删除")
			return
		}
		dir = filepath.Join(a.dataDir, product)
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			fail(w, http.StatusNotFound, "产品不存在")
			return
		}
	default:
		fail(w, http.StatusBadRequest, "type 只能是 product 或 script")
		return
	}
	if a.isUnderDrafts(dir) || a.containsDrafts(dir) {
		fail(w, http.StatusBadRequest, "不允许删除剪映草稿目录")
		return
	}
	st := scanDirStats(dir)
	writeJSON(w, http.StatusOK, map[string]any{
		"type":    typ,
		"product": product,
		"script":  script,
		"path":    dir,
		"files":   st.Files,
		"videos":  st.Videos,
		"size":    st.Size,
	})
}

// deleteScopeDir 删除一个脚本目录或整个产品目录；成功后再清理失去引用的共享素材。
func (a *App) deleteScopeDir(product, script, by string) (map[string]any, error) {
	dir := filepath.Join(a.dataDir, product)
	dirs := []string{}
	if script == "" {
		if strings.EqualFold(product, "bin") || strings.EqualFold(product, "drafts") {
			return nil, fmt.Errorf("该目录是保留目录，不能删除")
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("产品不存在")
		}
		dirs = a.scriptDirsOfProduct(product)
	} else {
		dir = filepath.Join(dir, script)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("脚本不存在")
		}
		dirs = []string{dir}
	}
	if a.isUnderDrafts(dir) || a.containsDrafts(dir) {
		return nil, fmt.Errorf("不允许删除剪映草稿目录")
	}

	st := scanDirStats(dir)
	tempRefs := a.tempRefsForDirs(dirs)
	if err := removeDirSafely(dir); err != nil {
		return nil, err
	}
	a.invalidateMediaIndex()

	// 目录已经删除，再通知在线协作端并移除房间（避免 saveNow 把目录写回来）
	for _, d := range dirs {
		name := filepath.Base(d)
		a.forgetRoom(d, map[string]any{"t": "gone", "product": product, "script": name})
	}
	if script == "" {
		a.dropProductStatus(product)
	} else {
		a.dropStatus(product, script)
	}

	a.mediaMu.Lock()
	refs := a.mediaRefCounts()
	deleted := make([]string, 0)
	kept := 0
	var tempFreed int64
	for p := range tempRefs {
		if refs[p] > 0 {
			kept++
			continue
		}
		rel, ok := a.deletableMediaPath(p)
		if !ok {
			continue
		}
		size, err := removeMediaFile(p)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("删除共享素材失败（已跳过）: %s: %v", p, err)
			}
			continue
		}
		deleted = append(deleted, rel)
		tempFreed += size
	}
	a.mediaMu.Unlock()

	sort.Strings(deleted)
	freed := st.Size + tempFreed
	what := "脚本「" + product + "/" + script + "」"
	if script == "" {
		what = "产品「" + product + "」"
	}
	text := fmt.Sprintf("删除%s，%d 个文件，释放 %s", what, st.Files, humanBytes(freed))
	if len(deleted) > 0 {
		text += fmt.Sprintf("；另清理共享素材 %d 个", len(deleted))
	}
	text = truncText(text, 160)
	kind := "script"
	if script == "" {
		kind = "product"
	}
	a.writeCleanupLog(cleanupLogEntry{
		ID:    newID(),
		At:    time.Now().Format(time.RFC3339),
		By:    by,
		Kind:  kind,
		Text:  text,
		Files: append([]string{filepath.ToSlash(strings.TrimPrefix(dir, a.dataDir+string(filepath.Separator)))}, deleted...),
		Freed: freed,
	})

	msg := fmt.Sprintf("已删除%s，释放 %s", what, humanBytes(freed))
	if len(deleted) > 0 {
		msg += fmt.Sprintf("（含 %d 个共享素材）", len(deleted))
	}
	return map[string]any{
		"ok":          true,
		"freed":       freed,
		"tempFreed":   tempFreed,
		"files":       st.Files,
		"videos":      st.Videos,
		"tempDeleted": deleted,
		"tempKept":    kept,
		"message":     msg,
	}, nil
}

/* ---------------- 更换剪辑原片后释放旧文件 ---------------- */

func (a *App) handleReleaseTemp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
		Script  string `json:"script"`
		File    string `json:"file"`
		Name    string `json:"name"`
		WaitMS  int    `json:"waitMs"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	by := cleanLabel(req.Name)
	if by == "" {
		by = "同事"
	}
	dir, err := a.scriptDir(req.Product, req.Script, true)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	file := strings.TrimSpace(req.File)
	path := a.safeTempFile(file)
	if path == "" {
		fail(w, http.StatusBadRequest, "原片名称无效")
		return
	}

	// 等前端把新的 edit.source 合并进来，最多等 5 秒
	wait := req.WaitMS
	if wait <= 0 || wait > 5000 {
		wait = 2500
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Millisecond)
	for a.scriptRefsTemp(dir, file) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}

	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()

	_, statErr := os.Lstat(path)
	if os.IsNotExist(statErr) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "deleted": false, "refs": 0, "freed": 0,
			"message": "旧原片已不存在，无需清理",
		})
		return
	}
	refs := a.countMediaRefs(path)
	if refs > 0 {
		text := fmt.Sprintf("更换剪辑原片，旧原片 %s 仍被 %d 处引用，已保留", file, refs)
		a.appendMediaHistory(dir, by, truncText(text, 160))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "deleted": false, "refs": refs, "freed": 0,
			"message": fmt.Sprintf("旧原片仍被 %d 处引用，已保留", refs),
		})
		return
	}
	rel, ok := a.deletableMediaPath(path)
	if !ok {
		fail(w, http.StatusBadRequest, "这个文件不在允许清理的范围内")
		return
	}
	size, err := removeMediaFile(path)
	if err != nil {
		fail(w, http.StatusInternalServerError, fmt.Sprintf("删除旧原片失败: %v", err))
		return
	}
	text := fmt.Sprintf("更换剪辑原片，清理旧原片 %s，释放 %s", file, humanBytes(size))
	a.appendMediaHistory(dir, by, truncText(text, 160))
	a.writeCleanupLog(cleanupLogEntry{
		ID: newID(), At: time.Now().Format(time.RFC3339),
		By: by, Kind: "source", Text: text,
		Files: []string{rel}, Freed: size,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "deleted": true, "refs": 0, "freed": size,
		"message": fmt.Sprintf("已删除旧原片，释放 %s", humanBytes(size)),
	})
}
