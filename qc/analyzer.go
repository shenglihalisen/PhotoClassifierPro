package qc

// analyzer.go 把「解码 → 双尺度灰度 → 各检测器 → 结果聚合」串成一条流水线。
//
// 单张照片的完整检测成本大致是：解码 10~40ms（取决于尺寸）、
// 64 灰度与直方图类检测 <1ms、细节灰度与模糊/噪点 5~15ms。
// 所以瓶颈在解码，批量扫描靠并发解码把 CPU 吃满。

import (
	"bytes"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sync"
)

// MaxPixels 单张图像素上限（约 1.5 亿，等效 12000×12000）。
// 超过这个量级的多半是拼接全景图或误改扩展名的文件，解码会吃掉数百 MB 内存，
// 对批量扫描是灾难。超限直接判「跳过」而不是判损坏。
const MaxPixels = 150_000_000

// MaxFileBytes 单文件上限（256MB）。误把视频/压缩包改名成 .jpg 时不该把整份
// 文件读进内存；一张正常照片远小于这个数。
const MaxFileBytes = 256 << 20

// FaceDetector 闭眼检测器接口。
//
// 定义成接口是为了让 qc 包不依赖人脸实现（face 包），
// 同时留出替换空间：今天用纯 Go 的 Haar+Canny，将来换成 ONNX 关键点模型
// 只要实现这个接口，检测流程一行都不用改。
type FaceDetector interface {
	// DetectFaces 返回检测到的人脸数、其中闭眼的脸数、最小的眼睛纵横比。
	// 没有检出人脸时返回 (0, 0, 0)。
	DetectFaces(img image.Image) (faces, closed int, minEAR float64)
	// Available 实现是否可用（模型缺失、初始化失败时为 false）。
	Available() bool
}

// Analyzer 检测器。并发安全：阈值读写走读写锁，检测本身无共享状态。
type Analyzer struct {
	mu   sync.RWMutex
	th   Thresholds
	face FaceDetector
}

// New 用默认阈值构造检测器。
func New() *Analyzer {
	return &Analyzer{th: DefaultThresholds()}
}

// SetThresholds 覆盖阈值（自动钳制到合法范围）。
func (a *Analyzer) SetThresholds(t Thresholds) {
	a.mu.Lock()
	a.th = ClampThresholds(t)
	a.mu.Unlock()
}

// CurrentThresholds 当前阈值快照。
func (a *Analyzer) CurrentThresholds() Thresholds {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.th
}

// Version 当前「算法版本 + 阈值指纹」，用于缓存失效判断。
func (a *Analyzer) Version() int { return VersionFor(a.CurrentThresholds()) }

// SetFace 接入闭眼检测器（nil 表示不检测闭眼）。
func (a *Analyzer) SetFace(f FaceDetector) {
	a.mu.Lock()
	a.face = f
	a.mu.Unlock()
}

// FaceAvailable 闭眼检测是否可用。
func (a *Analyzer) FaceAvailable() bool {
	a.mu.RLock()
	f := a.face
	a.mu.RUnlock()
	return f != nil && f.Available()
}

// AnalyzeFile 检测单个文件。任何失败都会返回一个带 FlagCorrupted 的结果，
// 而不是 nil —— 上层永远不用处理 nil，也永远不会把「没测成」误当成「没问题」。
//
// 只读一次盘：早先为了先拿尺寸读一次 512KB、解码又读一次全文件，
// 扫几千张照片就是几千次多余 I/O。现在一次 ReadFile，头部校验和解码都从
// 同一份内存数据走。
func (a *Analyzer) AnalyzeFile(path string) *Result {
	info, err := os.Stat(path)
	if err != nil {
		return &Result{Flags: FlagCorrupted, Corrupted: true, Error: err.Error()}
	}
	if info.Size() > MaxFileBytes {
		return &Result{
			Flags:     FlagCorrupted,
			Corrupted: true,
			Error:     fmt.Sprintf("文件 %d MB 超过上限 %d MB，已跳过", info.Size()>>20, MaxFileBytes>>20),
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return &Result{Flags: FlagCorrupted, Corrupted: true, Error: err.Error()}
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		if cfg.Width*cfg.Height > MaxPixels {
			return &Result{
				Flags:     FlagCorrupted,
				Width:     cfg.Width,
				Height:    cfg.Height,
				Corrupted: true,
				Error:     fmt.Sprintf("像素数 %d 超过上限 %d，已跳过", cfg.Width*cfg.Height, MaxPixels),
			}
		}
	}
	var img image.Image
	if IsExternalFormat(filepath.Ext(path)) {
		// HEIC/RAW 要让外部命令读到真实文件，走文件路径那条路
		img, err = DecodeFile(path)
	} else {
		img, err = DecodeBytes(data)
	}
	if err != nil {
		return &Result{Flags: FlagCorrupted, Corrupted: true, Error: err.Error()}
	}
	return a.AnalyzeImage(img)
}

// AnalyzeBytes 检测内存中的图片（Web 上传路径）。
func (a *Analyzer) AnalyzeBytes(data []byte) *Result {
	img, err := DecodeBytes(data)
	if err != nil {
		return &Result{Flags: FlagCorrupted, Corrupted: true, Error: err.Error()}
	}
	return a.AnalyzeImage(img)
}

// AnalyzeImage 检测已解码的图像。
func (a *Analyzer) AnalyzeImage(img image.Image) *Result {
	if img == nil {
		return &Result{Flags: FlagCorrupted, Corrupted: true, Error: "空图像"}
	}
	th := a.CurrentThresholds()
	r := &Result{}

	b := img.Bounds()
	r.Width, r.Height = b.Dx(), b.Dy()
	if r.Width <= 0 || r.Height <= 0 {
		return &Result{Flags: FlagCorrupted, Corrupted: true, Error: "图像尺寸非法"}
	}

	// ── 第一层：64×64 灰度（直方图类）
	g64 := gray64(img)
	if len(g64) == 0 {
		return &Result{Flags: FlagCorrupted, Corrupted: true, Error: "灰度提取失败"}
	}

	if blank, std := detectBlank(g64, th.BlankStd); blank {
		r.Flags |= FlagBlank
		r.Blank = &DetectorOut{Conf: std}
	}
	r.GrayStd = stdOfGray(g64)

	if boxes, conf := detectLensDirt(g64, GraySize); len(boxes) > 0 {
		r.Flags |= FlagLensDirt
		r.LensDirt = &LensDirtOut{Conf: conf, Boxes: boxes}
	}

	// 曝光是直方图统计，与分辨率无关，用现成的 64 灰度即可。
	// 指标本身总是记下来（校准/排查要看），但只有越过阈值才挂位掩码。
	if exp := detectExposure(g64, th.ExpUnder, th.ExpOver); exp != nil {
		r.ExpMean, r.ExpClipLow, r.ExpClipHi = exp.Mean, exp.ClipLow, exp.ClipHigh
		if exp.Under || exp.Over {
			r.Flags |= FlagExposure
			r.Exposure = exp
		}
	}

	r.DupHash = HashString(pHash(g64, GraySize))

	// ── 第二层：细节灰度（模糊 / 噪点）
	//
	// 空镜已经命中的图不必再判模糊：一张纯黑片子的拉普拉斯标准差必然是 0，
	// 两个徽章同时挂上去只是噪音（用户真正要知道的是「这是空镜」）。
	if r.Flags&FlagBlank == 0 {
		if detail, dw, dh := grayDetail(img, DetailSize); len(detail) > 0 {
			lap := lapStdOf(detail, dw, dh)
			r.LapStd = lap
			if lap < th.BlurLapStd {
				r.Flags |= FlagBlur
				r.Blur = &DetectorOut{Conf: lap}
			}
			floor := noiseFloorOf(detail, dw, dh)
			r.NoiseFloor = floor
			if floor > th.NoiseFloor {
				r.Flags |= FlagNoise
				r.Noise = &DetectorOut{Conf: floor}
			}
		}
	}

	// ── 第三层：人脸（闭眼）
	//
	// 只对「人像尺度」的图有意义：太小的缩略图检不出人脸，纯风景图也检不出，
	// 所以这里不加额外门槛，交给检测器自己返回 (0,0,0)。
	a.mu.RLock()
	face := a.face
	a.mu.RUnlock()
	if face != nil && face.Available() {
		if faces, closed, minEdge := face.DetectFaces(img); faces > 0 {
			r.Blink = &BlinkOut{Faces: faces, Closed: closed, MinEdge: minEdge}
			if closed > 0 {
				r.Flags |= FlagBlink
				// 置信度：闭眼占比越高越确定，同时用边缘密度偏离阈值的幅度加权
				conf := float64(closed) / float64(faces) * 0.6
				if th.BlinkEdge > 0 {
					conf += clamp01((th.BlinkEdge-minEdge)/th.BlinkEdge) * 0.4
				}
				r.Blink.Conf = clamp01(conf)
			}
		}
	}

	return r
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
