package main

import (
	"bufio"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

// App 保存服务运行期所需的全局信息。
type App struct {
	dataDir      string
	settingsPath string

	envMu      sync.Mutex
	settings   Settings
	envCached  bool
	envAt      time.Time
	envFFmpeg  ToolInfo
	envFFprobe ToolInfo

	jobsMu sync.Mutex
	jobs   map[string]*SplitJob

	// 阶段三：每份脚本一个协作房间，按脚本目录去重
	hubMu sync.Mutex
	rooms map[string]*Room

	// 脚本的「使用 / 未使用」状态，按 产品/脚本 记录在数据目录的 _脚本状态.json
	statusMu   sync.Mutex
	statusPath string
	status     statusFile
}

func newApp(dataDir string) *App {
	settingsPath := filepath.Join(dataDir, settingsFileName)
	statusPath := filepath.Join(dataDir, statusFileName)
	return &App{
		dataDir:      dataDir,
		settingsPath: settingsPath,
		settings:     readSettings(settingsPath),
		jobs:         make(map[string]*SplitJob),
		rooms:        make(map[string]*Room),
		statusPath:   statusPath,
		status:       readStatus(statusPath),
	}
}

func main() {
	// 先定 umask，再建任何目录：保证新建目录 0775、文件 0664
	applyUmask()
	addr := flag.String("addr", envOr("SM_ADDR", ":8686"), "监听地址，例如 :8686")
	dataFlag := flag.String("data", envOr("SM_DATA", ""), "数据目录，默认为程序旁边的「脚本管理」")
	openFlag := flag.Bool("open", envOr("SM_NO_OPEN", "") == "", "启动后自动打开浏览器（Windows / macOS）")
	flag.Parse()

	dataDir, err := resolveDataDir(*dataFlag)
	if err != nil {
		log.Fatalf("无法确定数据目录: %v", err)
	}
	if err := ensureDir(dataDir); err != nil {
		log.Fatalf("无法创建数据目录 %s: %v", dataDir, err)
	}
	// 老版本可能把目录/文件建成 0 权限（宿主机上看起来是空的），启动时顺手修正
	if dirs, files := fixZeroPerms(dataDir); dirs+files > 0 {
		fmt.Printf("  权限修正: %d 个目录、%d 个文件原权限为 0，已恢复为 0775 / 0664\n", dirs, files)
	}

	app := newApp(dataDir)
	mux := http.NewServeMux()
	app.routes(mux)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("端口监听失败（%s）: %v", *addr, err)
	}
	port := 8686
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		port = tcp.Port
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	fmt.Println("脚本管理 已启动")
	fmt.Println("  本机访问: " + url)
	for _, ip := range lanIPs() {
		fmt.Printf("  局域网:   http://%s:%d\n", ip, port)
	}
	fmt.Println("  数据目录: " + dataDir)
	if ff, fp := app.envTools(false); ff.OK && fp.OK {
		fmt.Println("  ffmpeg:   " + ff.Path)
	} else {
		fmt.Println("  ffmpeg:   未找到（智能裁切不可用，可在页面右上角手动指定）")
	}
	fmt.Println("  关闭窗口或按 Ctrl+C 停止服务")

	// 收到 Ctrl+C / docker stop 时，把协作状态落盘再退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-quit
		app.flushAllRooms()
		os.Exit(0)
	}()

	if *openFlag && runtime.GOOS != "linux" {
		go func() {
			time.Sleep(250 * time.Millisecond)
			openBrowser(url)
		}()
	}

	server := &http.Server{
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 15 * time.Second,
	}
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/tree", a.handleTree)
	mux.HandleFunc("POST /api/product", a.handleCreateProduct)
	mux.HandleFunc("POST /api/script", a.handleCreateScript)
	mux.HandleFunc("POST /api/script/rename", a.handleRenameScript)
	mux.HandleFunc("POST /api/script/delete", a.handleDeleteScript)
	mux.HandleFunc("POST /api/usage", a.handleSetUsage)
	mux.HandleFunc("GET /api/search", a.handleSearch)
	mux.HandleFunc("GET /api/history", a.handleHistory)
	mux.HandleFunc("POST /api/history/rollback", a.handleRollback)
	mux.HandleFunc("GET /api/export/precheck", a.handleExportPrecheck)
	mux.HandleFunc("POST /api/export/draft", a.handleExportDraft)
	mux.HandleFunc("GET /api/export/fallback", a.handleExportFallback)
	mux.HandleFunc("GET /api/doc", a.handleGetDoc)
	mux.HandleFunc("PUT /api/doc", a.handlePutDoc)
	mux.HandleFunc("POST /api/doc", a.handlePutDoc) // sendBeacon 兜底保存
	mux.HandleFunc("GET /api/env", a.handleEnv)
	mux.HandleFunc("POST /api/env", a.handleSetEnv)
	mux.HandleFunc("POST /api/probe", a.handleProbe)
	mux.HandleFunc("POST /api/split", a.handleSplit)
	mux.HandleFunc("GET /api/split/{id}", a.handleSplitStatus)
	mux.HandleFunc("POST /api/clip", a.handleClip)
	mux.HandleFunc("POST /api/upload", a.handleUpload)
	mux.HandleFunc("POST /api/temp", a.handleTempUpload)
	mux.HandleFunc("GET /api/collab", a.handleCollabState)
	mux.HandleFunc("POST /api/collab", a.handleCollabMerge)
	mux.HandleFunc("GET /api/collab/ws", a.handleCollabWS)
	mux.HandleFunc("GET /media/_temp/{file}", a.handleTempMedia)
	mux.HandleFunc("GET /media/{product}/{script}/{file}", a.handleScriptMedia)

	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("内置页面缺失: %v", err)
	}
	mux.Handle("GET /", noCache(http.FileServer(http.FS(webRoot))))
}

func resolveDataDir(flagVal string) (string, error) {
	if strings.TrimSpace(flagVal) != "" {
		return filepath.Abs(flagVal)
	}
	exe, err := os.Executable()
	if err != nil {
		return filepath.Abs("脚本管理")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "脚本管理"), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func lanIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				out = append(out, ip4.String())
			}
		}
	}
	return out
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack 让 WebSocket 握手能接管底层连接（包装器必须把该方法透传出去）。
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("底层连接不支持 Hijack")
	}
	return hj.Hijack()
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		if !strings.HasPrefix(r.URL.Path, "/media/") {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
		}
	})
}
