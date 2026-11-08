//go:build !windows

package main

// 非 Windows 平台不需要 DPI 声明（浏览器 / WebKit 自己处理缩放）。
func init() {}
