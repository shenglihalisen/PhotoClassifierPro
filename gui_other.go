//go:build !windows

// gui_other.go 非 Windows 平台没有 WebView2，GUI 模式直接不可用，
// 由 main 回退到「起服务 + 开默认浏览器」。

package main

import "fmt"

func runGUI(title, version string, port int) error {
	return fmt.Errorf("原生窗口仅在 Windows 上可用")
}
