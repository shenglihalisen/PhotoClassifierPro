// Package qc 照片质量自动检测。
//
// 算法源自 medreview 的 qc 包（QC v5），只保留图片路径的检测器：
// 损坏 / 空镜 / 镜头脏污 / 模糊 / 曝光 / 噪点 / 重复。
// 视频相关部分（抖动检测、全片抽帧、硬件加速探测）对静态照片没有意义，已裁剪。
//
// 三条设计原则（继承自 medreview）：
//   - 只标注，不改文件。检测结果永远只是建议，动不动照片由人决定。
//   - 零外部依赖。纯 Go 解码（jpeg/png/gif/bmp/tiff/webp），不需要 ffmpeg、
//     不需要 OpenCV、不需要 Python。单文件分发的前提。
//   - 阈值可运行时覆盖，且改阈值会让旧结果自动失效（见 thresholds.go 的 VersionFor）。
package qc

import (
	"encoding/binary"
	"fmt"
	"image"
	"math"
	"math/bits"
	"sort"
)

// qcVersion 在算法或阈值语义调整时 +1。
// 5：与 medreview QC v5 对齐（重复加像素二次确认、曝光改 clip 双条件、细节提取保持宽高比）
const qcVersion = 5

// Version 导出给外部落库/缓存使用。
const Version = qcVersion

// 检测器位掩码。
const (
	FlagDup       = 1 << iota // 重复
	FlagCorrupted             // 损坏
	FlagBlank                 // 空镜（全黑/全白/纯色）
	FlagLensDirt              // 镜头脏污 / 局部遮挡
	FlagBlur                  // 模糊（失焦 / 运动模糊）
	FlagExposure              // 曝光异常（欠曝 / 过曝）
	FlagNoise                 // 噪点（高感颗粒 / 压缩噪声）
	FlagBlink                 // 闭眼（人脸检测器，由 face 包填充）
)

// FlagIssueMask 所有「质量问题」位的合集。
// 注意不含重复：重复只是冗余，照片本身没问题，混进来会让"只看问题件"被好片淹没。
const FlagIssueMask = FlagCorrupted | FlagBlank | FlagLensDirt | FlagBlur | FlagExposure | FlagNoise | FlagBlink

// 测量常量（不是判定阈值，判定阈值见 thresholds.go）。
const (
	GraySize   = 64   // 灰度提取边长（直方图类判定用）
	DetailSize = 1024 // 细节提取长边（模糊/噪点用，64 会把细节和噪点一起平均掉）
	NoiseCell  = 16   // 噪点统计的块边长（固定边长，不是固定块数）
)

// 判定阈值（默认值 = 实拍校准常量，可在运行时覆盖）。
const (
	BlankStdThresh     = 8.0  // 全局灰度标准差低于此值判空镜
	LensDirtMinBlocks  = 3    // 至少多少异常块才算脏污
	DupHammingThresh   = 5    // pHash 汉明距离 ≤ 此值判重复（只是候选，还要过像素确认）
	DupPixelGrayThresh = 25   // 像素确认：灰度差超过此值算"显著不同"的像素
	DupPixelDiffThresh = 0.05 // 像素确认：显著不同像素占比 > 5% = 主体不同，不算重复
	DupTimeWindowSecs  = 10   // hash 相同的两张，拍摄时间差超过此值就不是"连拍/副本"

	// 2026-09-26 二次校准于一组真实素材（14 张，细节提取保持宽高比）：
	// 真实图 lap=34.3~70.9、噪声底线 0.00~1.54；重度模糊 1.78、加噪图 8.20。
	// 宁漏勿滥，两侧都留 2~3 倍余量。
	BlurLapStdThresh = 15.0 // 低于此值判模糊（真实最小 34.3，留 2 倍余量）
	NoiseFloorThresh = 5.0  // 高于此值判噪点（真实最大 1.54，留 3 倍余量）

	// 曝光：光看均值不行（夜景本来就暗、雪地本来就亮），必须看死黑/死白占比。
	// 过曝阈值定得高，是为了躲开白底文档/截图：OCR 截图 mean≈240、clipHi 0.63~0.75
	// 是正常的白纸黑字，而真过曝的图 clipHi=0.99。
	ExpClipLowThresh  = 0.35 // 近黑像素占比 + 整体偏暗 → 欠曝
	ExpClipHighThresh = 0.80 // 近白像素占比 + 整体偏亮 → 过曝
	ExpUnderMean      = 100.0
	ExpOverMean       = 150.0
	ExpClipLowValue   = 8   // 判定"死黑"的灰度上限
	ExpClipHighValue  = 247 // 判定"死白"的灰度下限
)

// Box 脏点框，坐标归一化到 0~1（前端按比例画到图片上）。
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// IoU 两个脏点框的交并比。镜头污点是固定在画面坐标上的，
// 同一设备两张照片的污点框会高度重叠，所以 IoU 是"真污点"的核心证据。
func (b Box) IoU(o Box) float64 {
	x0 := math.Max(b.X, o.X)
	y0 := math.Max(b.Y, o.Y)
	x1 := math.Min(b.X+b.W, o.X+o.W)
	y1 := math.Min(b.Y+b.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return 0
	}
	inter := (x1 - x0) * (y1 - y0)
	area := b.W*b.H + o.W*o.H - inter
	if area <= 0 {
		return 0
	}
	return inter / area
}

// DetectorOut 只带置信度的检测器输出。
type DetectorOut struct {
	Conf float64 `json:"conf"`
}

// LensDirtOut 镜头脏污输出：置信度 + 脏点位置。
type LensDirtOut struct {
	Conf  float64 `json:"conf"`
	Boxes []Box   `json:"boxes"`
}

// ExposureOut 曝光输出。
type ExposureOut struct {
	Under    bool    `json:"under"`
	Over     bool    `json:"over"`
	Mean     float64 `json:"mean"`
	ClipLow  float64 `json:"clipLow"`  // 近黑像素占比
	ClipHigh float64 `json:"clipHigh"` // 近白像素占比
}

// Result 单张照片的检测结果。JSON 字段直接给前端。
type Result struct {
	Flags int `json:"flags"`

	Corrupted bool `json:"corrupted,omitempty"`

	DupHash string `json:"dupHash,omitempty"`

	Blank    *DetectorOut  `json:"blank,omitempty"`
	LensDirt *LensDirtOut  `json:"lensDirt,omitempty"`
	Blur     *DetectorOut  `json:"blur,omitempty"`
	Exposure *ExposureOut  `json:"exposure,omitempty"`
	Noise    *DetectorOut  `json:"noise,omitempty"`
	Blink    *BlinkOut     `json:"blink,omitempty"`

	// 原始指标：不管有没有越过阈值都记下来，排查和校准时要看。
	GrayStd    float64 `json:"grayStd"`
	LapStd     float64 `json:"lapStd"`
	NoiseFloor float64 `json:"noiseFloor"`
	ExpMean    float64 `json:"expMean"`
	ExpClipLow float64 `json:"expClipLow"`
	ExpClipHi  float64 `json:"expClipHigh"`

	Width  int `json:"width"`
	Height int `json:"height"`

	// Error 仅在解码失败等异常情况下非空（同时会置 FlagCorrupted）。
	Error string `json:"error,omitempty"`
}

// BlinkOut 闭眼检测输出。
type BlinkOut struct {
	Faces   int     `json:"faces"`
	Closed  int     `json:"closed"`
	MinEdge float64 `json:"minEdge"` // 被检出眼睛里最小的边缘密度
	Conf    float64 `json:"conf"`
}

// Has 是否命中某个检测器位。
func (r *Result) Has(flag int) bool { return r != nil && r.Flags&flag != 0 }

// AnyIssue 是否命中任何「质量问题」位（不含重复）。
func (r *Result) AnyIssue() bool { return r != nil && r.Flags&FlagIssueMask != 0 }

// Labels 返回命中的检测器中文名（前端与 CSV 共用一份，避免两边措辞不一致）。
func (r *Result) Labels() []string {
	if r == nil {
		return nil
	}
	var out []string
	if r.Flags&FlagCorrupted != 0 {
		out = append(out, "损坏")
	}
	if r.Flags&FlagBlank != 0 {
		out = append(out, "空镜")
	}
	if r.Flags&FlagBlur != 0 {
		out = append(out, "模糊")
	}
	if r.Flags&FlagExposure != 0 {
		if r.Exposure != nil && r.Exposure.Under {
			out = append(out, "欠曝")
		}
		if r.Exposure != nil && r.Exposure.Over {
			out = append(out, "过曝")
		}
	}
	if r.Flags&FlagNoise != 0 {
		out = append(out, "噪点")
	}
	if r.Flags&FlagLensDirt != 0 {
		out = append(out, "脏污")
	}
	if r.Flags&FlagBlink != 0 {
		out = append(out, "闭眼")
	}
	if r.Flags&FlagDup != 0 {
		out = append(out, "重复")
	}
	return out
}

// ───────────────────────── 灰度缓冲 ─────────────────────────

// gray 一块单通道灰度图。
type gray struct {
	Pix  []byte
	W, H int
}

// detectBlank 空镜：全局灰度标准差低于阈值。
func detectBlank(buf []byte, blankStd float64) (bool, float64) {
	std := stdOfGray(buf)
	return std < blankStd, std
}

// stdOfGray 全局灰度标准差。
func stdOfGray(buf []byte) float64 {
	if len(buf) == 0 {
		return 0
	}
	var sum, sum2 float64
	for _, b := range buf {
		v := float64(b)
		sum += v
		sum2 += v * v
	}
	n := float64(len(buf))
	mean := sum / n
	variance := sum2/n - mean*mean
	if variance < 0 {
		variance = 0
	}
	return math.Sqrt(variance)
}

// detectExposure 曝光异常：近黑/近白像素占比过高 **并且** 整体确实偏暗/偏亮。
//
// 只看均值会误判：夜景本来就暗、雪地本来就亮，那都是正常曝光。
// 真正的欠曝/过曝特征是直方图在 0 或 255 处堆出一个尖峰（暗部被压死 / 高光糊掉）。
func detectExposure(buf []byte, underMean, overMean float64) *ExposureOut {
	if len(buf) == 0 {
		return nil
	}
	var sum float64
	low, high := 0, 0
	n := len(buf)
	for _, b := range buf {
		sum += float64(b)
		if int(b) <= ExpClipLowValue {
			low++
		}
		if int(b) >= ExpClipHighValue {
			high++
		}
	}
	mean := sum / float64(n)
	out := &ExposureOut{
		Mean:     mean,
		ClipLow:  float64(low) / float64(n),
		ClipHigh: float64(high) / float64(n),
	}
	if mean < underMean && out.ClipLow > ExpClipLowThresh {
		out.Under = true
	}
	if mean > overMean && out.ClipHigh > ExpClipHighThresh {
		out.Over = true
	}
	return out
}

// lapStdOf 拉普拉斯响应的标准差（越大越锐利）。
// 模糊 = 高频缺失 = 边缘变缓 = 拉普拉斯响应整体变小，所以这个值和锐度正相关。
// 支持任意 w×h（细节提取保持宽高比，不一定是正方形）。
func lapStdOf(buf []byte, w, h int) float64 {
	if w < 3 || h < 3 {
		return 0
	}
	var sum, sum2 float64
	cnt := 0
	for y := 1; y < h-1; y++ {
		row := y * w
		for x := 1; x < w-1; x++ {
			i := row + x
			l := 4*float64(buf[i]) - float64(buf[i-1]) - float64(buf[i+1]) -
				float64(buf[i-w]) - float64(buf[i+w])
			sum += l
			sum2 += l * l
			cnt++
		}
	}
	if cnt == 0 {
		return 0
	}
	mean := sum / float64(cnt)
	v := sum2/float64(cnt) - mean*mean
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v)
}

// noiseFloorOf 噪声底线：所有块标准差里**最平坦那 10%** 的中位数（取第 10 百分位）。
//
// 再干净的图也有成片的平坦区（天空、墙面、皮肤），那里的波动**就是**噪声本身；
// 取最平坦的一批块来量标准差，就能把"结构纹理"排除掉、单独测出噪声。
// 块边长固定（不是块数固定）：不管图片多大、宽高比如何，每块覆盖的像素数都一样，
// 噪声统计才可比。
func noiseFloorOf(buf []byte, w, h int) float64 {
	cell := NoiseCell
	if w < cell*2 || h < cell*2 {
		return 0
	}
	gw, gh := w/cell, h/cell
	stds := make([]float64, 0, gw*gh)
	for gy := 0; gy < gh; gy++ {
		for gx := 0; gx < gw; gx++ {
			var s, s2 float64
			cnt := 0
			y0, y1 := gy*cell, (gy+1)*cell
			x0, x1 := gx*cell, (gx+1)*cell
			if y1 > h {
				y1 = h
			}
			if x1 > w {
				x1 = w
			}
			for y := y0; y < y1; y++ {
				row := y * w
				for x := x0; x < x1; x++ {
					v := float64(buf[row+x])
					s += v
					s2 += v * v
					cnt++
				}
			}
			if cnt == 0 {
				continue
			}
			m := s / float64(cnt)
			v := s2/float64(cnt) - m*m
			if v < 0 {
				v = 0
			}
			stds = append(stds, math.Sqrt(v))
		}
	}
	if len(stds) == 0 {
		return 0
	}
	sort.Float64s(stds)
	idx := len(stds) / 10
	if idx >= len(stds) {
		idx = len(stds) - 1
	}
	return stds[idx]
}

// detectLensDirt 镜头脏污 / 局部遮挡检测。
//
// 思路（区别于简单的"低方差块"）：真实的脏污/遮挡在灰度上是
//   - 内部平滑（低方差），且
//   - 相对周围邻域是"突兀的"亮/暗块（亮度异常）。
//
// 天空、墙面这类大面积平滑区虽平滑，但邻域同质 → 不判异常；
// 地平线这类细线因宽/高不足 2 块被排除；整帧统一偏色被面积/密度约束排除。
func detectLensDirt(buf []byte, q int) ([]Box, float64) {
	grid := 8
	cell := q / grid
	if cell < 1 {
		return nil, 0
	}
	means := make([]float64, grid*grid)
	stds := make([]float64, grid*grid)
	for gy := 0; gy < grid; gy++ {
		for gx := 0; gx < grid; gx++ {
			var s, s2 float64
			cnt := 0
			for y := gy * cell; y < (gy+1)*cell; y++ {
				for x := gx * cell; x < (gx+1)*cell; x++ {
					v := float64(buf[y*q+x])
					s += v
					s2 += v * v
					cnt++
				}
			}
			if cnt == 0 {
				continue
			}
			m := s / float64(cnt)
			v := s2/float64(cnt) - m*m
			if v < 0 {
				v = 0
			}
			means[gy*grid+gx] = m
			stds[gy*grid+gx] = math.Sqrt(v)
		}
	}
	// 全局块方差分布：衡量整图纹理强度，用于设定"平滑"阈值。
	var gvSum, gvSum2 float64
	for _, s := range stds {
		gvSum += s
		gvSum2 += s * s
	}
	gn := float64(len(stds))
	gvMean := gvSum / gn
	gvVar := gvSum2/gn - gvMean*gvMean
	if gvVar < 0 {
		gvVar = 0
	}
	gvStd := math.Sqrt(gvVar)
	// 平滑阈值：低于此方差视为"疑似污点内部"。固定下限 4 避免把有纹理当平滑。
	smoothThresh := math.Max(4.0, 0.35*gvStd)
	// 亮度异常阈值（0-255 尺度固定值）：污点相对场景是突兀色块。
	// 用固定值而非按邻域方差缩放，避免污点边缘邻域双峰把阈值撑大导致漏检。
	const brightOutlier = 25.0

	type gridCell struct{ gy, gx int }
	var bad []gridCell
	for gy := 0; gy < grid; gy++ {
		for gx := 0; gx < grid; gx++ {
			i := gy*grid + gx
			if stds[i] >= smoothThresh {
				continue // 自身不够平滑：不是污点内部
			}
			// 局部邻域（3x3 不含自身）的块均值
			var nSum float64
			nn := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					ny, nx := gy+dy, gx+dx
					if ny < 0 || ny >= grid || nx < 0 || nx >= grid {
						continue
					}
					nSum += means[ny*grid+nx]
					nn++
				}
			}
			if nn == 0 {
				continue
			}
			nMean := nSum / float64(nn)
			if math.Abs(means[i]-nMean) > brightOutlier {
				bad = append(bad, gridCell{gy, gx})
			}
		}
	}
	if len(bad) < LensDirtMinBlocks {
		return nil, 0
	}
	minx, miny, maxx, maxy := grid, grid, 0, 0
	for _, c := range bad {
		if c.gx < minx {
			minx = c.gx
		}
		if c.gx > maxx {
			maxx = c.gx
		}
		if c.gy < miny {
			miny = c.gy
		}
		if c.gy > maxy {
			maxy = c.gy
		}
	}
	bw := maxx - minx + 1
	bh := maxy - miny + 1
	// 排除细线（如地平线）：成团的脏点宽高都应 ≥ 2 块
	if bw < 2 || bh < 2 {
		return nil, 0
	}
	// 局部性约束：脏点框面积占比应 < 0.6，避免整帧统一偏色被误判
	areaRatio := float64(bw*bh) / float64(grid*grid)
	if areaRatio >= 0.6 {
		return nil, 0
	}
	// 密度约束：脏斑块是**实心**的（异常块填满包围盒的一半以上）。
	// 灰色天空与地面/建筑的交界是一条**稀疏的线**——包围盒被撑得很大
	// 但内部大多是正常块（实测天空图密度 12%~22%，验收真斑 100%）。
	density := float64(len(bad)) / float64(bw*bh)
	if density < 0.5 {
		return nil, 0
	}
	box := Box{
		X: float64(minx) / float64(grid),
		Y: float64(miny) / float64(grid),
		W: float64(bw) / float64(grid),
		H: float64(bh) / float64(grid),
	}
	return []Box{box}, float64(len(bad)) / gn
}

// pHash 计算 64-bit 感知哈希（DCT 低频二值化）。
func pHash(buf []byte, q int) uint64 {
	N := 64
	if q < N {
		N = q
	}
	if N == 0 {
		return 0
	}
	m := make([]float64, N*N)
	for i := 0; i < N*N && i < len(buf); i++ {
		m[i] = float64(buf[i])
	}
	d := dct2D(m, N)
	// 取左上 8×8 低频块（DCT 能量集中在低频，即矩阵的左上角）
	low := make([]float64, 8*8)
	for r := 0; r < 8; r++ {
		for c := 0; c < 8; c++ {
			if r*N+c < len(d) {
				low[r*8+c] = d[r*N+c]
			}
		}
	}
	vals := make([]float64, 0, 8*8-1)
	for i := 1; i < 8*8; i++ {
		vals = append(vals, low[i]) // 排除 DC（low[0]）
	}
	med := median(vals)
	var h uint64
	for i := 0; i < 8*8; i++ {
		if low[i] > med {
			h |= 1 << uint(i)
		}
	}
	return h
}

// Hamming 两个 pHash 的汉明距离。
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// ParseHash 把 16 位十六进制串解析回哈希。解析失败返回 ok=false。
func ParseHash(s string) (uint64, bool) {
	if len(s) != 16 {
		return 0, false
	}
	var v uint64
	for i := 0; i < 16; i++ {
		var d uint64
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint64(c-'A') + 10
		default:
			return 0, false
		}
		v = v<<4 | d
	}
	return v, true
}

// HashString 把哈希格式化成前端/数据库用的 16 位十六进制串。
func HashString(h uint64) string { return fmt.Sprintf("%016x", h) }

func dct1D(x []float64) []float64 {
	N := len(x)
	X := make([]float64, N)
	for k := 0; k < N; k++ {
		var s float64
		for n := 0; n < N; n++ {
			s += x[n] * math.Cos(math.Pi/float64(N)*(float64(n)+0.5)*float64(k))
		}
		c := math.Sqrt(2 / float64(N))
		if k == 0 {
			c = math.Sqrt(1 / float64(N))
		}
		X[k] = c * s
	}
	return X
}

func dct2D(m []float64, N int) []float64 {
	tmp := make([]float64, N*N)
	row := make([]float64, N)
	for i := 0; i < N; i++ {
		copy(row, m[i*N:(i+1)*N])
		r := dct1D(row)
		copy(tmp[i*N:], r)
	}
	out := make([]float64, N*N)
	col := make([]float64, N)
	for j := 0; j < N; j++ {
		for i := 0; i < N; i++ {
			col[i] = tmp[i*N+j]
		}
		c := dct1D(col)
		for i := 0; i < N; i++ {
			out[i*N+j] = c[i]
		}
	}
	return out
}

func median(vs []float64) float64 {
	s := make([]float64, len(vs))
	copy(s, vs)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// floatBits 给阈值指纹用：把 float64 写成字节。
func floatBits(v float64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	return b[:]
}

// 保证 image 包被引用（decode.go 用到，这里仅作编译期提示）。
var _ image.Image = nil
