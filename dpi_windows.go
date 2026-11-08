//go:build windows

package main

// dpi_windows.go 声明系统 DPI 感知。
//
// Windows 在 125% / 150% 缩放下，会把「未声明 DPI 感知」的进程窗口按 96dpi
// 渲染成位图，再整体拉伸到目标尺寸 —— 文字边缘和缩略图全部发虚。
// WebView2 自带清晰的矢量渲染，前提是宿主进程别让系统去做那次拉伸。
//
// 选 PROCESS_SYSTEM_DPI_AWARE 而不是 Per-Monitor V2：
//   - 单显示器 / 统一缩放（本机 120 DPI = 125%）场景下效果一样清晰；
//   - 不需要处理 WM_DPICHANGED，窗口尺寸仍按逻辑像素给，
//     不会在 125% 屏幕上突然变成一个偏小的窗口。
//
// init() 保证在任何窗口创建之前执行。失败（旧系统 / 已由 manifest 设置）时静默忽略，
// 最坏情况退回原来的拉伸行为，不影响功能。
import (
	"syscall"
)

func init() {
	// shcore.SetProcessDpiAwareness(PROCESS_SYSTEM_DPI_AWARE = 1)，Vista SP2 / Win8+
	if shcore := syscall.NewLazyDLL("shcore.dll"); shcore.Handle() != 0 {
		if p := shcore.NewProc("SetProcessDpiAwareness"); p.Find() == nil {
			// S_OK = 设置成功；E_ACCESSDENIED = 已由 manifest 设置过，同样没问题
			_, _, _ = p.Call(1)
			return
		}
	}
	// 兜底：Win7 / 没有 shcore 时用 user32 的老 API
	if u := syscall.NewLazyDLL("user32.dll"); u.Handle() != 0 {
		if p := u.NewProc("SetProcessDPIAware"); p.Find() == nil {
			_, _, _ = p.Call()
		}
	}
}
