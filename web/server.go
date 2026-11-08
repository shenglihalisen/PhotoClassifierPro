// Package web 内嵌的浏览器界面与 HTTP API。
//
// 只监听 127.0.0.1（不是 0.0.0.0）：这是个本地工具，不提供服务给局域网，
// 也就不需要账号体系。要给别人用得自己套反代 + 认证。
package web

// server.go HTTP 路由。

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shenglihalisen/PhotoClassifierPro/engine"
	"github.com/shenglihalisen/PhotoClassifierPro/qc"
)

//go:embed static
var staticFS embed.FS

// Config 服务参数。
type Config struct {
	Addr        string
	Version     string
	OpenBrowser bool
	// Quiet 不打印启动信息（GUI 模式没有控制台）。
	Quiet bool
}

// state 当前会话状态。单用户本地工具，一个进程只服务一个扫描根。
type state struct {
	mu       sync.RWMutex
	root     string
	lastRes  *engine.Result
	faceOn   bool
	th       qc.Thresholds
	scanning bool
}

var st = &state{th: qc.DefaultThresholds()}

// Serve 启动 HTTP 服务（阻塞）。自己监听 cfg.Addr。
func Serve(cfg Config) error {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("端口被占用或无法监听 %s: %w", cfg.Addr, err)
	}
	return ServeListener(ln, cfg)
}

// ServeListener 在已就绪的 listener 上提供服务（阻塞）。
//
// 单独拆出来是给 GUI 用的：窗口模式希望先拿到真实端口（:0 随机分配），
// 再把地址交给 WebView 导航，避免和别的进程抢 8080。
func ServeListener(ln net.Listener, cfg Config) error {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/scan", handleScan)
	mux.HandleFunc("/api/thumb", handleThumb)
	mux.HandleFunc("/api/full", handleFull)
	mux.HandleFunc("/api/thresholds", handleThresholds)
	mux.HandleFunc("/api/organize", handleOrganize)
	mux.HandleFunc("/api/export.csv", handleCSV)
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"version": cfg.Version, "face": qc.DefaultThresholds().BlinkEdge})
	})

	url := "http://" + ln.Addr().String()
	if !cfg.Quiet {
		fmt.Printf("PhotoClassifierPro %s\n界面地址: %s\n按 Ctrl+C 退出\n", cfg.Version, url)
	}
	if cfg.OpenBrowser {
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(url)
		}()
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// ─────────────────────── 扫描（SSE） ───────────────────────

func handleScan(w http.ResponseWriter, r *http.Request) {
	root := strings.TrimSpace(r.URL.Query().Get("root"))
	if root == "" {
		http.Error(w, "缺少 root 参数", http.StatusBadRequest)
		return
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		http.Error(w, "路径无效", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		http.Error(w, "不是有效目录", http.StatusBadRequest)
		return
	}
	faceOn := r.URL.Query().Get("face") != "0"

	st.mu.Lock()
	if st.scanning {
		st.mu.Unlock()
		http.Error(w, "已有扫描在进行中", http.StatusConflict)
		return
	}
	st.scanning = true
	st.root = abs
	st.faceOn = faceOn
	th := st.th
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		st.scanning = false
		st.mu.Unlock()
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "服务器不支持流式输出", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // 给 nginx 反代用

	send := func(event string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	engine.ResetCache()
	res, err := engine.Scan(ctx, engine.Options{
		Root:       abs,
		Recursive:  true,
		Face:       faceOn,
		Thresholds: th,
	}, func(p engine.Progress) {
		_ = send("progress", p)
	})
	if err != nil {
		_ = send("failed", map[string]string{"error": err.Error()})
		return
	}
	st.mu.Lock()
	st.lastRes = res
	st.mu.Unlock()
	_ = send("done", map[string]any{
		"summary": res.Summary,
		"items":   res.Items,
		"groups":  res.Groups,
		"root":    abs,
	})
}

// ─────────────────────── 图片 ───────────────────────

// safePath 把用户传来的路径限制在当前扫描根内。
func safePath(p string) (string, bool) {
	st.mu.RLock()
	root := st.root
	st.mu.RUnlock()
	if root == "" || p == "" {
		return "", false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", false
	}
	return abs, true
}

func handleThumb(w http.ResponseWriter, r *http.Request) {
	p, ok := safePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "路径不在扫描范围内", http.StatusForbidden)
		return
	}
	size := 320
	if v, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil && v > 0 && v <= 1024 {
		size = v
	}
	img, err := qc.DecodeFile(p)
	if err != nil {
		http.Error(w, "无法解码", http.StatusUnsupportedMediaType)
		return
	}
	thumb := shrink(img, size)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_ = jpeg.Encode(w, thumb, &jpeg.Options{Quality: 82})
}

func handleFull(w http.ResponseWriter, r *http.Request) {
	p, ok := safePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "路径不在扫描范围内", http.StatusForbidden)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "打不开", http.StatusNotFound)
		return
	}
	defer f.Close()
	if stat, err := f.Stat(); err == nil {
		http.ServeContent(w, r, filepath.Base(p), stat.ModTime(), f)
		return
	}
	http.Error(w, "读文件失败", http.StatusInternalServerError)
}

// shrink 等比缩到长边 maxPx（不放大）。
func shrink(src image.Image, maxPx int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	long := w
	if h > long {
		long = h
	}
	if long <= maxPx {
		return src
	}
	s := float64(maxPx) / float64(long)
	dw, dh := int(float64(w)*s+0.5), int(float64(h)*s+0.5)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// 盒式平均，快的缩法够用了
	for y := 0; y < dh; y++ {
		y0, y1 := y*h/dh, (y+1)*h/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0, x1 := x*w/dw, (x+1)*w/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, cnt int
			for yy := y0; yy < y1 && yy < h; yy++ {
				for xx := x0; xx < x1 && xx < w; xx++ {
					cr, cg, cb, _ := src.At(b.Min.X+xx, b.Min.Y+yy).RGBA()
					r += int(cr >> 8)
					g += int(cg >> 8)
					bl += int(cb >> 8)
					cnt++
				}
			}
			if cnt == 0 {
				cnt = 1
			}
			dst.Set(x, y, color.RGBA{uint8(r / cnt), uint8(g / cnt), uint8(bl / cnt), 255})
		}
	}
	return dst
}

// ─────────────────────── 阈值 ───────────────────────

func handleThresholds(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		st.mu.RLock()
		th := st.th
		st.mu.RUnlock()
		writeJSON(w, map[string]any{
			"current": th,
			"default": qc.DefaultThresholds(),
			"specs":   qc.ThresholdSpecs(),
			"version": qc.VersionFor(th),
		})
	case http.MethodPost:
		var in qc.Thresholds
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			http.Error(w, "参数解析失败", http.StatusBadRequest)
			return
		}
		th := qc.ClampThresholds(in)
		st.mu.Lock()
		st.th = th
		st.mu.Unlock()
		writeJSON(w, map[string]any{"current": th, "version": qc.VersionFor(th)})
	default:
		http.Error(w, "不支持的方法", http.StatusMethodNotAllowed)
	}
}

// ─────────────────────── 归档 ───────────────────────

func handleOrganize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "不支持的方法", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Mode string `json:"mode"` // "move" / "copy"
		Dry  bool   `json:"dry"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "参数解析失败", http.StatusBadRequest)
		return
	}
	st.mu.RLock()
	res, root := st.lastRes, st.root
	st.mu.RUnlock()
	if res == nil || root == "" {
		http.Error(w, "还没有扫描结果", http.StatusConflict)
		return
	}
	mode := engine.ModeMove
	if req.Mode == "copy" {
		mode = engine.ModeCopy
	}
	actions := engine.Plan(res.Items, engine.OrganizeOptions{Root: root, Mode: mode})
	if req.Dry {
		writeJSON(w, map[string]any{"actions": actions, "dry": true})
		return
	}
	done := engine.Execute(actions, mode)
	writeJSON(w, map[string]any{"done": done, "total": len(actions), "actions": actions})
}

func handleCSV(w http.ResponseWriter, r *http.Request) {
	st.mu.RLock()
	res := st.lastRes
	st.mu.RUnlock()
	if res == nil {
		http.Error(w, "还没有扫描结果", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="qc-report.csv"`)
	_, _ = w.Write([]byte("\xEF\xBB\xBF"))
	_, _ = w.Write([]byte("文件,大小KB,问题,重复组,拉普拉斯标准差,噪声底线,亮度均值,近黑占比,近白占比,人脸数,闭眼数,宽,高,错误\n"))
	for _, it := range res.Items {
		if it == nil || it.Result == nil {
			continue
		}
		labels := strings.Join(it.Result.Labels(), " ")
		if labels == "" {
			labels = "正常"
		}
		faces, closed := 0, 0
		if it.Result.Blink != nil {
			faces, closed = it.Result.Blink.Faces, it.Result.Blink.Closed
		}
		fmt.Fprintf(w, "%q,%d,%q,%d,%.2f,%.2f,%.1f,%.3f,%.3f,%d,%d,%d,%d,%q\n",
			it.Rel, it.Size/1024, labels, it.DupGroup,
			it.Result.LapStd, it.Result.NoiseFloor, it.Result.ExpMean,
			it.Result.ExpClipLow, it.Result.ExpClipHi, faces, closed,
			it.Result.Width, it.Result.Height, it.Result.Error)
	}
}

// ─────────────────────── 杂项 ───────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
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
