package qc

// qc_test.go 用合成素材验证「该报的报、不该报的不报」。
//
// 合成图不是真实照片，不能用来定阈值（那是 medreview 在真实素材上做的），
// 但足以验证三件事：测量口径没写错、判据方向对、极端情况不崩。
// 真实素材校准见 README「阈值校准」一节。

import (
	"image"
	"image/color"
	"math"
	"math/rand"
	"testing"
)

// ─────────────────────── 合成素材 ───────────────────────

// checker 生成棋盘格（高频结构，代表「清晰有细节」的照片）。
func checker(w, h, cell int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(30)
			if ((x/cell)+(y/cell))%2 == 0 {
				v = 220
			}
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

// blurImage 可分离方框模糊（模拟失焦）。先水平再垂直，O(w·h·r)。
func blurImage(src *image.RGBA, radius int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	tmp := make([]int, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sum, cnt int
			for dx := -radius; dx <= radius; dx++ {
				nx := x + dx
				if nx < 0 || nx >= w {
					continue
				}
				r, _, _, _ := src.At(nx, y).RGBA()
				sum += int(r >> 8)
				cnt++
			}
			tmp[y*w+x] = sum / cnt
		}
	}
	dst := image.NewRGBA(b)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sum, cnt int
			for dy := -radius; dy <= radius; dy++ {
				ny := y + dy
				if ny < 0 || ny >= h {
					continue
				}
				sum += tmp[ny*w+x]
				cnt++
			}
			v := uint8(sum / cnt)
			dst.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return dst
}

// solid 纯色图。
func solid(w, h int, v uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

// noiseImage 在底图上叠加均匀噪声（模拟高 ISO）。
//
// 必须有「场景结构」当底：整幅都是噪声的图并不存在，而且 64×64 面积平均
// 会把纯噪声完全抹平（实测 std 从 23 掉到 1.45），先被空镜接住就测不到噪点了。
func noiseImage(base *image.RGBA, amp float64) *image.RGBA {
	b := base.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(b)
	rng := rand.New(rand.NewSource(42))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, _, _, _ := base.At(x, y).RGBA()
			v := float64(r>>8) + (rng.Float64()*2-1)*amp
			vv := uint8(math.Max(0, math.Min(255, v)))
			dst.SetRGBA(x, y, color.RGBA{vv, vv, vv, 255})
		}
	}
	return dst
}

// clipped 大面积死黑 / 死白（模拟真欠曝 / 真过曝）。
func clipped(w, h int, dark bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(7))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var v uint8
			if dark {
				// 95% 死黑 + 少量微亮，均值压在 100 以下
				if rng.Float64() < 0.95 {
					v = 0
				} else {
					v = 18
				}
			} else {
				// 95% 死白 + 少量灰，均值抬到 150 以上
				if rng.Float64() < 0.95 {
					v = 255
				} else {
					v = 200
				}
			}
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

// ─────────────────────── 空镜 ───────────────────────

func TestBlankDetected(t *testing.T) {
	a := New()
	for _, tc := range []struct {
		name string
		img  *image.RGBA
	}{
		{"纯黑", solid(800, 600, 0)},
		{"纯白", solid(800, 600, 255)},
		{"纯灰", solid(800, 600, 128)},
	} {
		r := a.AnalyzeImage(tc.img)
		if !r.Has(FlagBlank) {
			t.Errorf("%s: 应判空镜，实际 flags=%d grayStd=%.2f", tc.name, r.Flags, r.GrayStd)
		}
	}
}

func TestBlankNotTriggeredOnTexture(t *testing.T) {
	a := New()
	r := a.AnalyzeImage(checker(1200, 800, 40))
	if r.Has(FlagBlank) {
		t.Errorf("棋盘格不该判空镜，grayStd=%.2f", r.GrayStd)
	}
}

// ─────────────────────── 模糊 ───────────────────────

func TestBlurDetected(t *testing.T) {
	a := New()
	sharp := checker(1600, 1200, 24)
	blurred := blurImage(sharp, 8)

	rs := a.AnalyzeImage(sharp)
	rb := a.AnalyzeImage(blurred)

	t.Logf("清晰 lapStd=%.2f / 模糊 lapStd=%.2f（阈值 %.1f）", rs.LapStd, rb.LapStd, BlurLapStdThresh)

	if rb.LapStd >= rs.LapStd {
		t.Fatalf("模糊图的拉普拉斯标准差应低于清晰图：清晰=%.2f 模糊=%.2f", rs.LapStd, rb.LapStd)
	}
	if !rb.Has(FlagBlur) {
		t.Errorf("明显失真应判模糊，实际 lapStd=%.2f（阈值 %.1f）", rb.LapStd, BlurLapStdThresh)
	}
	if rs.Has(FlagBlur) {
		t.Errorf("清晰棋盘格不该判模糊，实际 lapStd=%.2f", rs.LapStd)
	}
}

// ─────────────────────── 噪点 ───────────────────────

func TestNoiseDetected(t *testing.T) {
	a := New()
	base := checker(1200, 900, 60)
	clean := noiseImage(base, 0)
	noisy := noiseImage(base, 45)

	rc := a.AnalyzeImage(clean)
	rn := a.AnalyzeImage(noisy)

	t.Logf("干净 noiseFloor=%.2f / 噪声 noiseFloor=%.2f（阈值 %.1f）", rc.NoiseFloor, rn.NoiseFloor, NoiseFloorThresh)

	if rn.NoiseFloor <= rc.NoiseFloor {
		t.Fatalf("噪声图的噪声底线应更高：干净=%.2f 噪声=%.2f", rc.NoiseFloor, rn.NoiseFloor)
	}
	if !rn.Has(FlagNoise) {
		t.Errorf("重噪图应判噪点，实际 noiseFloor=%.2f（阈值 %.1f）", rn.NoiseFloor, NoiseFloorThresh)
	}
	if rc.Has(FlagNoise) {
		t.Errorf("干净图不该判噪点，实际 noiseFloor=%.2f", rc.NoiseFloor)
	}
}

// ─────────────────────── 曝光 ───────────────────────

func TestExposureUnder(t *testing.T) {
	a := New()
	r := a.AnalyzeImage(clipped(1200, 800, true))
	if !r.Has(FlagExposure) || r.Exposure == nil || !r.Exposure.Under {
		t.Fatalf("大面积死黑应判欠曝：mean=%.1f clipLow=%.2f flags=%d", r.ExpMean, r.ExpClipLow, r.Flags)
	}
}

func TestExposureOver(t *testing.T) {
	a := New()
	r := a.AnalyzeImage(clipped(1200, 800, false))
	if !r.Has(FlagExposure) || r.Exposure == nil || !r.Exposure.Over {
		t.Fatalf("大面积死白应判过曝：mean=%.1f clipHigh=%.2f flags=%d", r.ExpMean, r.ExpClipHi, r.Flags)
	}
}

func TestExposureNormalNotTrigered(t *testing.T) {
	a := New()
	// 均匀中灰：均值 128，clip 两端都接近 0 —— 正常曝光
	r := a.AnalyzeImage(solid(1200, 800, 128))
	if r.Has(FlagExposure) {
		t.Errorf("均匀中灰不该判曝光异常：mean=%.1f clipLow=%.2f clipHigh=%.2f", r.ExpMean, r.ExpClipLow, r.ExpClipHi)
	}
}

// TestWhiteDocumentNotOverexposed 白底文档不该被判过曝。
// 这是 medreview 里花了不少代价才修掉的误报：截图 mean≈240、clipHigh 0.63~0.75，
// 是正常的白纸黑字，不是过曝。判据要求 clipHigh > 0.80 才成立。
func TestWhiteDocumentNotOverexposed(t *testing.T) {
	a := New()
	// 白底 + 少量黑字：85% 白、15% 黑
	img := image.NewRGBA(image.Rect(0, 0, 1200, 900))
	rng := rand.New(rand.NewSource(3))
	for y := 0; y < 900; y++ {
		for x := 0; x < 1200; x++ {
			v := uint8(250)
			if rng.Float64() < 0.15 {
				v = 20
			}
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	r := a.AnalyzeImage(img)
	if r.Has(FlagExposure) && r.Exposure != nil && r.Exposure.Over {
		t.Errorf("白底文档被误判过曝：mean=%.1f clipHigh=%.2f", r.ExpMean, r.ExpClipHi)
	}
}

// ─────────────────────── 重复 ───────────────────────

func TestDuplicateHashStable(t *testing.T) {
	a := New()
	img := checker(1200, 900, 30)
	r1 := a.AnalyzeImage(img)
	r2 := a.AnalyzeImage(img)
	if r1.DupHash == "" || r1.DupHash != r2.DupHash {
		t.Fatalf("同一张图的 pHash 应一致：%q vs %q", r1.DupHash, r2.DupHash)
	}
	h1, ok1 := ParseHash(r1.DupHash)
	h2, ok2 := ParseHash(r2.DupHash)
	if !ok1 || !ok2 || Hamming(h1, h2) != 0 {
		t.Fatalf("哈希解析或汉明距离异常")
	}
}

func TestDuplicateHashDifferentForDifferentImages(t *testing.T) {
	a := New()
	r1 := a.AnalyzeImage(checker(1200, 900, 30))
	r2 := a.AnalyzeImage(checker(1200, 900, 80)) // 不同格宽 → 结构不同
	h1, _ := ParseHash(r1.DupHash)
	h2, _ := ParseHash(r2.DupHash)
	if Hamming(h1, h2) == 0 {
		t.Errorf("结构不同的两张图 pHash 不该完全相同")
	}
}

// ─────────────────────── 阈值 ───────────────────────

func TestClampThresholds(t *testing.T) {
	in := Thresholds{BlurLapStd: -5, NoiseFloor: 999, BlankStd: 0, ExpUnder: 1e9, ExpOver: -1, DupSim: 0, BlinkEdge: 99}
	out := ClampThresholds(in)
	s := ThresholdSpecs()
	if out.BlurLapStd != s["blurLapStd"].Min {
		t.Errorf("BlurLapStd 未钳制到位: %v", out.BlurLapStd)
	}
	if out.NoiseFloor != s["noiseFloor"].Max {
		t.Errorf("NoiseFloor 未钳制到位: %v", out.NoiseFloor)
	}
	if out.BlinkEdge != s["blinkEdge"].Max {
		t.Errorf("BlinkEdge 未钳制到位: %v", out.BlinkEdge)
	}
}

func TestThresholdVersionFingerprint(t *testing.T) {
	def := DefaultThresholds()
	if got := VersionFor(def); got != Version {
		t.Errorf("默认阈值应等于算法版本: got %d want %d", got, Version)
	}
	custom := def
	custom.BlurLapStd += 1
	v := VersionFor(custom)
	if v == Version {
		t.Errorf("自定义阈值必须产生不同版本号，否则旧结果不会被重算")
	}
	// 同一套自定义阈值必须稳定
	if VersionFor(custom) != v {
		t.Errorf("同一阈值集两次计算的指纹不一致")
	}
}

func TestAnalyzerThresholdUpdate(t *testing.T) {
	a := New()
	def := a.CurrentThresholds()
	if def != DefaultThresholds() {
		t.Fatalf("初始阈值应等于默认值")
	}
	a.SetThresholds(Thresholds{BlurLapStd: 9999})
	got := a.CurrentThresholds()
	if got.BlurLapStd > ThresholdSpecs()["blurLapStd"].Max {
		t.Errorf("SetThresholds 未钳制: %v", got.BlurLapStd)
	}
}

// ─────────────────────── 损坏 / 边界 ───────────────────────

func TestCorruptedBytes(t *testing.T) {
	a := New()
	r := a.AnalyzeBytes([]byte("这不是图片，只是一段文本"))
	if !r.Has(FlagCorrupted) {
		t.Fatalf("非图片字节应判损坏，实际 flags=%d", r.Flags)
	}
	if r.Error == "" {
		t.Errorf("损坏必须带错误信息，不能静默")
	}
}

func TestNilImage(t *testing.T) {
	a := New()
	r := a.AnalyzeImage(nil)
	if !r.Has(FlagCorrupted) {
		t.Fatalf("nil 图像应判损坏")
	}
}

func TestTinyImage(t *testing.T) {
	a := New()
	r := a.AnalyzeImage(solid(8, 8, 100))
	if r.Has(FlagCorrupted) {
		t.Fatalf("8×8 的小图不该判损坏: %s", r.Error)
	}
	if r.Width != 8 || r.Height != 8 {
		t.Errorf("尺寸记录错误: %dx%d", r.Width, r.Height)
	}
}

func TestLabels(t *testing.T) {
	r := &Result{Flags: FlagBlur | FlagNoise}
	got := r.Labels()
	if len(got) != 2 || got[0] != "模糊" || got[1] != "噪点" {
		t.Errorf("标签输出异常: %v", got)
	}
	if !r.Has(FlagBlur) || r.Has(FlagBlank) {
		t.Errorf("Has 判定异常")
	}
}

// TestIssueMaskExcludesDup 重复不算「质量问题」。
func TestIssueMaskExcludesDup(t *testing.T) {
	r := &Result{Flags: FlagDup}
	if r.AnyIssue() {
		t.Errorf("重复只是冗余，不该混进问题件过滤")
	}
	r2 := &Result{Flags: FlagBlur}
	if !r2.AnyIssue() {
		t.Errorf("模糊应算质量问题")
	}
}
