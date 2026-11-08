// Package web 内嵌的浏览器界面与 HTTP API。
//
// 安全模型分两种模式，靠监听地址自动切换：
//
//   - 本机模式（127.0.0.1 / ::1）：不加认证。浏览器里的页面本来就是本机进程自己开的，
//     再加口令只是给使用者添堵。
//   - 对外模式（0.0.0.0 或任何非回环地址，容器里就是这种）：**强制口令**。
//     口令来自 -pass 或环境变量 PCP_PASS，都没给就启动时随机生成并打到 stdout
//     （docker logs 里能看到）。所有 /api/* 都要带会话 Cookie。
//
// 另外两条硬约束，跟认证无关但同样重要：
//   - /api/scan 的 root 在容器模式下被锁死在挂载目录内，不许扫宿主机的任意路径；
//   - /api/full、/api/thumb 只服务图片扩展名，防止把扫描根里的其他文件读出去。
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
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

const sessionCookie = "pcp_session"

// Config 服务参数。
type Config struct {
	Addr        string
	Version     string
	OpenBrowser bool
	// Quiet 不打印启动信息（GUI 模式没有控制台）。
	Quiet bool
	// BaseDir 非空时，/api/scan 的 root 必须在这个目录内（容器里传 /data）。
	BaseDir string
	// Pass 对外模式的口令。为空时读 PCP_PASS，仍为空则随机生成。
	Pass string
}

// state 当前会话状态。单用户工具，一个进程只服务一个扫描根。
type state struct {
	mu       sync.RWMutex
	root     string
	lastRes  *engine.Result
	faceOn   bool
	th       qc.Thresholds
	scanning bool

	auth    bool   // 是否需要口令
	pass    string // 口令（明文保存，仅用于进程内比较）
	baseDir string // 允许扫描的根
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

	auth := needsAuth(ln.Addr().String())
	pass := cfg.Pass
	if pass == "" {
		pass = os.Getenv("PCP_PASS")
	}
	if auth && pass == "" {
		pass = randomToken()
		if !cfg.Quiet {
			fmt.Printf("⚠️  监听了非回环地址，已启用口令认证。\n    本次自动生成的口令：%s\n    也可用 -pass 或环境变量 PCP_PASS 指定。\n", pass)
		}
	}
	st.mu.Lock()
	st.auth, st.pass, st.baseDir = auth, pass, cfg.BaseDir
	st.mu.Unlock()

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"version": cfg.Version,
			"auth":    auth,
			"authed":  !auth || hasSession(r),
		})
	})
	mux.HandleFunc("/api/login", handleLogin)

	// 其余接口统一过认证中间件；本地模式下中间件直接放行。
	mux.HandleFunc("/api/scan", guard(handleScan))
	mux.HandleFunc("/api/thumb", guard(handleThumb))
	mux.HandleFunc("/api/full", guard(handleFull))
	mux.HandleFunc("/api/thresholds", guard(handleThresholds))
	mux.HandleFunc("/api/organize", guard(handleOrganize))
	mux.HandleFunc("/api/export.csv", guard(handleCSV))

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

// ─────────────────────── 认证 ───────────────────────

// needsAuth 非回环监听一律要口令。注意 "0.0.0.0" 本身不是回环。
func needsAuth(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return !ip.IsLoopback()
}

func randomToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// 加密随机源不可用时退回时间戳，总比空口令强
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func hasSession(r *http.Request) bool {
	st.mu.RLock()
	auth, pass := st.auth, st.pass
	st.mu.RUnlock()
	if !auth {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(pass)) == 1
}

// guard 包一层认证。401 而不是重定向：前端拿到 401 自己弹登录框，
// 不用刷新页面，也不会打断正在进行的扫描。
func guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hasSession(r) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "需要口令", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "不支持的方法", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "参数解析失败", http.StatusBadRequest)
		return
	}
	st.mu.RLock()
	auth, pass := st.auth, st.pass
	st.mu.RUnlock()
	if !auth {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Pass), []byte(pass)) != 1 {
		http.Error(w, "口令不对", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    pass,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})
	writeJSON(w, map[string]any{"ok": true})
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
	if !rootAllowed(abs) {
		http.Error(w, "只能扫描允许的目录", http.StatusForbidden)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		http.Error(w, "不是有效目录", http.StatusBadRequest)
		return
	}
	faceOn := r.URL.Query().Get("face") != "0"
	globalDup := r.URL.Query().Get("global") == "1"

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
		GlobalDup:  globalDup,
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
		"summary":   res.Summary,
		"items":     res.Items,
		"groups":    res.Groups,
		"crossDups": res.CrossDups,
		"root":      abs,
	})
}

// rootAllowed 容器模式下把扫描范围锁在挂载目录内。
func rootAllowed(abs string) bool {
	st.mu.RLock()
	base := st.baseDir
	st.mu.RUnlock()
	if base == "" {
		return true // 本机工具，扫哪里由使用者决定
	}
	b, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(b, abs)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ─────────────────────── 图片 ───────────────────────

// safePath 把用户传来的路径限制在当前扫描根内，**并且**只允许图片扩展名。
//
// 加第二条是因为 /api/full 会直接把文件内容发出去：只校验"在根内"的话，
// 知道路径就能把同目录下的任意文件（配置、密钥、数据库）读出来。
func safePath(p string) (string, bool) {
	st.mu.RLock()
	root := st.root
	st.mu.RUnlock()
	if root == "" || p == "" {
		return "", false
	}
	if !qc.SupportedExts[strings.ToLower(filepath.Ext(p))] {
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

// ── 缩略图缓存 ──
//
// 网格里一张图可能被反复请求（切筛选、翻页来回、开预览再关），
// 每次都重新解码 + 缩放太浪费。用 path|size|mtime 做键缓存编码后的 JPEG。
type cacheItem struct {
	data []byte
	used int64
}

var thumbCache = struct {
	mu    sync.Mutex
	m     map[string]cacheItem
	order []string
	seq   int64
}{m: make(map[string]cacheItem)}

const thumbCacheMax = 400

func cacheGet(key string) ([]byte, bool) {
	thumbCache.mu.Lock()
	defer thumbCache.mu.Unlock()
	it, ok := thumbCache.m[key]
	if !ok {
		return nil, false
	}
	thumbCache.seq++
	it.used = thumbCache.seq
	thumbCache.m[key] = it
	return it.data, true
}

func cachePut(key string, data []byte) {
	thumbCache.mu.Lock()
	defer thumbCache.mu.Unlock()
	if len(thumbCache.m) >= thumbCacheMax {
		// 满了就整体清掉：实现简单，且扫描换目录后旧缓存本来也该失效，
		// 精确的 LRU 在这个量级下不值得多写 50 行。
		thumbCache.m = make(map[string]cacheItem)
		thumbCache.order = nil
	}
	thumbCache.seq++
	thumbCache.m[key] = cacheItem{data: data, used: thumbCache.seq}
}

func handleThumb(w http.ResponseWriter, r *http.Request) {
	p, ok := safePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "路径不在扫描范围内或不是图片", http.StatusForbidden)
		return
	}
	size := 320
	if v, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil && v > 0 && v <= 2048 {
		size = v
	}
	info, err := os.Stat(p)
	if err != nil {
		http.Error(w, "打不开", http.StatusNotFound)
		return
	}
	key := fmt.Sprintf("%s|%d|%d|%d", p, size, info.Size(), info.ModTime().UnixNano())
	if data, hit := cacheGet(key); hit {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Header().Set("X-Cache", "HIT")
		_, _ = w.Write(data)
		return
	}
	img, err := qc.DecodeFile(p)
	if err != nil {
		http.Error(w, "无法解码", http.StatusUnsupportedMediaType)
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, shrink(img, size), &jpeg.Options{Quality: 82}); err != nil {
		http.Error(w, "缩略图编码失败", http.StatusInternalServerError)
		return
	}
	cachePut(key, buf.Bytes())
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(buf.Bytes())
}

// handleFull 预览用原图；带 w/h 参数时按视口等比缩放成 JPEG。
//
// 相机一张 20MB 的 JPEG 全量发到前端只为了在 1000px 的窗口里显示，
// 既慢又占内存。前端会带上视口尺寸，这里按需缩，缩放结果同样进缓存。
func handleFull(w http.ResponseWriter, r *http.Request) {
	p, ok := safePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "路径不在扫描范围内或不是图片", http.StatusForbidden)
		return
	}
	wpx, _ := strconv.Atoi(r.URL.Query().Get("w"))
	hpx, _ := strconv.Atoi(r.URL.Query().Get("h"))
	long := 0
	if wpx > hpx {
		long = wpx
	} else {
		long = hpx
	}
	if long > 4096 {
		long = 4096
	}
	if long < 256 {
		long = 0 // 没给有效尺寸：发原图
	}

	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "打不开", http.StatusNotFound)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "读文件失败", http.StatusInternalServerError)
		return
	}

	if long == 0 {
		http.ServeContent(w, r, filepath.Base(p), stat.ModTime(), f)
		return
	}

	key := fmt.Sprintf("full|%s|%d|%d|%d", p, long, stat.Size(), stat.ModTime().UnixNano())
	if data, hit := cacheGet(key); hit {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=3600")
		_, _ = w.Write(data)
		return
	}
	img, err := qc.DecodeFile(p)
	if err != nil {
		http.Error(w, "无法解码", http.StatusUnsupportedMediaType)
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, shrink(img, long), &jpeg.Options{Quality: 88}); err != nil {
		http.Error(w, "编码失败", http.StatusInternalServerError)
		return
	}
	cachePut(key, buf.Bytes())
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(buf.Bytes())
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
	_, _ = w.Write([]byte("文件,大小KB,问题,重复组,跨目录重复,拉普拉斯标准差,噪声底线,亮度均值,近黑占比,近白占比,人脸数,闭眼数,宽,高,错误\n"))
	crossMap := map[string]string{}
	for _, c := range res.CrossDups {
		crossMap[c.Current] = c.Other
	}
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
		fmt.Fprintf(w, "%q,%d,%q,%d,%q,%.2f,%.2f,%.1f,%.3f,%.3f,%d,%d,%d,%d,%q\n",
			it.Rel, it.Size/1024, labels, it.DupGroup, crossMap[it.Rel],
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
