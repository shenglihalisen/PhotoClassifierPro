package qc

// external.go HEIC / 相机 RAW 的可选支持。
//
// Go 标准库和 x/image 解不开这两种格式，纯 Go 实现 HEIF 解码器不现实。
// 但把它们打包进 exe 又会拖进 cgo（libheif / libraw），破坏单文件静态构建。
//
// 折中：主程序保持纯 Go，遇到这类文件时**可选地**调用系统已装的
// ffmpeg 或 ImageMagick 转成 JPEG 再解码。没装就给一条明确的错误，
// 绝不静默当成「好照片」。
//
// 安全性：exec.Command 参数分离传，不经过 shell，路径不会被解释成命令；
// 输出限 64MB、超时 20 秒，避免异常文件把进程拖死。

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// externalExts 需要外部转换器才能读的格式。
var externalExts = map[string]bool{
	".heic": true, ".heif": true,
	".cr2": true, ".cr3": true, ".nef": true, ".arw": true,
	".dng": true, ".raf": true, ".orf": true, ".rw2": true, ".pef": true,
}

var converter struct {
	once sync.Once
	path string
	kind string // "ffmpeg" / "magick"
}

// ExternalConverter 返回可用的外部转换器，没装则返回空串。
// 结果缓存：扫描几千张 HEIC 时不能每张都去 PATH 里翻一遍。
func ExternalConverter() string {
	converter.once.Do(func() {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			converter.path, converter.kind = p, "ffmpeg"
			return
		}
		if p, err := exec.LookPath("magick"); err == nil {
			converter.path, converter.kind = p, "magick"
		}
	})
	return converter.path
}

// ConverterName 给 UI / 文档显示用的是哪个转换器（"ffmpeg" / "magick" / ""）。
func ConverterName() string {
	ExternalConverter()
	return converter.kind
}

// IsExternalFormat 这个扩展名是否需要外部转换器。
func IsExternalFormat(ext string) bool {
	return externalExts[strings.ToLower(ext)]
}

// convertVia 把文件转成单帧 JPEG 并解码。
func convertVia(tool, path string) (image.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if converter.kind == "ffmpeg" {
		cmd = exec.CommandContext(ctx, tool,
			"-hide_banner", "-loglevel", "error",
			"-i", path,
			"-frames:v", "1",
			"-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1")
	} else {
		cmd = exec.CommandContext(ctx, tool, path, "jpeg:-")
	}

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(pipe, 64<<20))
	_ = cmd.Wait()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("外部转换超时")
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("外部转换器没有输出")
	}
	return jpeg.Decode(bytes.NewReader(data))
}

var _ = filepath.Ext
