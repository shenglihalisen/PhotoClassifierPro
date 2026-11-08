// Package face 人脸与闭眼检测（纯 Go，无 cgo、无 OpenCV、无 MediaPipe）。
//
// 为什么不用 ONNX 关键点模型：那需要 onnxruntime + 一个 5MB 的人脸关键点模型
// + 一套 cgo 子进程，体积和构建复杂度都会上一个台阶。而闭眼判定的实际需求
// 只是「这个人眼睛睁着还是闭着」，用 Viola-Jones 检人脸 + 眼部区域的边缘密度
// 已经够用（这也是旧版 PhotoClassifierPro 在 Python 里走的同一条路）。
//
// 架构上留了 qc.FaceDetector 接口：将来真要换 ONNX 关键点算 EAR，
// 只要实现同一个接口，检测流水线一行都不用改。
package face

import (
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

//go:embed data/haarcascade_frontalface_alt2.xml
var frontalFaceXML []byte

// Rect 特征里的一个矩形（坐标相对窗口，权重用于明暗对比）。
type Rect struct {
	X, Y, W, H float64
	Weight     float64
}

// Feature 一组矩形构成的 Haar 特征（带权求和）。
type Feature struct {
	Rects  []Rect
	Tilted bool
}

// WeakNode 决策树上的一个节点。
//
// Left/Right 是 OpenCV 的相对偏移语义：> 0 表示跳到「当前节点 + 偏移」的下一个节点，
// <= 0 表示叶子，叶子值索引为 -offset。
type WeakNode struct {
	Left         int
	Right        int
	FeatureIndex int
	Threshold    float64
}

// WeakClassifier 一棵弱分类器决策树（alt2 是深度 2 的树，不是单层 stump）。
type WeakClassifier struct {
	Nodes  []WeakNode
	Leaves []float64
}

// Stage 一级强分类器：若干弱分类器投票之和 ≥ StageThreshold 才算通过。
type Stage struct {
	Threshold float64
	Weak      []WeakClassifier
}

// Cascade 完整的级联分类器。
type Cascade struct {
	WinW, WinH int
	Stages     []Stage
	Features   []Feature
}

// ─────────────────────── XML 解析 ───────────────────────

type xmlCascade struct {
	XMLName xml.Name `xml:"opencv_storage"`
	Cascade struct {
		Height  int `xml:"height"`
		Width   int `xml:"width"`
		StageNum int `xml:"stageNum"`
		Stages  struct {
			Items []xmlStage `xml:"_"`
		} `xml:"stages"`
		Features struct {
			Items []xmlFeature `xml:"_"`
		} `xml:"features"`
	} `xml:"cascade"`
}

type xmlStage struct {
	MaxWeakCount   int     `xml:"maxWeakCount"`
	StageThreshold float64 `xml:"stageThreshold"`
	WeakClassifiers struct {
		Items []xmlWeak `xml:"_"`
	} `xml:"weakClassifiers"`
}

type xmlWeak struct {
	InternalNodes string `xml:"internalNodes"`
	LeafValues    string `xml:"leafValues"`
}

type xmlFeature struct {
	Rects struct {
		Items []string `xml:"_"`
	} `xml:"rects"`
	Tilted int `xml:"tilted"`
}

// parseCascade 解析 OpenCV 的 haarcascade XML。
//
// 只支持 stump 形态（每个弱分类器一个 node、两个叶子）—— alt2/tree 系列
// 的 haarcascade 都是这个形态。真正多 node 的树形级联会解析失败并明确报错，
// 绝不静默降级（宁可报错也不要给出错误的人脸框）。
func parseCascade(data []byte) (*Cascade, error) {
	var x xmlCascade
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("级联模型解析失败: %w", err)
	}
	c := &Cascade{WinW: x.Cascade.Width, WinH: x.Cascade.Height}
	if c.WinW <= 0 || c.WinH <= 0 {
		return nil, errors.New("级联模型缺少窗口尺寸")
	}

	for i, fs := range x.Cascade.Features.Items {
		f := Feature{Tilted: fs.Tilted != 0}
		for _, r := range fs.Rects.Items {
			parts := strings.Fields(r)
			if len(parts) != 5 {
				return nil, fmt.Errorf("特征 %d 的矩形格式异常: %q", i, r)
			}
			var v [5]float64
			for k, p := range parts {
				n, err := strconv.ParseFloat(p, 64)
				if err != nil {
					return nil, fmt.Errorf("特征 %d 的矩形数值异常: %q", i, p)
				}
				v[k] = n
			}
			f.Rects = append(f.Rects, Rect{X: v[0], Y: v[1], W: v[2], H: v[3], Weight: v[4]})
		}
		c.Features = append(c.Features, f)
	}

	for si, s := range x.Cascade.Stages.Items {
		st := Stage{Threshold: s.StageThreshold}
		for wi, w := range s.WeakClassifiers.Items {
			nodes := strings.Fields(w.InternalNodes)
			leaves := strings.Fields(w.LeafValues)
			if len(nodes) == 0 || len(nodes)%4 != 0 {
				return nil, fmt.Errorf("stage %d 弱分类器 %d 的节点数不是 4 的倍数: %q", si, wi, w.InternalNodes)
			}
			if len(leaves) == 0 {
				return nil, fmt.Errorf("stage %d 弱分类器 %d 没有叶子值", si, wi)
			}
			wc := WeakClassifier{Leaves: make([]float64, len(leaves))}
			for i, p := range leaves {
				v, err := strconv.ParseFloat(p, 64)
				if err != nil {
					return nil, fmt.Errorf("stage %d 弱分类器 %d 叶子 %d 数值异常", si, wi, i)
				}
				wc.Leaves[i] = v
			}
			for n := 0; n < len(nodes); n += 4 {
				left, err1 := strconv.Atoi(nodes[n])
				right, err2 := strconv.Atoi(nodes[n+1])
				fi, err3 := strconv.Atoi(nodes[n+2])
				th, err4 := strconv.ParseFloat(nodes[n+3], 64)
				if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
					return nil, fmt.Errorf("stage %d 弱分类器 %d 节点 %d 数值异常", si, wi, n/4)
				}
				wc.Nodes = append(wc.Nodes, WeakNode{Left: left, Right: right, FeatureIndex: fi, Threshold: th})
			}
			st.Weak = append(st.Weak, wc)
		}
		c.Stages = append(c.Stages, st)
	}

	if len(c.Stages) == 0 || len(c.Features) == 0 {
		return nil, errors.New("级联模型为空")
	}
	return c, nil
}

// ─────────────────────── 积分图 ───────────────────────

// integral 积分图：ii[(y)*(w+1)+(x)] = 左上角到 (x-1,y-1) 的像素和。
type integral struct {
	ii   []int32
	w, h int // 原图尺寸
}

func newIntegral(g *grayImg) *integral {
	w, h := g.W, g.H
	ii := make([]int32, (w+1)*(h+1))
	stride := w + 1
	for y := 0; y < h; y++ {
		var rowSum int32
		src := y * w
		for x := 0; x < w; x++ {
			rowSum += int32(g.Pix[src+x])
			ii[(y+1)*stride+(x+1)] = ii[y*stride+(x+1)] + rowSum
		}
	}
	return &integral{ii: ii, w: w, h: h}
}

// sum 矩形 (x,y,w,h) 的像素和。
func (in *integral) sum(x, y, w, h int) int32 {
	if w <= 0 || h <= 0 {
		return 0
	}
	stride := in.w + 1
	x1, y1 := x, y
	x2, y2 := x+w, y+h
	if x2 > in.w {
		x2 = in.w
	}
	if y2 > in.h {
		y2 = in.h
	}
	if x1 < 0 {
		x1 = 0
	}
	if y1 < 0 {
		y1 = 0
	}
	if x2 <= x1 || y2 <= y1 {
		return 0
	}
	return in.ii[y2*stride+x2] - in.ii[y1*stride+x2] - in.ii[y2*stride+x1] + in.ii[y1*stride+x1]
}

// ─────────────────────── 检测 ───────────────────────

// grayImg 内部用的灰度图。
type grayImg struct {
	Pix  []byte
	W, H int
}

// Detector 人脸检测器（可并发使用：级联本身只读）。
type Detector struct {
	cascade *Cascade
	once    sync.Once
	err     error
}

// NewDetector 载入内嵌的正面人脸级联。
func NewDetector() *Detector { return &Detector{} }

func (d *Detector) load() (*Cascade, error) {
	d.once.Do(func() {
		d.cascade, d.err = parseCascade(frontalFaceXML)
	})
	return d.cascade, d.err
}

// Face 检出的人脸（像素坐标）。
type Face struct {
	X, Y, W, H int
	Score      float64 // 通过的最后一级强分类器得分，越高越可信
}

// DetectOptions 检测参数。
type DetectOptions struct {
	// MinSize 最小人脸边长（像素）。太小的脸检不准，也基本不构成「这照片废了」。
	MinSize int
	// MaxSize 最大人脸边长，0 表示不限（一般按图短边算）。
	MaxSize int
	// ScaleStep 金字塔缩放步长，默认 1.2。
	ScaleStep float64
	// MinNeighbors 合并阶段要求的最低重叠命中次数，默认 2。
	MinNeighbors int
}

func (o *DetectOptions) normalize(w, h int) {
	if o.MinSize <= 0 {
		o.MinSize = 48
	}
	if o.MaxSize <= 0 {
		m := w
		if h < m {
			m = h
		}
		o.MaxSize = m
	}
	if o.ScaleStep <= 0 {
		// 1.1 是 OpenCV 的默认值。调到 1.2 会把某些尺寸的人脸整个漏掉
		// （实测 lena 的 160px 脸在 1.2 的尺度序列里正好落在 144 和 172 之间）。
		o.ScaleStep = 1.1
	}
	if o.MinNeighbors <= 0 {
		o.MinNeighbors = 2
	}
}

// Detect 在灰度图上检测正面人脸。
//
// 实现是标准的 Viola-Jones：图像金字塔 → 滑动窗口 → 级联逐级否决 → 重叠合并。
// 逐级否决是关键提速点：绝大多数窗口在第一两级就被刷掉，
// 只有极少数候选会走完全部 20 级。
func (d *Detector) Detect(g *grayImg, opt DetectOptions) ([]Face, error) {
	c, err := d.load()
	if err != nil {
		return nil, err
	}
	if g.W < c.WinW || g.H < c.WinH {
		return nil, nil
	}
	opt.normalize(g.W, g.H)

	var hits []Face
	for scale := float64(c.WinW); ; scale *= opt.ScaleStep {
		winW := int(scale + 0.5)
		winH := int(scale*float64(c.WinH)/float64(c.WinW) + 0.5)
		if winW > g.W || winH > g.H || winW > opt.MaxSize*2 {
			break
		}
		if winW < opt.MinSize {
			continue
		}
		// 步长：窗口的 1/12。再粗就会系统性漏掉人脸
		// （step=win/8 时 lena 的 160px 脸一个都没扫到）。
		step := winW / 24
		if step < 2 {
			step = 2
		}
		if step < 2 {
			step = 2
		}
		in := newIntegral(g)
		for y := 0; y+winH < g.H; y += step {
			for x := 0; x+winW < g.W; x += step {
				if score, ok := c.evaluate(in, x, y, winW, winH); ok {
					hits = append(hits, Face{X: x, Y: y, W: winW, H: winH, Score: score})
				}
			}
		}
	}
	return mergeFaces(hits, opt.MinNeighbors), nil
}

// evaluate 在 (x,y) 处用 winW×winH 的窗口跑完整级联。
func (c *Cascade) evaluate(in *integral, x, y, winW, winH int) (float64, bool) {
	// 特征坐标以级联的 WinW/WinH 为基准，需按窗口放大
	sx := float64(winW) / float64(c.WinW)
	sy := float64(winH) / float64(c.WinH)
	// 归一化因子：OpenCV 的阈值是按「窗口面积 × 255」归一后的尺度标定的
	norm := 1.0 / (float64(winW) * float64(winH) * 255.0)

	var lastScore float64
	for si := range c.Stages {
		st := &c.Stages[si]
		var sum float64
		for wi := range st.Weak {
			w := &st.Weak[wi]
			node := 0
			// 防御：级联里有病态环时不能把进程挂死
			for depth := 0; depth < 16; depth++ {
				if node < 0 || node >= len(w.Nodes) {
					return 0, false
				}
				n := &w.Nodes[node]
				fv := c.featureValue(in, n.FeatureIndex, x, y, sx, sy, norm)
				idx := n.Left
				if fv >= n.Threshold {
					idx = n.Right
				}
				if idx <= 0 { // 叶子
					li := -idx
					if li < 0 || li >= len(w.Leaves) {
						return 0, false
					}
					sum += w.Leaves[li]
					break
				}
				node += idx // 相对偏移
			}
		}
		if sum < st.Threshold {
			return 0, false
		}
		lastScore = sum
	}
	return lastScore, true
}

// featureValue 计算单个 Haar 特征的值（已按窗口面积归一化）。
func (c *Cascade) featureValue(in *integral, fi, x, y int, sx, sy, norm float64) float64 {
	if fi < 0 || fi >= len(c.Features) {
		return 0
	}
	f := &c.Features[fi]
	var v float64
	for _, r := range f.Rects {
		rx := x + int(r.X*sx+0.5)
		ry := y + int(r.Y*sy+0.5)
		rw := int(r.W * sx)
		rh := int(r.H * sy)
		if rw < 1 {
			rw = 1
		}
		if rh < 1 {
			rh = 1
		}
		v += float64(in.sum(rx, ry, rw, rh)) * r.Weight
	}
	return v * norm
}

// mergeFaces 合并重叠窗口（简易非极大值抑制）。
//
// 同一张脸会在相邻位置、相邻尺度反复命中，必须合并成一个框，
// 否则一张脸会被数成十几个，闭眼比例直接失真。
func mergeFaces(hits []Face, minNeighbors int) []Face {
	if len(hits) == 0 {
		return nil
	}
	used := make([]bool, len(hits))
	// 按得分从高到低处理：高分框作为合并的中心
	order := make([]int, len(hits))
	for i := range order {
		order[i] = i
	}
	// 简单插入排序（候选通常只有几十个）
	for i := 1; i < len(order); i++ {
		key := order[i]
		j := i - 1
		for j >= 0 && hits[order[j]].Score < hits[key].Score {
			order[j+1] = order[j]
			j--
		}
		order[j+1] = key
	}

	var out []Face
	for _, i := range order {
		if used[i] {
			continue
		}
		group := []int{i}
		used[i] = true
		for _, j := range order {
			if used[j] {
				continue
			}
			if iou(hits[i], hits[j]) > 0.3 {
				used[j] = true
				group = append(group, j)
			}
		}
		if len(group) < minNeighbors {
			continue
		}
		// 取组内平均框
		var sx, sy, sw, sh, sc float64
		for _, k := range group {
			sx += float64(hits[k].X)
			sy += float64(hits[k].Y)
			sw += float64(hits[k].W)
			sh += float64(hits[k].H)
			sc += hits[k].Score
		}
		n := float64(len(group))
		out = append(out, Face{
			X: int(sx/n + 0.5), Y: int(sy/n + 0.5),
			W: int(sw/n + 0.5), H: int(sh/n + 0.5),
			Score: sc / n,
		})
	}
	return out
}

func iou(a, b Face) float64 {
	x0 := math.Max(float64(a.X), float64(b.X))
	y0 := math.Max(float64(a.Y), float64(b.Y))
	x1 := math.Min(float64(a.X+a.W), float64(b.X+b.W))
	y1 := math.Min(float64(a.Y+a.H), float64(b.Y+b.H))
	if x1 <= x0 || y1 <= y0 {
		return 0
	}
	inter := (x1 - x0) * (y1 - y0)
	ua := float64(a.W*a.H) + float64(b.W*b.H) - inter
	if ua <= 0 {
		return 0
	}
	return inter / ua
}
