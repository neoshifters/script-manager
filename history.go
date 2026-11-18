package main

// 修改历史：每个脚本目录下保存一份 _历史.json，记录最近的镜头增删、文案修改、
// 序号调整、素材更换与使用状态变更，并提供「回滚到某个时间点之前」的能力。
//
// 体量控制：只保留最近 historyLimit 条；每次合并最多记 maxHistoryBatch 条，
// 同一批次的多次输入会按字段合并成一条。回滚不是快照回放，而是把回滚点之后
// 每个字段的「最早旧值」重新写回寄存器（CRDT 的正常写入，其它端会自动同步）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	historyFileName = "_历史.json"
	historyLimit    = 150
	maxHistoryBatch = 40
	// 同一人、同一字段在这段时间内的连续输入只记一条，避免打字把历史刷屏。
	historyMergeWindow = 8 * time.Second
)

type historyEntry struct {
	ID   string `json:"id"`
	T    int64  `json:"t"`
	At   string `json:"at"`
	By   string `json:"by"`
	Kind string `json:"kind"` // add / del / text / no / video / status / rollback
	Key  string `json:"key,omitempty"`
	Text string `json:"text"`
	Prev string `json:"prev,omitempty"`
	Next string `json:"next,omitempty"`
	// Before 是回滚用的原始寄存器值（JSON 文本）；该字段当时不存在时为 null
	Before *string `json:"before,omitempty"`
}

// regDelta 是一次合并里生效的单个寄存器改动；before 为 nil 表示该字段是新增。
type regDelta struct {
	key    string
	before *Reg
	after  Reg
}

type historyFileData struct {
	Version int            `json:"version"`
	Entries []historyEntry `json:"entries"`
}

func historyPath(dir string) string {
	return filepath.Join(dir, historyFileName)
}

func readHistoryFile(path string) []historyEntry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f historyFileData
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return trimHistory(f.Entries)
}

func writeHistoryFile(path string, list []historyEntry) error {
	if len(list) == 0 {
		return nil
	}
	f := historyFileData{Version: 1, Entries: list}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// trimHistory 保留最近 historyLimit 条。
func trimHistory(list []historyEntry) []historyEntry {
	if len(list) <= historyLimit {
		return list
	}
	out := make([]historyEntry, historyLimit)
	copy(out, list[len(list)-historyLimit:])
	return out
}

// appendHistoryEntries 把新条目接进历史：同一人、同一字段短时间内的连续「修改文案」
// 类条目会就地合并（保留最早的旧值、更新新值和时间），其余照常追加。
func appendHistoryEntries(list []historyEntry, entries []historyEntry) []historyEntry {
	for _, e := range entries {
		if (e.Kind == "text" || e.Kind == "no") && e.Key != "" && len(list) > 0 {
			last := &list[len(list)-1]
			if last.Kind == e.Kind && last.Key == e.Key && last.By == e.By &&
				e.T-last.T <= int64(historyMergeWindow/time.Millisecond) {
				last.T = e.T
				last.At = e.At
				last.Next = e.Next
				continue
			}
		}
		list = append(list, e)
	}
	return trimHistory(list)
}

func reverseHistory(list []historyEntry) {
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
}

func truncText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// regText 把寄存器里的 JSON 值转成可读文本。
func regText(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	if len(v) == 0 || string(v) == "null" {
		return ""
	}
	return string(v)
}

// buildHistoryLocked 把一批生效的寄存器改动翻译成人类可读的历史记录。
// 只按需读取涉及的寄存器，不做整篇投影，避免打字密集时反复全量 buildDoc。
func (r *Room) buildHistoryLocked(deltas []regDelta, by string) []historyEntry {
	noFor := func(id string) string {
		if reg, ok := r.st.Regs["s|"+id+"|no"]; ok && reg.V != nil {
			var v string
			if json.Unmarshal(reg.V, &v) == nil && strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}

	fieldName := map[string]string{"content": "镜头内容", "narration": "口播", "copy": "文案"}
	out := make([]historyEntry, 0, len(deltas))
	for _, d := range deltas {
		parts := strings.SplitN(d.key, "|", 3)
		if len(parts) != 3 || parts[0] != "s" {
			continue
		}
		id, field := parts[1], parts[2]
		no := noFor(id)
		label := "镜头 " + no
		if no == "" {
			label = "该镜头"
		}
		e := historyEntry{ID: newID(), By: by, Key: d.key}
		switch field {
		case "dead":
			var dead bool
			if json.Unmarshal(d.after.V, &dead) != nil || !dead {
				continue
			}
			e.Kind = "del"
			e.Text = "删除" + label
		case "order":
			if d.before != nil {
				continue // 只记录新增，拖动排序不刷屏
			}
			e.Kind = "add"
			e.Text = "新增" + label
		case "content", "narration", "copy":
			next := regText(d.after.V)
			prev := ""
			if d.before != nil {
				prev = regText(d.before.V)
			}
			e.Kind = "text"
			e.Text = "修改" + label + "的" + fieldName[field]
			e.Prev = truncText(prev, 60)
			e.Next = truncText(next, 60)
		case "no":
			e.Kind = "no"
			e.Text = "修改" + label + "的序号"
			if d.before != nil {
				e.Prev = truncText(regText(d.before.V), 20)
			}
			e.Next = truncText(regText(d.after.V), 20)
		case "video":
			e.Kind = "video"
			e.Text = "更换" + label + "的素材"
		default:
			continue
		}
		// 体量控制：超大旧值不存快照，回滚时按空值处理，避免历史文件膨胀
		if d.before != nil && len(d.before.V) <= 8192 {
			b := string(d.before.V)
			e.Before = &b
		}
		out = append(out, e)
		if len(out) >= maxHistoryBatch {
			break
		}
	}

	base := time.Now().UnixMilli()
	for i := range out {
		out[i].T = base + int64(i)
		out[i].At = time.UnixMilli(out[i].T).Format(time.RFC3339)
	}
	return out
}

func (r *Room) appendHistory(entries []historyEntry) {
	if len(entries) == 0 {
		return
	}
	r.mu.Lock()
	r.hist = appendHistoryEntries(r.hist, entries)
	r.dirty = true
	r.mu.Unlock()
	r.scheduleSave()
}

// historySnapshot 返回按时间倒序排列的副本。
func (r *Room) historySnapshot() []historyEntry {
	r.mu.Lock()
	list := make([]historyEntry, len(r.hist))
	copy(list, r.hist)
	r.mu.Unlock()
	reverseHistory(list)
	return list
}

func (r *Room) findHistory(id string) (historyEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.hist {
		if e.ID == id {
			return e, true
		}
	}
	return historyEntry{}, false
}

// rollbackBefore 撤销 t（含）之后的所有字段改动，返回涉及的字段数。
func (r *Room) rollbackBefore(t int64, by string) int {
	r.mu.Lock()
	r.dirty = true
	r.rev++
	plans := make(map[string]historyEntry)
	keys := make([]string, 0, 16)
	for _, e := range r.hist {
		if e.T < t || e.Key == "" || e.Kind == "rollback" || e.Kind == "status" {
			continue
		}
		if _, ok := plans[e.Key]; ok {
			continue
		}
		plans[e.Key] = e
		keys = append(keys, e.Key)
	}

	now := time.Now().UnixMilli()
	n := int64(0)
	for _, key := range keys {
		e := plans[key]
		n++
		if e.Before != nil && json.Valid([]byte(*e.Before)) {
			r.st.Regs[key] = Reg{T: now + n, S: "rollback", V: json.RawMessage(*e.Before)}
			continue
		}
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 {
			continue
		}
		switch parts[2] {
		case "order":
			// 回滚点之后才新增的镜头：重新打上墓碑
			b, _ := json.Marshal(true)
			r.st.Regs["s|"+parts[1]+"|dead"] = Reg{T: now + n, S: "rollback", V: b}
		case "dead":
			// 这一行当时本来就不存在，跳过
		default:
			b, _ := json.Marshal("")
			r.st.Regs[key] = Reg{T: now + n, S: "rollback", V: b}
		}
	}

	r.hist = trimHistory(append(r.hist, historyEntry{
		ID:   newID(),
		T:    now,
		At:   time.UnixMilli(now).Format(time.RFC3339),
		By:   by,
		Kind: "rollback",
		Text: "回滚到 " + time.UnixMilli(t).Format("2006-01-02 15:04:05") + " 之前",
	}))
	r.mu.Unlock()

	r.saveNow()
	r.broadcast(nil, map[string]any{"t": "reload"})
	return len(keys)
}

/* ---------------- 使用状态的历史 ---------------- */

func (a *App) noteStatusHistory(product, script string, used bool, by string) {
	dir := filepath.Join(a.dataDir, product, script)
	now := time.Now().UnixMilli()
	text := "标记为「使用」"
	if !used {
		text = "取消「使用」标记"
	}
	entry := historyEntry{
		ID:   newID(),
		T:    now,
		At:   time.UnixMilli(now).Format(time.RFC3339),
		By:   by,
		Kind: "status",
		Text: text,
	}
	if room := a.existingRoom(dir); room != nil {
		room.appendHistory([]historyEntry{entry})
		return
	}
	path := historyPath(dir)
	list := append(readHistoryFile(path), entry)
	_ = writeHistoryFile(path, trimHistory(list))
}

/* ---------------- HTTP 接口 ---------------- */

func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	dir, err := a.scriptDir(r.URL.Query().Get("product"), r.URL.Query().Get("script"), true)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	var list []historyEntry
	if room := a.existingRoom(dir); room != nil {
		list = room.historySnapshot()
	} else {
		list = readHistoryFile(historyPath(dir))
		reverseHistory(list)
	}
	if list == nil {
		list = []historyEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "limit": historyLimit})
}

func (a *App) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product string `json:"product"`
		Script  string `json:"script"`
		ID      string `json:"id"`
		Name    string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if _, err := a.scriptDir(req.Product, req.Script, true); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	room, err := a.roomFor(req.Product, req.Script)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	entry, ok := room.findHistory(req.ID)
	if !ok {
		fail(w, http.StatusNotFound, "这条记录已经不在最近的历史里了")
		return
	}
	if entry.Kind == "status" {
		fail(w, http.StatusBadRequest, "使用状态不支持回滚，请直接点「使用 / 取消」")
		return
	}
	by := cleanLabel(req.Name)
	if by == "" {
		by = "同事"
	}
	applied := room.rollbackBefore(entry.T, by)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": applied})
}
