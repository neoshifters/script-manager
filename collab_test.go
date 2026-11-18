package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* ---------------- 合并规则 ---------------- */

func TestStageThreeMergeRules(t *testing.T) {
	app, mux := newTestApp(t)

	// 建产品 / 脚本
	if rec := callJSON(t, mux, "POST", "/api/product", map[string]string{"name": "产品C"}); rec.Code != 200 {
		t.Fatalf("create product: %d", rec.Code)
	}
	if rec := callJSON(t, mux, "POST", "/api/script", map[string]string{"product": "产品C", "name": "脚本C"}); rec.Code != 200 {
		t.Fatalf("create script: %d", rec.Code)
	}

	room, err := app.roomFor("产品C", "脚本C")
	if err != nil {
		t.Fatal(err)
	}
	doc := room.docNow()
	if len(doc.Shots) != 1 {
		t.Fatalf("默认应有一行: %+v", doc.Shots)
	}
	id := doc.Shots[0].ID
	contentKey := "s|" + id + "|content"
	narrationKey := "s|" + id + "|narration"

	reg := func(t int64, v string) Reg {
		b, _ := json.Marshal(v)
		return Reg{T: t, S: "ignored", V: b}
	}

	// 不同列并发写入，都能保留
	changed, _ := room.merge("A", "同事", map[string]Reg{contentKey: reg(1000, "A 的内容")})
	if len(changed) != 1 {
		t.Fatalf("A merge changed=%d", len(changed))
	}
	changed, _ = room.merge("B", "同事", map[string]Reg{narrationKey: reg(1000, "B 的口播")})
	if len(changed) != 1 {
		t.Fatalf("B merge changed=%d", len(changed))
	}
	doc = room.docNow()
	if doc.Shots[0].Content != "A 的内容" || doc.Shots[0].Narration != "B 的口播" {
		t.Fatalf("不同列未合并: %+v", doc.Shots[0])
	}

	// 旧版本不会覆盖新版本
	changed, _ = room.merge("A", "同事", map[string]Reg{contentKey: reg(900, "过期内容")})
	if len(changed) != 0 {
		t.Fatalf("旧版本不应生效: %+v", changed)
	}
	if room.docNow().Shots[0].Content != "A 的内容" {
		t.Fatal("旧版本覆盖了新版本")
	}

	// 同 t 时站点标识大的胜出（确定性）
	room.merge("A", "同事", map[string]Reg{contentKey: reg(2000, "A 新")})
	room.merge("B", "同事", map[string]Reg{contentKey: reg(2000, "B 新")})
	if got := room.docNow().Shots[0].Content; got != "B 新" {
		t.Fatalf("tie-break 应为 B 新，实际 %q", got)
	}

	// 空值/非法键被忽略
	bad := map[string]Reg{
		"":                     reg(9999, "x"),
		"s|" + id + "|content": {T: 0, S: "A", V: json.RawMessage(`"x"`)},
	}
	if c, _ := room.merge("A", "同事", bad); len(c) != 0 {
		t.Fatalf("非法寄存器不应生效: %+v", c)
	}

	// 落盘：文案.json 与 协同.json 一致
	room.saveNow()
	docPath := filepath.Join(app.dataDir, "产品C", "脚本C", docFileName)
	collabPath := filepath.Join(app.dataDir, "产品C", "脚本C", collabFileName)
	var saved Doc
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("文案.json 未生成: %v", err)
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Shots[0].Content != "B 新" || saved.Shots[0].Narration != "B 的口播" {
		t.Fatalf("文案.json 内容不对: %+v", saved.Shots[0])
	}
	var cf collabFile
	if b, err := os.ReadFile(collabPath); err != nil {
		t.Fatalf("协同.json 未生成: %v", err)
	} else if err := json.Unmarshal(b, &cf); err != nil {
		t.Fatal(err)
	}
	if cf.Epoch == "" || len(cf.Regs) == 0 {
		t.Fatalf("协同.json 内容不对: %+v", cf)
	}

	// 删除行（墓碑）
	dead := Reg{T: 5000, S: "A", V: json.RawMessage(`true`)}
	room.merge("A", "同事", map[string]Reg{"s|" + id + "|dead": dead})
	if n := len(room.docNow().Shots); n != 0 {
		t.Fatalf("删除后应为 0 行，实际 %d", n)
	}
	// 删除后旧内容不会把行"复活"
	room.merge("A", "同事", map[string]Reg{contentKey: reg(4000, "改内容")})
	if n := len(room.docNow().Shots); n != 0 {
		t.Fatalf("墓碑应保持删除状态，实际 %d 行", n)
	}

	// 重启前先落盘（生产环境由退出钩子 / 500ms 防抖完成）
	room.saveNow()

	// 重启后状态仍在（同一个数据目录重新建 app）
	r2, err := newApp(app.dataDir).roomFor("产品C", "脚本C")
	if err != nil {
		t.Fatal(err)
	}
	doc2 := r2.docNow()
	// 全删后脚本会补一行空白行，但被删的镜头不能复活
	if len(doc2.Shots) != 1 || doc2.Shots[0].ID == id {
		t.Fatalf("重启后墓碑丢失: %+v", doc2.Shots)
	}
	if doc2.Shots[0].Content != "" {
		t.Fatalf("被删除镜头的内容复活了: %+v", doc2.Shots[0])
	}
	st, _, _, _ := r2.snapshot()
	if st.Epoch != cf.Epoch {
		t.Fatalf("重启后 epoch 变化: %s -> %s", cf.Epoch, st.Epoch)
	}
}

/* ---------------- 真实 WebSocket 往返 ---------------- */

type testWS struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialTestWS(t *testing.T, serverURL, product, script, site string) *testWS {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	target := "/api/collab/ws?product=" + url.QueryEscape(product) + "&script=" + url.QueryEscape(script) +
		"&site=" + url.QueryEscape(site) + "&name=" + url.QueryEscape(site) + "&color=%234c8dff"
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", target, u.Host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("handshake write: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("handshake read: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status: %d", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), wsAcceptKey(key); got != want {
		t.Fatalf("accept key: %s != %s", got, want)
	}
	ws := &testWS{conn: conn, br: br}
	t.Cleanup(func() { _ = conn.Close() })
	return ws
}

func (c *testWS) send(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}
	head := []byte{0x81}
	n := len(b)
	switch {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n <= 0xffff:
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		head = append(head, 0x80|126, ext[0], ext[1])
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		head = append(head, 0x80|127)
		head = append(head, ext[:]...)
	}
	head = append(head, mask[:]...)
	payload := make([]byte, n)
	for i := range b {
		payload[i] = b[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(append(head, payload...)); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// next 读取下一条 JSON 消息（跳过 ping/pong）。
func (c *testWS) next(t *testing.T) map[string]any {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	client := &wsClient{conn: c.conn, br: c.br, done: make(chan struct{})}
	_, data, err := client.readMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return msg
}

func (c *testWS) until(t *testing.T, kind string) map[string]any {
	t.Helper()
	for i := 0; i < 20; i++ {
		msg := c.next(t)
		if msg["t"] == kind {
			return msg
		}
	}
	t.Fatalf("没有收到 %s 消息", kind)
	return nil
}

func TestStageThreeRealtimeWS(t *testing.T) {
	app, mux := newTestApp(t)
	if rec := callJSON(t, mux, "POST", "/api/product", map[string]string{"name": "产品D"}); rec.Code != 200 {
		t.Fatalf("create product: %d", rec.Code)
	}
	if rec := callJSON(t, mux, "POST", "/api/script", map[string]string{"product": "产品D", "name": "脚本D"}); rec.Code != 200 {
		t.Fatalf("create script: %d", rec.Code)
	}

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := dialTestWS(t, srv.URL, "产品D", "脚本D", "siteA")
	hello := a.until(t, "hello")
	epoch, _ := hello["epoch"].(string)
	if epoch == "" {
		t.Fatal("hello 缺少 epoch")
	}
	docRaw, _ := hello["doc"].(map[string]any)
	if docRaw == nil {
		t.Fatal("hello 缺少 doc")
	}
	shots, _ := docRaw["shots"].([]any)
	if len(shots) != 1 {
		t.Fatalf("hello 应带默认行: %+v", docRaw)
	}
	id, _ := shots[0].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatal("默认行缺少 id")
	}

	b := dialTestWS(t, srv.URL, "产品D", "脚本D", "siteB")
	helloB := b.until(t, "hello")
	if helloB["epoch"] != epoch {
		t.Fatalf("两个客户端 epoch 不一致: %v %v", helloB["epoch"], epoch)
	}
	// A 会收到 B 加入后的在线列表
	peersMsg := a.until(t, "peers")
	if peers, _ := peersMsg["peers"].([]any); len(peers) != 2 {
		t.Fatalf("在线人数应为 2: %+v", peersMsg)
	}

	// A 改内容，B 改口播，各自收到对方的增量
	contentKey := "s|" + id + "|content"
	narrationKey := "s|" + id + "|narration"
	a.send(t, map[string]any{"t": "merge", "regs": map[string]any{
		contentKey: map[string]any{"t": time.Now().UnixMilli(), "s": "siteA", "v": "A 写的内容"},
	}})
	m1 := b.until(t, "merge")
	regs1, _ := m1["regs"].(map[string]any)
	if _, ok := regs1[contentKey]; !ok || m1["from"] != "siteA" {
		t.Fatalf("B 未收到 A 的改动: %+v", m1)
	}
	if ack := a.until(t, "ack"); ack["t"] != "ack" {
		t.Fatalf("A 未收到 ack: %+v", ack)
	}

	b.send(t, map[string]any{"t": "merge", "regs": map[string]any{
		narrationKey: map[string]any{"t": time.Now().UnixMilli() + 5, "s": "siteB", "v": "B 写的口播"},
	}})
	m2 := a.until(t, "merge")
	if _, ok := m2["regs"].(map[string]any)[narrationKey]; !ok {
		t.Fatalf("A 未收到 B 的改动: %+v", m2)
	}

	// 在线光标：A 选中内容格，B 能看到位置
	a.send(t, map[string]any{"t": "presence", "cell": contentKey})
	found := false
	for i := 0; i < 10; i++ {
		msg := b.until(t, "peers")
		peers, _ := msg["peers"].([]any)
		for _, p := range peers {
			pm, _ := p.(map[string]any)
			if pm["site"] == "siteA" && pm["cell"] == contentKey {
				found = true
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("B 没看到 A 的编辑位置")
	}

	// B 重命名
	b.send(t, map[string]any{"t": "rename", "name": "剪辑师小李"})
	renamed := false
	for i := 0; i < 10 && !renamed; i++ {
		msg := a.until(t, "peers")
		peers, _ := msg["peers"].([]any)
		for _, p := range peers {
			if pm, _ := p.(map[string]any); pm["site"] == "siteB" && pm["name"] == "剪辑师小李" {
				renamed = true
			}
		}
	}
	if !renamed {
		t.Fatal("重命名未广播")
	}

	// 落盘：两个不同列的改动都在 文案.json 里
	docPath := filepath.Join(app.dataDir, "产品D", "脚本D", docFileName)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var saved Doc
		if b, err := os.ReadFile(docPath); err == nil && json.Unmarshal(b, &saved) == nil && len(saved.Shots) == 1 &&
			saved.Shots[0].Content == "A 写的内容" && saved.Shots[0].Narration == "B 写的口播" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("文案.json 未及时合并两边改动: %s", mustRead(t, docPath))
		}
		time.Sleep(80 * time.Millisecond)
	}

	// B 断线重连：先拿到最新快照，再补发自己的离线改动
	a.send(t, map[string]any{"t": "merge", "regs": map[string]any{
		"e|cuts": map[string]any{"t": time.Now().UnixMilli() + 50, "s": "siteA", "v": []float64{1.5, 3}},
	}})
	b.until(t, "merge")
	_ = b.conn.Close()

	b2 := dialTestWS(t, srv.URL, "产品D", "脚本D", "siteB")
	hello2 := b2.until(t, "hello")
	regs2, _ := hello2["regs"].(map[string]any)
	if _, ok := regs2["e|cuts"]; !ok {
		t.Fatal("重连快照没带上 A 的新改动")
	}
	reDoc, _ := hello2["doc"].(map[string]any)
	reEdit, _ := reDoc["edit"].(map[string]any)
	if reEdit == nil || reEdit["source"] != nil {
		// 没有原片时不应产生 edit 对象
		if reEdit != nil {
			t.Fatalf("不应有 edit: %+v", reEdit)
		}
	}

	// B 补发自己的缓存（含旧时间戳），不应产生重复行或旧值覆盖
	b2.send(t, map[string]any{"t": "merge", "regs": map[string]any{
		narrationKey: map[string]any{"t": 1000, "s": "siteB", "v": "过期口播"},
	}})
	b2.until(t, "ack")
	time.Sleep(150 * time.Millisecond)
	if got := len(mustDoc(t, docPath).Shots); got != 1 {
		t.Fatalf("重连后行数应保持 1，实际 %d", got)
	}
	if got := mustDoc(t, docPath).Shots[0].Narration; got != "B 写的口播" {
		t.Fatalf("重连补发覆盖了较新的值: %q", got)
	}
}

func TestStageThreeHTTPFallback(t *testing.T) {
	app, mux := newTestApp(t)
	if rec := callJSON(t, mux, "POST", "/api/product", map[string]string{"name": "产品E"}); rec.Code != 200 {
		t.Fatal("create product")
	}
	if rec := callJSON(t, mux, "POST", "/api/script", map[string]string{"product": "产品E", "name": "脚本E"}); rec.Code != 200 {
		t.Fatal("create script")
	}
	q := "?product=" + url.QueryEscape("产品E") + "&script=" + url.QueryEscape("脚本E")

	rec := call(t, mux, "GET", "/api/collab"+q+"&site=siteX", nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"epoch"`) {
		t.Fatalf("collab state: %d %s", rec.Code, rec.Body.String())
	}
	var snap struct {
		Epoch string         `json:"epoch"`
		Now   int64          `json:"now"`
		Regs  map[string]any `json:"regs"`
		Doc   Doc            `json:"doc"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Doc.Shots) != 1 {
		t.Fatalf("应有默认行: %+v", snap.Doc)
	}
	id := snap.Doc.Shots[0].ID

	body := map[string]any{
		"product": "产品E", "script": "脚本E", "site": "siteX",
		"regs": map[string]any{
			"s|" + id + "|content": map[string]any{"t": snap.Now, "s": "siteX", "v": "轮询内容"},
		},
	}
	rec = callJSON(t, mux, "POST", "/api/collab", body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"changed":1`) {
		t.Fatalf("collab merge: %d %s", rec.Code, rec.Body.String())
	}
	// 旧时间戳再来一次不生效
	if rec := callJSON(t, mux, "POST", "/api/collab", body); !strings.Contains(rec.Body.String(), `"changed":0`) {
		t.Fatalf("重复提交应幂等: %s", rec.Body.String())
	}
	// 缺少 site 被拒绝
	if rec := callJSON(t, mux, "POST", "/api/collab", map[string]any{"product": "产品E", "script": "脚本E"}); rec.Code != 400 {
		t.Fatalf("缺少 site 应 400: %d", rec.Code)
	}
	// 产品不存在
	rec = call(t, mux, "GET", "/api/collab?product=nope&script=nope", nil, "")
	if rec.Code != 404 {
		t.Fatalf("不存在应 404: %d", rec.Code)
	}

	// 旧的整体保存接口在协作模式下也能合并，并通知客户端
	doc := mustDoc(t, filepath.Join(app.dataDir, "产品E", "脚本E", docFileName))
	doc.Shots[0].Content = "旧接口覆盖"
	if rec := callJSON(t, mux, "PUT", "/api/doc"+q, doc); rec.Code != 200 {
		t.Fatalf("legacy put: %d %s", rec.Code, rec.Body.String())
	}
	doc2 := mustDoc(t, filepath.Join(app.dataDir, "产品E", "脚本E", docFileName))
	if doc2.Shots[0].Content != "旧接口覆盖" {
		t.Fatalf("旧接口未生效: %+v", doc2.Shots[0])
	}
}

/* ---------------- 测试辅助 ---------------- */

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "读取失败: " + err.Error()
	}
	return string(b)
}

func mustDoc(t *testing.T, path string) Doc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	var doc Doc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	return doc
}
