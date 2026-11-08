//go:build windows

// gui_windows.go 原生窗口（WebView2）。
//
// 为什么是 WebView2 而不是 Wails 或自绘 Win32：
//   - WebView2 绑定是纯 Go（不经 cgo），CGO_ENABLED=0 的单文件前提保住了；
//   - 界面已经是一套完整的 HTML/CSS/JS，嵌进原生窗口就行，不用再画一遍 GDI；
//   - Win10/11 随 Edge 预装运行时，绝大多数机器开箱即用。
//
// 没有运行时（或初始化失败）时返回错误，由 main 回退到浏览器模式 ——
// 宁可开浏览器，也不能让用户双击之后什么都没发生。

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jchv/go-webview2"

	"github.com/shenglihalisen/PhotoClassifierPro/web"
)

// webview2Runtime 检查 WebView2 运行时是否装在机器上。
//
// 直接查目录而不是查注册表：注册表有三处（HKLM 32/64 位视图、HKCU），
// 而且卸载残留的键会骗人；看 msedgewebview2.exe 在不在更实在。
func webview2Runtime() bool {
	var roots []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		if base := os.Getenv(env); base != "" {
			roots = append(roots, filepath.Join(base, "Microsoft", "EdgeWebView", "Application"))
		}
	}
	for _, r := range roots {
		if m, _ := filepath.Glob(filepath.Join(r, "*", "msedgewebview2.exe")); len(m) > 0 {
			return true
		}
	}
	return false
}

// runGUI 起一个原生窗口，里面装的是本地服务页面。窗口关掉就返回。
func runGUI(title, version string, port int) error {
	if !webview2Runtime() {
		return fmt.Errorf("未检测到 WebView2 运行时")
	}

	// 端口：指定了就先试指定的，被占用或为 0 时退回随机端口，
	// 免得和别的程序抢 8080 直接起不来。
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return fmt.Errorf("无法监听本地端口: %w", err)
		}
	}
	url := "http://" + ln.Addr().String()

	go func() {
		_ = web.ServeListener(ln, web.Config{
			Addr:    url,
			Version: version,
			Quiet:   true, // 窗口模式没有控制台
		})
	}()

	// 等服务真正可用再开窗口：直接 Navigate 太早会显示"无法访问此页面"，
	// 用户还得手动刷新。
	if !waitReady(url, 5*time.Second) {
		return fmt.Errorf("本地服务启动超时")
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  fmt.Sprintf("%s %s", title, version),
			Width:  1280,
			Height: 860,
			Center: true,
		},
	})
	if w == nil {
		return fmt.Errorf("WebView2 初始化失败")
	}
	defer w.Destroy()

	w.Navigate(url)
	w.Run() // 阻塞到窗口关闭
	return nil
}

// waitReady 轮询本地服务是否已经能响应。
func waitReady(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url + "/api/version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
	return false
}
