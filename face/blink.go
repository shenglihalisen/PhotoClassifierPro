package face

// blink.go 闭眼判定：Haar 检人脸 → 按人脸几何切出左右眼区域 → 比边缘密度。
//
// 原理：睁眼时眼眶里有虹膜、眼白、睫毛，是一小片高频结构；闭眼时只剩
// 一条眼睑弧线，边缘像素骤减。所以「眼部区域的边缘密度」就是一个足够好用的
// 开合代理量，不需要 68/106 点关键点。
//
// 两处刻意的保守设计（宁漏勿滥）：
//   - 两只眼睛**都**低于阈值才判闭眼。单眼偏低多半是侧脸、刘海或反光，
//     不是闭眼 —— 误报一张笑得很好看的照片，比漏掉一张闭眼照片更让人恼火。
//   - 眼睛区域在人脸框内的几何位置用固定比例，不追关键点。比例来自旧版
//     PhotoClassifierPro 的实测参数，对正脸足够稳。

import (
	"image"
	"math"
	"sync"
)

// 眼部区域几何（相对人脸框的比例）。
const (
	eyeTopRatio    = 0.22
	eyeBottomRatio = 0.48
	leftEyeX0      = 0.10
	leftEyeX1      = 0.42
	rightEyeX0     = 0.58
	rightEyeX1     = 0.90
)

// BlinkDetector 闭眼检测器。并发安全。
type BlinkDetector struct {
	det *Detector
	mu  sync.RWMutex
	// edgeThresh 眼部边缘密度低于此值判为闭眼。
	edgeThresh float64
	// maxDim 检测前把图缩到这个长边，控制耗时。
	maxDim int
}

// NewBlink 构造闭眼检测器。edgeThresh ≤0 时用默认 0.10。
func NewBlink(edgeThresh float64) *BlinkDetector {
	if edgeThresh <= 0 {
		edgeThresh = 0.10
	}
	return &BlinkDetector{det: NewDetector(), edgeThresh: edgeThresh, maxDim: 1200}
}

// SetEdgeThreshold 运行时调整闭眼阈值。
func (b *BlinkDetector) SetEdgeThreshold(v float64) {
	if v <= 0 {
		return
	}
	b.mu.Lock()
	b.edgeThresh = v
	b.mu.Unlock()
}

// Available 模型是否加载成功。
func (b *BlinkDetector) Available() bool {
	_, err := b.det.load()
	return err == nil
}

// DetectFaces 返回人脸数、闭眼脸数、以及所有被检出眼睛里最小的边缘密度。
//
// minEdge 用来给前端一个「离阈值有多远」的参考；没人脸时返回 0。
func (b *BlinkDetector) DetectFaces(img image.Image) (faces, closed int, minEdge float64) {
	// 肤色预筛：风景、文档、截图这类图占绝大多数，先花 2ms 判掉，
	// 免得每张都去跑 0.2 秒的金字塔扫描。
	if skinRatio(img) < 0.015 {
		return 0, 0, 0
	}
	g := toGrayScaled(img, b.maxDim)
	if g == nil {
		return 0, 0, 0
	}
	fs, err := b.det.Detect(g, DetectOptions{MinSize: 48, MinNeighbors: 3})
	if err != nil || len(fs) == 0 {
		return 0, 0, 0
	}
	b.mu.RLock()
	th := b.edgeThresh
	b.mu.RUnlock()

	minEdge = math.MaxFloat64
	for _, f := range fs {
		l, r := eyeEdgeDensity(g, f)
		edge := math.Min(l, r)
		if edge < minEdge {
			minEdge = edge
		}
		if l < th && r < th {
			closed++
		}
	}
	if minEdge == math.MaxFloat64 {
		minEdge = 0
	}
	return len(fs), closed, minEdge
}

// FaceMinEdge 返回图中每张检出人脸的「左右眼边缘密度的较小值」。
//
// 闭眼判据用的正是这个量（两眼都低才判），所以阈值校准必须统计它，
// 而不是所有眼睛的边际分布 —— 后者会把侧脸/遮挡的单眼样本混进来。
// 返回值可直接拿去算分位数；没有人脸时返回空切片。
func (b *BlinkDetector) FaceMinEdge(img image.Image) []float64 {
	if skinRatio(img) < 0.015 {
		return nil
	}
	g := toGrayScaled(img, b.maxDim)
	if g == nil {
		return nil
	}
	fs, err := b.det.Detect(g, DetectOptions{MinSize: 48, MinNeighbors: 3})
	if err != nil || len(fs) == 0 {
		return nil
	}
	out := make([]float64, 0, len(fs))
	for _, f := range fs {
		l, r := eyeEdgeDensity(g, f)
		out = append(out, math.Min(l, r))
	}
	return out
}

// eyeEdgeDensity 返回左、右眼的边缘密度（0~1）。
func eyeEdgeDensity(g *grayImg, f Face) (float64, float64) {
	// 人脸框稍微外扩：Haar 的框偏紧，眼睛上缘常被切掉
	padX := f.W / 10
	padY := f.H / 10
	x := f.X - padX
	y := f.Y - padY
	w := f.W + 2*padX
	h := f.H + 2*padY
	l := subEdgeDensity(g, x+int(float64(w)*leftEyeX0), y+int(float64(h)*eyeTopRatio),
		int(float64(w)*(leftEyeX1-leftEyeX0)), int(float64(h)*(eyeBottomRatio-eyeTopRatio)))
	r := subEdgeDensity(g, x+int(float64(w)*rightEyeX0), y+int(float64(h)*eyeTopRatio),
		int(float64(w)*(rightEyeX1-rightEyeX0)), int(float64(h)*(eyeBottomRatio-eyeTopRatio)))
	return l, r
}

// subEdgeDensity 子区域的边缘密度：Sobel 梯度幅值超过阈值的像素占比。
//
// 用 Sobel 而不是真 Canny：这里只关心「高频结构的多少」，不关心边缘是否连通，
// 省掉非极大值抑制和双阈值滞后两步，结果稳定且快一个数量级。
func subEdgeDensity(g *grayImg, x, y, w, h int) float64 {
	if w < 6 || h < 4 {
		return 0
	}
	// 裁到图内
	if x < 1 {
		x = 1
	}
	if y < 1 {
		y = 1
	}
	if x+w > g.W-1 {
		w = g.W - 1 - x
	}
	if y+h > g.H-1 {
		h = g.H - 1 - y
	}
	if w < 4 || h < 3 {
		return 0
	}
	const magThresh = 80.0 // Sobel 幅值阈值：对应 Canny 的高低阈值之间
	edge := 0
	total := 0
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			i := yy*g.W + xx
			gx := -float64(g.Pix[i-g.W-1]) + float64(g.Pix[i-g.W+1]) -
				2*float64(g.Pix[i-1]) + 2*float64(g.Pix[i+1]) -
				float64(g.Pix[i+g.W-1]) + float64(g.Pix[i+g.W+1])
			gy := -float64(g.Pix[i-g.W-1]) - 2*float64(g.Pix[i-g.W]) - float64(g.Pix[i-g.W+1]) +
				float64(g.Pix[i+g.W-1]) + 2*float64(g.Pix[i+g.W]) + float64(g.Pix[i+g.W+1])
			if math.Hypot(gx, gy) > magThresh {
				edge++
			}
			total++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(edge) / float64(total)
}

// toGrayScaled 把图像转灰度并按长边限制缩放。
func toGrayScaled(img image.Image, maxDim int) *grayImg {
	if img == nil {
		return nil
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	// 先算出缩放后的尺寸
	dw, dh := w, h
	long := w
	if h > long {
		long = h
	}
	if maxDim > 0 && long > maxDim {
		s := float64(maxDim) / float64(long)
		dw = int(float64(w)*s + 0.5)
		dh = int(float64(h)*s + 0.5)
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
	}
	// 直接按目标网格做面积平均（等价于先灰度再缩放，但只需一次遍历）
	pix := make([]byte, dw*dh)
	for dy := 0; dy < dh; dy++ {
		y0 := dy * h / dh
		y1 := (dy + 1) * h / dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		if y1 > h {
			y1 = h
		}
		for dx := 0; dx < dw; dx++ {
			x0 := dx * w / dw
			x1 := (dx + 1) * w / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if x1 > w {
				x1 = w
			}
			var sum, cnt int
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					r, g, bl, _ := img.At(b.Min.X+xx, b.Min.Y+yy).RGBA()
					sum += (299*int(r>>8) + 587*int(g>>8) + 114*int(bl>>8)) / 1000
					cnt++
				}
			}
			if cnt == 0 {
				pix[dy*dw+dx] = 0
			} else {
				pix[dy*dw+dx] = uint8(sum / cnt)
			}
		}
	}
	return &grayImg{Pix: pix, W: dw, H: dh}
}
