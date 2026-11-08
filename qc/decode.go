package qc

// decode.go 纯 Go 图片解码 + 双尺度灰度提取。
//
// 为什么不用 ffmpeg：这个工具的卖点是「一个 exe 双击就能用」，不该让用户
// 去装 200MB 的 ffmpeg。Go 标准库 + x/image 覆盖 jpeg/png/gif/bmp/tiff/webp，
// 已经涵盖了照片管理的绝大多数场景。代价是不支持 HEIC/HEIF 与相机 RAW
// （这两类需要 libheif / libraw 的 cgo 绑定，会破坏 CGO_ENABLED=0 的单文件前提）。
//
// 灰度系数用 BT.601（0.299R + 0.587G + 0.114B），与 ffmpeg 的 gray 转换、
// OpenCV 的 COLOR_BGR2GRAY 一致 —— 这样阈值才能和 medreview / 旧 Python 版对齐。

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// SupportedExts 声明支持的扩展名（供扫描器预筛，避免拿二进制文件去解码）。
var SupportedExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".jpe": true,
	".png": true,
	".gif": true,
	".bmp": true,
	".tif": true, ".tiff": true,
	".webp": true,
	// 下面这些需要系统装了 ffmpeg 或 ImageMagick 才能解（见 external.go）；
	// 没装时扫描器仍会收集它们，但会记成带明确原因的「损坏」。
	".heic": true, ".heif": true,
	".cr2": true, ".cr3": true, ".nef": true, ".arw": true,
	".dng": true, ".raf": true, ".orf": true, ".rw2": true, ".pef": true,
}

// DecodeFile 读文件并解码成 image.Image，同时按 EXIF Orientation 转正。
//
// EXIF 方向必须处理：手机竖拍的照片在像素上是横的，只靠 Orientation 标记
// 才显示成正的。不转正的话人脸检测会把人脸当侧躺、构图类判断也会歪。
func DecodeFile(path string) (image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	img, err := DecodeBytes(data)
	if err == nil {
		return img, nil
	}
	// 纯 Go 解不开：如果是 HEIC/RAW，试试系统的外部转换器
	ext := filepath.Ext(path)
	if IsExternalFormat(ext) {
		if tool := ExternalConverter(); tool != "" {
			if img2, err2 := convertVia(tool, path); err2 == nil {
				return img2, nil
			} else {
				return nil, fmt.Errorf("%w（外部转换也失败了: %v）", err, err2)
			}
		}
		return nil, fmt.Errorf("%w（%s 需要系统安装 ffmpeg 或 ImageMagick）", err, ext)
	}
	return nil, err
}

// DecodeBytes 从内存解码（供 Web 上传与测试用）。
func DecodeBytes(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解码失败（格式不支持或文件损坏）: %w", err)
	}
	if o := jpegOrientation(data); o != 0 && o != 1 {
		img = applyOrientation(img, o)
	}
	return img, nil
}

// ─────────────────────── EXIF 方向 ───────────────────────

// jpegOrientation 从 JPEG 的 APP1/Exif 段里读 Orientation(0x0112)。
// 非 JPEG 或没有 EXIF 时返回 0（= 未知，按原样处理）。
func jpegOrientation(data []byte) int {
	// SOI
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0
	}
	p := 2
	for p+4 <= len(data) {
		if data[p] != 0xFF {
			return 0
		}
		marker := data[p+1]
		// 无长度字段的标记
		if marker == 0xD8 || marker == 0xD9 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			p += 2
			continue
		}
		if p+4 > len(data) {
			return 0
		}
		segLen := int(binary.BigEndian.Uint16(data[p+2 : p+4]))
		if segLen < 2 || p+2+segLen > len(data) {
			return 0
		}
		seg := data[p+4 : p+2+segLen]
		// APP1 且以 "Exif\0\0" 开头
		if marker == 0xE1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		// 到 SOS 就不会再有 EXIF 了
		if marker == 0xDA {
			return 0
		}
		p += 2 + segLen
	}
	return 0
}

// tiffOrientation 在 TIFF 头（IFD0）里找 Orientation 标签。
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(t[2:4]) != 42 {
		return 0
	}
	ifdOff := int(bo.Uint32(t[4:8]))
	if ifdOff <= 0 || ifdOff+2 > len(t) {
		return 0
	}
	n := int(bo.Uint16(t[ifdOff : ifdOff+2]))
	base := ifdOff + 2
	for i := 0; i < n; i++ {
		e := base + i*12
		if e+12 > len(t) {
			return 0
		}
		tag := bo.Uint16(t[e : e+2])
		if tag == 0x0112 {
			// SHORT，值内联在偏移 8 处的前 2 字节
			return int(bo.Uint16(t[e+8 : e+10]))
		}
	}
	return 0
}

// applyOrientation 按 EXIF Orientation(1~8) 旋转/翻转图像。
func applyOrientation(src image.Image, o int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	// 5~8 会交换长宽
	swap := o >= 5
	outW, outH := w, h
	if swap {
		outW, outH = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			var nx, ny int
			switch o {
			case 2: // 水平翻转
				nx, ny = w-1-x, y
			case 3: // 180°
				nx, ny = w-1-x, h-1-y
			case 4: // 垂直翻转
				nx, ny = x, h-1-y
			case 5: // 转置
				nx, ny = y, x
			case 6: // 顺时针 90°
				nx, ny = h-1-y, x
			case 7: // 反转置
				nx, ny = h-1-y, w-1-x
			case 8: // 逆时针 90°
				nx, ny = y, w-1-x
			default:
				nx, ny = x, y
			}
			dst.SetRGBA(nx, ny, color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)})
		}
	}
	return dst
}

// ─────────────────────── 灰度提取 ───────────────────────

// toGray 把彩色图转成单通道灰度缓冲（BT.601 系数）。
func toGray(img image.Image) gray {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	pix := make([]byte, w*h)
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			// RGBA() 返回的是 16 位预乘值，取高 8 位即可
			pix[i] = uint8((299*int(r>>8) + 587*int(g>>8) + 114*int(bl>>8)) / 1000)
			i++
		}
	}
	return gray{Pix: pix, W: w, H: h}
}

// gray64 生成 64×64 灰度（强制正方形，直方图类检测用）。
//
// 强制正方形是刻意的：medreview 的 ffmpeg 取的是 scale=64:64，
// 空镜/脏污/曝光/pHash 的阈值都是在这个口径下校准的。
func gray64(img image.Image) []byte {
	g := toGray(img)
	return resizeBox(g.Pix, g.W, g.H, GraySize, GraySize)
}

// grayDetail 生成细节灰度：长边缩到 DetailSize，保持宽高比。
//
// 为什么要单独一张：64×64 相当于对原图做 8~60 倍降采样，
// 会把高频细节和噪点一起平均掉 —— 模糊和噪点必须看高清的那一张。
func grayDetail(img image.Image, long int) (pix []byte, w, h int) {
	g := toGray(img)
	if g.W <= 0 || g.H <= 0 {
		return nil, 0, 0
	}
	srcLong := g.W
	if g.H > srcLong {
		srcLong = g.H
	}
	if srcLong <= long {
		return g.Pix, g.W, g.H // 本身就不大，不用放大
	}
	scale := float64(long) / float64(srcLong)
	dw := int(float64(g.W)*scale + 0.5)
	dh := int(float64(g.H)*scale + 0.5)
	if dw < 8 {
		dw = 8
	}
	if dh < 8 {
		dh = 8
	}
	return resizeBox(g.Pix, g.W, g.H, dw, dh), dw, dh
}

// resizeBox 盒式平均缩放（面积平均）。
//
// 用盒式而不是双线性：缩小倍数很大时（4000px → 64px），双线性只采样少数像素，
// 结果会随采样相位抖动；盒式是真正的面积平均，稳定且符合"降采样后测噪声"的语义。
func resizeBox(src []byte, sw, sh, dw, dh int) []byte {
	if sw <= 0 || sh <= 0 || dw <= 0 || dh <= 0 {
		return nil
	}
	if sw == dw && sh == dh {
		out := make([]byte, len(src))
		copy(out, src)
		return out
	}
	out := make([]byte, dw*dh)
	for dy := 0; dy < dh; dy++ {
		y0 := dy * sh / dh
		y1 := (dy + 1) * sh / dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		if y1 > sh {
			y1 = sh
		}
		for dx := 0; dx < dw; dx++ {
			x0 := dx * sw / dw
			x1 := (dx + 1) * sw / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if x1 > sw {
				x1 = sw
			}
			var sum, cnt int
			for y := y0; y < y1; y++ {
				row := y * sw
				for x := x0; x < x1; x++ {
					sum += int(src[row+x])
					cnt++
				}
			}
			if cnt == 0 {
				out[dy*dw+dx] = 0
			} else {
				out[dy*dw+dx] = uint8(sum / cnt)
			}
		}
	}
	return out
}

// loadFromReader 供 Web 上传用：限制读入大小，避免超大文件打爆内存。
func loadFromReader(r io.Reader, maxBytes int64) ([]byte, error) {
	lr := io.LimitReader(r, maxBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("文件超过上限 %d MB", maxBytes/1024/1024)
	}
	return data, nil
}

// GrayThumb 返回 n×n 的灰度缩略图（跨文件像素比对用）。
//
// 强制正方形：比对只看「同一位置是不是同一个东西」，
// 拉伸到同一尺寸比保持宽高比更好对齐。
func GrayThumb(img image.Image, n int) []byte {
	if img == nil || n <= 0 {
		return nil
	}
	g := toGray(img)
	return resizeBox(g.Pix, g.W, g.H, n, n)
}
