package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestApp(t *testing.T) (*App, http.Handler) {
	t.Helper()
	dir := "tmp-test-data"
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	app := newApp(dir)
	mux := http.NewServeMux()
	app.routes(mux)
	return app, mux
}

func call(t *testing.T, h http.Handler, method, target string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func callJSON(t *testing.T, h http.Handler, method, target string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = b
	}
	return call(t, h, method, target, body, "application/json")
}

func mpBody(t *testing.T, filename string, content []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

func TestStageOneAPI(t *testing.T) {
	app, mux := newTestApp(t)

	// 空目录
	rec := call(t, mux, "GET", "/api/tree", nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"products":[]`) {
		t.Fatalf("empty tree: %d %s", rec.Code, rec.Body.String())
	}

	// 非法名称
	rec = callJSON(t, mux, "POST", "/api/product", map[string]string{"name": "../evil"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal name should be rejected, got %d", rec.Code)
	}

	// 建产品 / 脚本
	rec = callJSON(t, mux, "POST", "/api/product", map[string]string{"name": "产品A"})
	if rec.Code != 200 {
		t.Fatalf("create product: %d %s", rec.Code, rec.Body.String())
	}
	rec = callJSON(t, mux, "POST", "/api/script", map[string]string{"product": "产品A", "name": "脚本一"})
	if rec.Code != 200 {
		t.Fatalf("create script: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(app.dataDir, "产品A", "脚本一", docFileName)); err != nil {
		t.Fatalf("文案.json missing: %v", err)
	}

	// 树
	rec = call(t, mux, "GET", "/api/tree", nil, "")
	if !strings.Contains(rec.Body.String(), `"name":"产品A"`) || !strings.Contains(rec.Body.String(), `"脚本一"`) {
		t.Fatalf("tree content: %s", rec.Body.String())
	}

	// 读文案
	q := "?product=" + url.QueryEscape("产品A") + "&script=" + url.QueryEscape("脚本一")
	rec = call(t, mux, "GET", "/api/doc"+q, nil, "")
	if rec.Code != 200 {
		t.Fatalf("get doc: %d %s", rec.Code, rec.Body.String())
	}
	var doc Doc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Shots) != 1 || doc.Shots[0].No != "1" {
		t.Fatalf("default doc: %+v", doc)
	}

	// 保存两行
	doc.Shots = append(doc.Shots, Shot{ID: "s2", No: "2", Content: "特写", Narration: "口播二", Copy: "文案二"})
	rec = callJSON(t, mux, "PUT", "/api/doc"+q, doc)
	if rec.Code != 200 {
		t.Fatalf("put doc: %d %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(filepath.Join(app.dataDir, "产品A", "脚本一", docFileName))
	if err != nil {
		t.Fatal(err)
	}
	var reread Doc
	if err := json.Unmarshal(saved, &reread); err != nil {
		t.Fatal(err)
	}
	if len(reread.Shots) != 2 || reread.Shots[1].Narration != "口播二" || reread.UpdatedAt == "" {
		t.Fatalf("saved doc: %+v", reread)
	}

	// 上传视频，按镜头序号命名
	payload, ctype := mpBody(t, "原文件名.mp4", []byte("fake-video-bytes"))
	uploadURL := "/api/upload?product=" + url.QueryEscape("产品A") + "&script=" + url.QueryEscape("脚本一") + "&shot=2"
	rec = call(t, mux, "POST", uploadURL, payload, ctype)
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"file":"2.mp4"`) {
		t.Fatalf("upload response: %s", rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(app.dataDir, "产品A", "脚本一", "2.mp4"))
	if err != nil || string(got) != "fake-video-bytes" {
		t.Fatalf("uploaded file: %v %q", err, got)
	}

	// 视频 Range 预览
	media := "/media/" + url.PathEscape("产品A") + "/" + url.PathEscape("脚本一") + "/2.mp4"
	req := httptest.NewRequest("GET", media, nil)
	req.Header.Set("Range", "bytes=0-3")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "fake" {
		t.Fatalf("range media: %d %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Range"), "0-3/16") {
		t.Fatalf("content-range: %s", rec.Header().Get("Content-Range"))
	}

	// 临时素材
	payload, ctype = mpBody(t, "source.mov", []byte("temp-source"))
	rec = call(t, mux, "POST", "/api/temp", payload, ctype)
	if rec.Code != 200 {
		t.Fatalf("temp upload: %d %s", rec.Code, rec.Body.String())
	}
	var tempResp struct{ File string }
	_ = json.Unmarshal(rec.Body.Bytes(), &tempResp)
	if !strings.HasSuffix(tempResp.File, ".mov") {
		t.Fatalf("temp file: %s", tempResp.File)
	}
	rec = call(t, mux, "GET", "/media/_temp/"+url.PathEscape(tempResp.File), nil, "")
	if rec.Code != 200 || rec.Body.String() != "temp-source" {
		t.Fatalf("temp media: %d %q", rec.Code, rec.Body.String())
	}

	// 内嵌前端
	rec = call(t, mux, "GET", "/", nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "脚本管理") {
		t.Fatalf("index: %d", rec.Code)
	}
	rec = call(t, mux, "GET", "/app.js", nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "markCut") {
		t.Fatalf("app.js: %d", rec.Code)
	}

	// 不存在
	rec = call(t, mux, "GET", "/api/doc?product=%E4%B8%8D%E5%AD%98%E5%9C%A8&script=x", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing doc: %d", rec.Code)
	}
	rec = call(t, mux, "GET", "/api/health", nil, "")
	if rec.Code != 200 {
		t.Fatalf("health: %d", rec.Code)
	}
}

func TestStageTwoParsers(t *testing.T) {
	if fps := parseRate("30000/1001"); fps < 29.96 || fps > 29.98 {
		t.Fatalf("parseRate 30000/1001 = %v", fps)
	}
	if fps := parseRate("25"); fps != 25 {
		t.Fatalf("parseRate 25 = %v", fps)
	}
	if fps := parseRate("0/0"); fps != 0 {
		t.Fatalf("parseRate 0/0 = %v", fps)
	}
	if v, ok := parseTimecode("00:00:04.004"); !ok || v < 4.0 || v > 4.01 {
		t.Fatalf("parseTimecode = %v %v", v, ok)
	}
	if v, ok := parseProgressLine("out_time=00:01:02.500000"); !ok || v != 62.5 {
		t.Fatalf("parseProgressLine = %v %v", v, ok)
	}
	if v, ok := parseSceneLine("[Parsed_metadata_1 @ 0x1] lavfi.scd.time=4.004"); !ok || v != 4.004 {
		t.Fatalf("parseSceneLine = %v %v", v, ok)
	}
	if _, ok := parseSceneLine("lavfi.scd.score: 75.0, lavfi.scd.time: 4.004"); ok {
		t.Fatal("score 行不应被当成切点")
	}
	if v, ok := parsePtsLine("[Parsed_showinfo_1 @ 0x1] n: 12 pts: 7680 pts_time:4.004 flags:K"); !ok || v != 4.004 {
		t.Fatalf("parsePtsLine = %v %v", v, ok)
	}

	segs := buildSegments([]float64{2, 4}, 6)
	if len(segs) != 3 || segs[0].Start != 0 || segs[0].End != 2 || segs[2].End != 6 {
		t.Fatalf("buildSegments: %+v", segs)
	}
	if one := buildSegments(nil, 6); len(one) != 1 || one[0].End != 6 {
		t.Fatalf("buildSegments no cuts: %+v", one)
	}
	// 太近的切点被丢弃
	if merged := buildSegments([]float64{2, 2.2}, 6); len(merged) != 2 {
		t.Fatalf("buildSegments merged: %+v", merged)
	}
}

func TestStageTwoEditAndEnv(t *testing.T) {
	app, mux := newTestApp(t)

	rec := callJSON(t, mux, "POST", "/api/product", map[string]string{"name": "产品B"})
	if rec.Code != 200 {
		t.Fatalf("create product: %d", rec.Code)
	}
	rec = callJSON(t, mux, "POST", "/api/script", map[string]string{"product": "产品B", "name": "脚本B"})
	if rec.Code != 200 {
		t.Fatalf("create script: %d", rec.Code)
	}
	q := "?product=" + url.QueryEscape("产品B") + "&script=" + url.QueryEscape("脚本B")
	rec = call(t, mux, "GET", "/api/doc"+q, nil, "")
	if rec.Code != 200 {
		t.Fatalf("get doc: %d", rec.Code)
	}
	var doc Doc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Edit = &EditState{
		Source:      "20260101-abcdef.mp4",
		SourceName:  "原片.mp4",
		FPS:         25,
		Duration:    6,
		Cuts:        []float64{2, 4},
		CurrentTime: 3.5,
	}
	rec = callJSON(t, mux, "PUT", "/api/doc"+q, doc)
	if rec.Code != 200 {
		t.Fatalf("put doc edit: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(t, mux, "GET", "/api/doc"+q, nil, "")
	var reread Doc
	if err := json.Unmarshal(rec.Body.Bytes(), &reread); err != nil {
		t.Fatal(err)
	}
	if reread.Edit == nil || reread.Edit.Source != "20260101-abcdef.mp4" ||
		len(reread.Edit.Cuts) != 2 || reread.Edit.CurrentTime != 3.5 || reread.Edit.FPS != 25 {
		t.Fatalf("edit not persisted: %+v", reread.Edit)
	}

	// 环境接口
	rec = call(t, mux, "GET", "/api/env", nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ffmpeg"`) {
		t.Fatalf("env: %d %s", rec.Code, rec.Body.String())
	}
	// 指定不存在的路径应被拒绝
	rec = callJSON(t, mux, "POST", "/api/env", map[string]string{"ffmpeg": filepath.Join(app.dataDir, "nope", "ffmpeg.exe")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad env path: %d %s", rec.Code, rec.Body.String())
	}
	// 指定存在的路径会保存到 设置.json
	fake := filepath.Join(app.dataDir, "fake-ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec = callJSON(t, mux, "POST", "/api/env", map[string]string{"ffmpeg": fake})
	if rec.Code != 200 {
		t.Fatalf("set env: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(app.dataDir, settingsFileName)); err != nil {
		t.Fatalf("设置.json missing: %v", err)
	}

	// 没有 ffmpeg 时切片接口给出 503 与明确提示。
	// 本机可能装了 ffmpeg，先把所有查找途径清空，模拟干净机器。
	t.Setenv("PATH", "")
	t.Setenv("SM_FFMPEG", "")
	t.Setenv("SM_FFPROBE", "")
	t.Setenv("LOCALAPPDATA", t.TempDir())
	app.envMu.Lock()
	app.envCached = false
	app.envMu.Unlock()
	if err := os.MkdirAll(filepath.Join(app.dataDir, tempDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.dataDir, tempDirName, "src.mp4"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = callJSON(t, mux, "POST", "/api/clip", map[string]any{
		"product": "产品B", "script": "脚本B", "shot": "2",
		"src": "temp", "file": "src.mp4", "in": 0, "out": 1,
	})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "ffmpeg") {
		t.Fatalf("clip without ffmpeg: %d %s", rec.Code, rec.Body.String())
	}
	// 不存在的临时素材
	rec = callJSON(t, mux, "POST", "/api/probe", map[string]string{"src": "temp", "file": "missing.mp4"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("probe missing: %d", rec.Code)
	}
	// 非法镜头序号
	rec = callJSON(t, mux, "POST", "/api/clip", map[string]any{
		"product": "产品B", "script": "脚本B", "shot": "../x",
		"src": "temp", "file": "src.mp4", "in": 0, "out": 1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clip bad shot: %d %s", rec.Code, rec.Body.String())
	}

	// bin 是放置 ffmpeg 的保留目录，既不能建产品，也不出现在产品列表里
	rec = callJSON(t, mux, "POST", "/api/product", map[string]string{"name": "bin"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create bin product: %d", rec.Code)
	}
	if err := os.MkdirAll(filepath.Join(app.dataDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec = call(t, mux, "GET", "/api/tree", nil, "")
	if strings.Contains(rec.Body.String(), `"name":"bin"`) {
		t.Fatalf("bin 不应出现在产品列表: %s", rec.Body.String())
	}
}
