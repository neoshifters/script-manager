package main

// 阶段三：多人同时编辑同一份脚本。
//
// 设计要点（详见 部署说明.md）：
//   - 文档被拆成一堆「字段寄存器」，键形如 s|<镜头ID>|content、e|cuts。
//     每个寄存器带 (t, s) 版本号：t 是混合逻辑时钟(毫秒)，s 是编辑者标识。
//     合并规则是「后写者胜」——t 大者胜，t 相同则 s 大者胜，仍相同则比值的 JSON。
//     这个合并是交换、结合、幂等的，所以不同镜头、同一行不同列永远能合并成功。
//   - 服务端只做合并、广播与落盘，不做加锁式的「整篇保存」。
//   - 协同.json 是协作状态的唯一权威，文案.json 是它的物化导出（供其它工具读取）。
//     server 启动时若发现 文案.json 比 协同.json 新 2 秒以上，认定被手工改过，按 文案.json 重建。
//   - 传输：WebSocket（/api/collab/ws）优先；被代理拦住时自动退回 HTTP 轮询（/api/collab）。

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	collabFileName = "协同.json"

	maxEditors     = 8 // 建议同一脚本同时在线人数
	wsPingEvery    = 20 * time.Second
	wsReadWindow   = 80 * time.Second
	saveDebounce   = 500 * time.Millisecond
	saveMaxDelay   = 5 * time.Second // 连续编辑时落盘最长推迟这么久，避免一直不写盘
	maxWSFrame     = 4 << 20
	maxRegsPerMsg  = 4000
	maxRegKeyLen   = 200
	maxRegValueLen = 512 << 10
	maxCollabBody  = 16 << 20
	importSlack    = 2 * time.Second
)

var (
	errFrameTooLarge = errors.New("WebSocket 帧超过上限")
	errNotUpgrade    = errors.New("不是 WebSocket 升级请求")
)

// Reg 是一个字段寄存器：t=版本毫秒，s=编辑者，v=值。
type Reg struct {
	T int64           `json:"t"`
	S string          `json:"s"`
	V json.RawMessage `json:"v"`
}

func (r Reg) newerThan(other Reg) bool {
	if r.T != other.T {
		return r.T > other.T
	}
	if r.S != other.S {
		return r.S > other.S
	}
	return string(r.V) > string(other.V)
}

type collabFile struct {
	Version int            `json:"version"`
	Epoch   string         `json:"epoch"`
	SavedAt string         `json:"savedAt,omitempty"`
	Regs    map[string]Reg `json:"regs"`
}

type peerInfo struct {
	Site  string `json:"site"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	Cell  string `json:"cell,omitempty"`
}

// Room 是一份脚本的协作房间。
type Room struct {
	app     *App
	product string
	script  string
	dir     string

	mu         sync.Mutex
	st         collabFile
	clients    map[*wsClient]struct{}
	saveTimer  *time.Timer
	savedAt    string
	hist       []historyEntry
	dead       bool      // 目录已被删除：任何迟到的保存都必须放弃，避免把目录写回来
	dirty      bool      // 自上次落盘后是否有实际改动
	dirtySince time.Time // 本轮改动开始的时间，用于限制连续编辑下的落盘延迟
	rev        int64     // 状态版本号：内容或在线状态一变就加一，轮询兜底靠它跳过无变化的拉取
}

/* ---------------- 房间与持久化 ---------------- */

func (a *App) roomFor(product, script string) (*Room, error) {
	p, err := sanitizeName(product)
	if err != nil {
		return nil, fmt.Errorf("产品名无效: %w", err)
	}
	s, err := sanitizeName(script)
	if err != nil {
		return nil, fmt.Errorf("脚本名无效: %w", err)
	}
	dir := filepath.Join(a.dataDir, p, s)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("产品或脚本不存在")
	}

	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	if a.rooms == nil {
		a.rooms = make(map[string]*Room)
	}
	if r, ok := a.rooms[dir]; ok {
		return r, nil
	}
	r := &Room{app: a, product: p, script: s, dir: dir, clients: make(map[*wsClient]struct{})}
	r.load()
	a.rooms[dir] = r
	return r, nil
}

func (a *App) existingRoom(dir string) *Room {
	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	return a.rooms[dir]
}

// flushAllRooms 在进程退出前把内存里的协作状态落盘。
func (a *App) flushAllRooms() {
	a.hubMu.Lock()
	rooms := make([]*Room, 0, len(a.rooms))
	for _, r := range a.rooms {
		rooms = append(rooms, r)
	}
	a.hubMu.Unlock()
	for _, r := range rooms {
		r.saveNow()
	}
}

func (r *Room) load() {
	r.dirty = true // 首次加载后至少落盘一次，保证三个文件都存在
	docPath := filepath.Join(r.dir, docFileName)
	collabPath := filepath.Join(r.dir, collabFileName)
	r.hist = readHistoryFile(historyPath(r.dir))

	var st collabFile
	loaded := false
	if b, err := os.ReadFile(collabPath); err == nil {
		if json.Unmarshal(b, &st) == nil && st.Epoch != "" && st.Regs != nil {
			loaded = true
		}
	}
	if loaded {
		// 文案.json 被外部工具手工改过时，以它为准重建协作状态
		if di, err := os.Stat(docPath); err == nil {
			if ci, err := os.Stat(collabPath); err == nil && di.ModTime().After(ci.ModTime().Add(importSlack)) {
				loaded = false
				log.Printf("协作状态重建: %s 比 协同.json 新，按手工修改的 文案.json 导入", docPath)
			}
		}
	}
	if loaded {
		r.st = st
		r.savedAt = st.SavedAt
		if r.ensureRowLocked() {
			r.saveNow()
		}
		return
	}
	doc, err := readDoc(docPath, r.product, r.script)
	if err != nil {
		doc = defaultDoc(r.product, r.script)
	}
	r.st = collabFile{Version: 1, Epoch: newID(), Regs: make(map[string]Reg)}
	r.importLocked(doc, "import", time.Now().UnixMilli())
	r.ensureRowLocked()
	r.saveNow()
}

func (r *Room) liveRowIDsLocked() map[string]bool {
	ids := make(map[string]bool)
	dead := make(map[string]bool)
	for key, reg := range r.st.Regs {
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 || parts[0] != "s" || reg.V == nil {
			continue
		}
		switch parts[2] {
		case "dead":
			var b bool
			if json.Unmarshal(reg.V, &b) == nil && b {
				dead[parts[1]] = true
			}
		case "order":
			ids[parts[1]] = true
		}
	}
	for id := range dead {
		delete(ids, id)
	}
	return ids
}

// ensureRowLocked 保证每个脚本至少有一行，避免多人同时首次打开时各建一行。
func (r *Room) ensureRowLocked() bool {
	if len(r.liveRowIDsLocked()) > 0 {
		return false
	}
	base := time.Now().UnixMilli()
	id := newID()
	r.setLocked("s|"+id+"|order", float64(base), base, "init")
	r.setLocked("s|"+id+"|no", "1", base+1, "init")
	return true
}

func (r *Room) setLocked(key string, value any, t int64, site string) {
	b, err := json.Marshal(value)
	if err != nil {
		return
	}
	reg := Reg{T: t, S: site, V: b}
	if old, ok := r.st.Regs[key]; ok && !reg.newerThan(old) {
		return
	}
	r.st.Regs[key] = reg
	r.dirty = true
}

// importLocked 把一篇普通文档转成寄存器（用于首次导入与旧接口整体覆盖）。
func (r *Room) importLocked(doc Doc, site string, base int64) {
	n := int64(0)
	next := func() int64 {
		n++
		return base + n
	}
	for i, shot := range doc.Shots {
		id := strings.TrimSpace(shot.ID)
		if id == "" {
			id = newID()
		}
		r.setLocked("s|"+id+"|order", float64(base+int64(i)), next(), site)
		r.setLocked("s|"+id+"|no", shot.No, next(), site)
		if shot.Content != "" {
			r.setLocked("s|"+id+"|content", shot.Content, next(), site)
		}
		if shot.Narration != "" {
			r.setLocked("s|"+id+"|narration", shot.Narration, next(), site)
		}
		if shot.Copy != "" {
			r.setLocked("s|"+id+"|copy", shot.Copy, next(), site)
		}
		if shot.Video != nil && shot.Video.File != "" {
			r.setLocked("s|"+id+"|video", shot.Video, next(), site)
		}
	}
	if doc.Edit != nil && doc.Edit.Source != "" {
		r.setLocked("e|source", doc.Edit.Source, next(), site)
		r.setLocked("e|sourceName", doc.Edit.SourceName, next(), site)
		r.setLocked("e|fps", doc.Edit.FPS, next(), site)
		r.setLocked("e|duration", doc.Edit.Duration, next(), site)
		r.setLocked("e|cuts", doc.Edit.Cuts, next(), site)
		r.setLocked("e|currentTime", doc.Edit.CurrentTime, next(), site)
	}
}

// buildDocLocked 把寄存器投影成 文案.json 的结构。
func (r *Room) buildDocLocked(updatedAt string) Doc {
	doc := Doc{Version: 1, Product: r.product, Script: r.script, Shots: []Shot{}, UpdatedAt: updatedAt}

	type rowState struct {
		id        string
		order     float64
		hasOrder  bool
		dead      bool
		no        string
		content   string
		narration string
		copy      string
		video     *VideoRef
	}
	rows := make(map[string]*rowState)
	get := func(id string) *rowState {
		rs, ok := rows[id]
		if !ok {
			rs = &rowState{id: id}
			rows[id] = rs
		}
		return rs
	}

	for key, reg := range r.st.Regs {
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 || reg.V == nil {
			continue
		}
		if parts[0] != "s" {
			continue
		}
		rs := get(parts[1])
		switch parts[2] {
		case "dead":
			var b bool
			if json.Unmarshal(reg.V, &b) == nil && b {
				rs.dead = true
			}
		case "order":
			var f float64
			if json.Unmarshal(reg.V, &f) == nil {
				rs.order = f
			}
			rs.hasOrder = true
		case "no":
			_ = json.Unmarshal(reg.V, &rs.no)
		case "content":
			_ = json.Unmarshal(reg.V, &rs.content)
		case "narration":
			_ = json.Unmarshal(reg.V, &rs.narration)
		case "copy":
			_ = json.Unmarshal(reg.V, &rs.copy)
		case "video":
			var ref VideoRef
			if json.Unmarshal(reg.V, &ref) == nil && ref.File != "" {
				rs.video = &ref
			}
		}
	}

	list := make([]*rowState, 0, len(rows))
	for _, rs := range rows {
		if rs.dead || !rs.hasOrder {
			continue
		}
		list = append(list, rs)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].order != list[j].order {
			return list[i].order < list[j].order
		}
		return list[i].id < list[j].id
	})
	for _, rs := range list {
		doc.Shots = append(doc.Shots, Shot{
			ID:        rs.id,
			No:        rs.no,
			Content:   rs.content,
			Narration: rs.narration,
			Copy:      rs.copy,
			Video:     rs.video,
		})
	}

	var es EditState
	for key, reg := range r.st.Regs {
		if reg.V == nil || !strings.HasPrefix(key, "e|") {
			continue
		}
		switch strings.TrimPrefix(key, "e|") {
		case "source":
			_ = json.Unmarshal(reg.V, &es.Source)
		case "sourceName":
			_ = json.Unmarshal(reg.V, &es.SourceName)
		case "fps":
			_ = json.Unmarshal(reg.V, &es.FPS)
		case "duration":
			_ = json.Unmarshal(reg.V, &es.Duration)
		case "cuts":
			_ = json.Unmarshal(reg.V, &es.Cuts)
		case "currentTime":
			_ = json.Unmarshal(reg.V, &es.CurrentTime)
		}
	}
	if strings.TrimSpace(es.Source) != "" {
		doc.Edit = &es
	}
	return doc
}

func (r *Room) snapshot() (collabFile, Doc, []peerInfo, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *Room) snapshotLocked() (collabFile, Doc, []peerInfo, string) {
	updated := time.Now().Format(time.RFC3339)
	regs := make(map[string]Reg, len(r.st.Regs))
	for k, v := range r.st.Regs {
		regs[k] = v
	}
	st := collabFile{Version: 1, Epoch: r.st.Epoch, SavedAt: r.savedAt, Regs: regs}
	doc := r.buildDocLocked(updated)
	return st, doc, r.peersLocked(), r.savedAt
}

func (r *Room) docNow() Doc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buildDocLocked(time.Now().Format(time.RFC3339))
}

func (r *Room) scheduleSave() {
	r.mu.Lock()
	if r.dead {
		r.mu.Unlock()
		return
	}
	if r.saveTimer == nil {
		r.saveTimer = time.AfterFunc(saveDebounce, r.saveNow)
		r.dirtySince = time.Now()
		r.mu.Unlock()
		return
	}
	// 连续编辑会不断重置防抖；改动积压超过 saveMaxDelay 时立即落一次盘兜底。
	overdue := !r.dirtySince.IsZero() && time.Since(r.dirtySince) >= saveMaxDelay
	r.saveTimer.Reset(saveDebounce)
	r.mu.Unlock()
	if overdue {
		go r.saveNow()
	}
}

// saveNow 先写 文案.json（物化导出），再写 协同.json（权威状态，紧凑编码）。
// 没有实际改动时直接跳过，避免空转写盘。
func (r *Room) saveNow() {
	updated := time.Now().Format(time.RFC3339)
	r.mu.Lock()
	if r.saveTimer != nil {
		r.saveTimer.Stop()
		r.saveTimer = nil
	}
	if r.dead || !r.dirty {
		r.mu.Unlock()
		return
	}
	regs := make(map[string]Reg, len(r.st.Regs))
	for k, v := range r.st.Regs {
		regs[k] = v
	}
	doc := r.buildDocLocked(updated)
	r.savedAt = updated
	st := collabFile{Version: 1, Epoch: r.st.Epoch, SavedAt: updated, Regs: regs}
	hist := make([]historyEntry, len(r.hist))
	copy(hist, r.hist)
	r.dirty = false
	r.dirtySince = time.Time{}
	r.mu.Unlock()

	if b, err := json.MarshalIndent(doc, "", "  "); err == nil {
		_ = writeFileAtomic(filepath.Join(r.dir, docFileName), b)
	}
	if b, err := json.Marshal(st); err == nil {
		_ = writeFileAtomic(filepath.Join(r.dir, collabFileName), b)
	}
	_ = writeHistoryFile(historyPath(r.dir), hist)
	if r.app != nil {
		r.app.invalidateMediaIndex()
	}
}

/* ---------------- 合并 ---------------- */

func validRegKey(key string) bool {
	if key == "" || len(key) > maxRegKeyLen {
		return false
	}
	for _, ch := range key {
		if ch < 32 || ch == 127 {
			return false
		}
	}
	return true
}

// merge 把客户端上报的寄存器合并进房间，返回真正生效的部分，
// 以及这次改动里可能失去引用的视频路径候选（交给异步清理队列核对）。
// 传入的 site 会覆盖寄存器里自带的编辑者标识，避免冒充别人。
// 生效的改动会顺手记进修改历史（每批最多 maxHistoryBatch 条；同一字段
// 短时间内的连续输入会合并成一条，避免刷屏）。
func (r *Room) merge(site, name string, regs map[string]Reg) (map[string]Reg, []string) {
	changed := make(map[string]Reg)
	if site == "" || len(regs) == 0 {
		return changed, nil
	}
	var deltas []regDelta
	var lostRefs []string // 可能失去引用的视频路径，最终以全库引用计数为准
	r.mu.Lock()
	n := 0
	for key, reg := range regs {
		if n >= maxRegsPerMsg {
			break
		}
		n++
		if !validRegKey(key) || reg.T <= 0 || len(reg.V) == 0 || len(reg.V) > maxRegValueLen {
			continue
		}
		reg.S = site
		old, existed := r.st.Regs[key]
		if existed && !reg.newerThan(old) {
			continue
		}
		r.st.Regs[key] = reg
		r.dirty = true
		r.rev++
		changed[key] = reg
		if existed {
			if p := r.oldMediaPathLocked(key, old); p != "" {
				lostRefs = append(lostRefs, p)
			}
		}
		if len(deltas) < maxHistoryBatch {
			d := regDelta{key: key, after: reg}
			if existed {
				prev := old
				d.before = &prev
			}
			deltas = append(deltas, d)
		}
	}
	// 整行打上墓碑：video 寄存器本身没动，也算失去一处引用。
	for key, reg := range changed {
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 || parts[0] != "s" || parts[2] != "dead" {
			continue
		}
		var dead bool
		if json.Unmarshal(reg.V, &dead) != nil || !dead {
			continue
		}
		if vreg, ok := r.st.Regs["s|"+parts[1]+"|video"]; ok {
			if p := r.videoRegPathLocked(vreg); p != "" {
				lostRefs = append(lostRefs, p)
			}
		}
	}
	if len(deltas) > 0 {
		by := name
		if by == "" {
			by = "同事"
		}
		r.hist = appendHistoryEntries(r.hist, r.buildHistoryLocked(deltas, by))
	}
	r.mu.Unlock()
	if len(changed) > 0 {
		r.scheduleSave()
	}
	return changed, lostRefs
}

// oldMediaPathLocked 从被覆盖寄存器的旧值里取出视频路径；只关心 s|<id>|video 与 e|source。
func (r *Room) oldMediaPathLocked(key string, old Reg) string {
	if old.V == nil {
		return ""
	}
	if key == "e|source" {
		var src string
		if json.Unmarshal(old.V, &src) != nil || strings.TrimSpace(src) == "" {
			return ""
		}
		return r.app.safeTempFile(src)
	}
	parts := strings.SplitN(key, "|", 3)
	if len(parts) != 3 || parts[0] != "s" || parts[2] != "video" {
		return ""
	}
	return r.videoRegPathLocked(old)
}

func (r *Room) videoRegPathLocked(reg Reg) string {
	if reg.V == nil || r.app == nil {
		return ""
	}
	var ref VideoRef
	if json.Unmarshal(reg.V, &ref) != nil || ref.File == "" {
		return ""
	}
	return r.app.videoRefPath(r.dir, &ref)
}

// replaceWithDoc 供旧的整体保存接口使用：整篇覆盖后通知在线客户端重新拉取。
func (r *Room) replaceWithDoc(doc Doc) {
	r.mu.Lock()
	r.dirty = true
	r.rev++
	for k := range r.st.Regs {
		if strings.HasPrefix(k, "s|") || strings.HasPrefix(k, "e|") {
			delete(r.st.Regs, k)
		}
	}
	r.importLocked(doc, "legacy", time.Now().UnixMilli())
	r.ensureRowLocked()
	r.mu.Unlock()
	r.saveNow()
	r.broadcast(nil, map[string]any{"t": "reload"})
}

/* ---------------- 在线状态 ---------------- */

func (r *Room) peersLocked() []peerInfo {
	out := make([]peerInfo, 0, len(r.clients))
	for c := range r.clients {
		if c.site == "" {
			continue
		}
		out = append(out, peerInfo{Site: c.site, Name: c.name, Color: c.color, Cell: c.cell})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Site < out[j].Site })
	return out
}

func (r *Room) peers() []peerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peersLocked()
}

func (r *Room) addClient(c *wsClient) {
	r.mu.Lock()
	r.clients[c] = struct{}{}
	r.rev++
	st, doc, peers, savedAt := r.snapshotLocked()
	r.mu.Unlock()

	c.sendJSON(map[string]any{
		"t": "hello", "site": c.site, "epoch": st.Epoch, "now": time.Now().UnixMilli(),
		"savedAt": savedAt, "regs": st.Regs, "doc": doc, "peers": peers, "maxEditors": maxEditors,
	})
	r.broadcast(c, map[string]any{"t": "peers", "peers": r.peers()})
}

func (r *Room) removeClient(c *wsClient) {
	r.mu.Lock()
	_, ok := r.clients[c]
	delete(r.clients, c)
	if ok {
		r.rev++
	}
	r.mu.Unlock()
	if ok {
		r.broadcast(nil, map[string]any{"t": "peers", "peers": r.peers()})
	}
}

func (r *Room) setPresence(c *wsClient, cell string) {
	if len(cell) > maxRegKeyLen {
		cell = ""
	}
	r.mu.Lock()
	changed := c.cell != cell
	if changed {
		c.cell = cell
		r.rev++
	}
	r.mu.Unlock()
	if changed {
		r.broadcast(c, map[string]any{"t": "peers", "peers": r.peers()})
	}
}

func (r *Room) renameClient(c *wsClient, name string) {
	r.mu.Lock()
	c.name = name
	r.rev++
	r.mu.Unlock()
	r.broadcast(nil, map[string]any{"t": "peers", "peers": r.peers()})
}

func (r *Room) broadcast(skip *wsClient, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	r.mu.Lock()
	targets := make([]*wsClient, 0, len(r.clients))
	for c := range r.clients {
		if c != skip {
			targets = append(targets, c)
		}
	}
	r.mu.Unlock()
	for _, c := range targets {
		if c.writeFrame(0x1, b) != nil {
			c.close()
		}
	}
}

// broadcastAll 给所有房间的在线客户端发同一条消息（列表级变更：使用状态、重命名、删除）。
func (a *App) broadcastAll(v any) {
	a.hubMu.Lock()
	rooms := make([]*Room, 0, len(a.rooms))
	for _, r := range a.rooms {
		rooms = append(rooms, r)
	}
	a.hubMu.Unlock()
	for _, r := range rooms {
		r.broadcast(nil, v)
	}
}

// evictRoom 把房间从内存摘掉：先落盘，再通知并断开在线客户端。
func (a *App) evictRoom(dir string, notice any) {
	a.hubMu.Lock()
	room := a.rooms[dir]
	delete(a.rooms, dir)
	a.hubMu.Unlock()
	if room == nil {
		return
	}
	room.saveNow()
	if notice != nil {
		room.broadcast(nil, notice)
	}
	room.closeClients()
}

// forgetRoom 在目录已经删除后移除房间：不再 saveNow（否则会把目录写回来），
// 只广播提示并断开在线客户端。
func (a *App) forgetRoom(dir string, notice any) {
	a.hubMu.Lock()
	room := a.rooms[dir]
	delete(a.rooms, dir)
	a.hubMu.Unlock()
	if room == nil {
		return
	}
	// 标记房间作废并停掉防抖保存：目录已经删了，迟到的定时器不能再把文件夹写回来
	room.mu.Lock()
	room.dead = true
	if room.saveTimer != nil {
		room.saveTimer.Stop()
		room.saveTimer = nil
	}
	room.mu.Unlock()
	if notice != nil {
		room.broadcast(nil, notice)
	}
	room.closeClients()
}

func (r *Room) closeClients() {
	r.mu.Lock()
	clients := make([]*wsClient, 0, len(r.clients))
	for c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	for _, c := range clients {
		c.close()
	}
}

/* ---------------- WebSocket ---------------- */

type wsClient struct {
	room  *Room
	conn  net.Conn
	br    *bufio.Reader
	site  string
	name  string
	color string
	cell  string

	wmu  sync.Mutex
	once sync.Once
	frag []byte
	done chan struct{}
}

type collabMsg struct {
	T    string         `json:"t"`
	Cell string         `json:"cell,omitempty"`
	Name string         `json:"name,omitempty"`
	Regs map[string]Reg `json:"regs,omitempty"`
}

func wsAcceptKey(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func headerHasToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func cleanLabel(s string) string {
	s = strings.TrimSpace(s)
	out := make([]rune, 0, 16)
	for _, r := range s {
		if r < 32 || r == 127 {
			continue
		}
		out = append(out, r)
		if len(out) >= 16 {
			break
		}
	}
	return string(out)
}

func cleanColor(s string) string {
	s = strings.TrimSpace(s)
	if len(s) != 7 || s[0] != '#' {
		return ""
	}
	for _, ch := range s[1:] {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
			return ""
		}
	}
	return s
}

var peerPalette = []string{"#4c8dff", "#f2994a", "#27ae60", "#9b51e0", "#eb5757", "#2d9cdb", "#f2c94c", "#00b8a9"}

func colorForSite(site string) string {
	var h int
	for _, ch := range site {
		h = (h*31 + int(ch)) % 100000
	}
	return peerPalette[h%len(peerPalette)]
}

func (c *wsClient) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n := len(payload)
	var head [10]byte
	var hlen int
	switch {
	case n < 126:
		head[0] = 0x80 | op
		head[1] = byte(n)
		hlen = 2
	case n <= 0xffff:
		head[0] = 0x80 | op
		head[1] = 126
		binary.BigEndian.PutUint16(head[2:4], uint16(n))
		hlen = 4
	default:
		head[0] = 0x80 | op
		head[1] = 127
		binary.BigEndian.PutUint64(head[2:10], uint64(n))
		hlen = 10
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(head[:hlen]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (c *wsClient) sendJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	if c.writeFrame(0x1, b) != nil {
		c.close()
	}
}

func (c *wsClient) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// readMessage 读取一个完整的数据帧（自动处理分片、ping/pong）。
func (c *wsClient) readMessage() (byte, []byte, error) {
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
			return 0, nil, err
		}
		fin := hdr[0]&0x80 != 0
		op := hdr[0] & 0x0f
		masked := hdr[1]&0x80 != 0
		n := int64(hdr[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return 0, nil, err
			}
			u := binary.BigEndian.Uint64(b[:])
			if u > maxWSFrame {
				return 0, nil, errFrameTooLarge
			}
			n = int64(u)
		}
		if n < 0 || n > maxWSFrame {
			return 0, nil, errFrameTooLarge
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8:
			return 0, nil, io.EOF
		case 0x9:
			_ = c.writeFrame(0xA, payload)
			continue
		case 0xA:
			continue
		case 0x0:
			if len(c.frag)+len(payload) > maxWSFrame {
				return 0, nil, errFrameTooLarge
			}
			c.frag = append(c.frag, payload...)
			if !fin {
				continue
			}
			out := c.frag
			c.frag = nil
			return 0x1, out, nil
		default:
			if !fin {
				c.frag = append([]byte(nil), payload...)
				continue
			}
			return op, payload, nil
		}
	}
}

func (a *App) handleCollabWS(w http.ResponseWriter, r *http.Request) {
	room, err := a.roomFor(r.URL.Query().Get("product"), r.URL.Query().Get("script"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHasToken(r.Header.Get("Connection"), "upgrade") {
		fail(w, http.StatusBadRequest, errNotUpgrade.Error())
		return
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		fail(w, http.StatusBadRequest, "WebSocket 版本不受支持")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		fail(w, http.StatusInternalServerError, "当前服务不支持 WebSocket")
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		fail(w, http.StatusInternalServerError, "WebSocket 握手失败")
		return
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		wsAcceptKey(key) + "\r\n\r\n"
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(resp)); err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})

	site := cleanLabel(r.URL.Query().Get("site"))
	if site == "" || len(site) > 64 {
		site = newID()
	}
	name := cleanLabel(r.URL.Query().Get("name"))
	if name == "" {
		name = "同事-" + site
		if len(site) > 4 {
			name = "同事-" + site[:4]
		}
	}
	color := cleanColor(r.URL.Query().Get("color"))
	if color == "" {
		color = colorForSite(site)
	}

	c := &wsClient{room: room, conn: conn, br: brw.Reader, site: site, name: name, color: color, done: make(chan struct{})}
	room.addClient(c)
	defer func() {
		room.removeClient(c)
		c.close()
	}()

	go func() {
		t := time.NewTicker(wsPingEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if c.writeFrame(0x9, nil) != nil {
					c.close()
					return
				}
			case <-c.done:
				return
			}
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(wsReadWindow))
		op, data, err := c.readMessage()
		if err != nil {
			return
		}
		if op != 0x1 {
			continue
		}
		var msg collabMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "merge":
			changed, lost := room.merge(site, c.name, msg.Regs)
			if len(changed) > 0 {
				room.broadcast(c, map[string]any{"t": "merge", "from": site, "regs": changed})
			}
			c.sendJSON(map[string]any{"t": "ack", "now": time.Now().UnixMilli(), "savedAt": room.savedAtValue()})
			if len(lost) > 0 {
				a.enqueueMediaCleanup(room.dir, lost, c.name)
			}
		case "presence":
			room.setPresence(c, msg.Cell)
		case "rename":
			if n := cleanLabel(msg.Name); n != "" {
				room.renameClient(c, n)
			}
		case "bye":
			return
		}
	}
}

func (r *Room) savedAtValue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.savedAt
}

// revValue 返回当前状态版本号。轮询客户端带上自己手里的版本，服务端没变就只回一个 noop。
func (r *Room) revValue() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rev
}

/* ---------------- HTTP 接口（轮询兜底 / 首次加载） ---------------- */

func (a *App) handleCollabState(w http.ResponseWriter, r *http.Request) {
	room, err := a.roomFor(r.URL.Query().Get("product"), r.URL.Query().Get("script"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	// 轮询增量：客户端带上手里的版本号，没变化就只回一个 noop，省掉全量投影和传输。
	if since := strings.TrimSpace(r.URL.Query().Get("rev")); since != "" {
		if cur := room.revValue(); strconv.FormatInt(cur, 10) == since {
			writeJSON(w, http.StatusOK, map[string]any{
				"noop": true, "rev": cur, "now": time.Now().UnixMilli(),
			})
			return
		}
	}
	rev := room.revValue() // 取快照前的版本：宁可多发一次全量，也不能漏
	st, doc, peers, savedAt := room.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"epoch":   st.Epoch,
		"rev":     rev,
		"now":     time.Now().UnixMilli(),
		"savedAt": savedAt,
		"regs":    st.Regs,
		"doc":     doc,
		"peers":   peers,
		"limits":  map[string]any{"maxEditors": maxEditors},
	})
}

func (a *App) handleCollabMerge(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCollabBody)
	var req struct {
		Product string         `json:"product"`
		Script  string         `json:"script"`
		Site    string         `json:"site"`
		Name    string         `json:"name"`
		Regs    map[string]Reg `json:"regs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	room, err := a.roomFor(req.Product, req.Script)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	site := cleanLabel(req.Site)
	if site == "" {
		fail(w, http.StatusBadRequest, "缺少协作标识")
		return
	}
	changed, lost := room.merge(site, cleanLabel(req.Name), req.Regs)
	if len(lost) > 0 {
		a.enqueueMediaCleanup(room.dir, lost, cleanLabel(req.Name))
	}
	rev := room.revValue()
	st, doc, peers, savedAt := room.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"changed": len(changed),
		"regs":    changed,
		"epoch":   st.Epoch,
		"rev":     rev,
		"now":     time.Now().UnixMilli(),
		"doc":     doc,
		"peers":   peers,
		"savedAt": savedAt,
	})
}
