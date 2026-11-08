package face

// blink_test.go 验证级联解析、积分图、边缘密度，以及（可选的）真实人像检测。
//
// 真实人像断言默认跳过：仓库里不放带肖像权的照片。
// 想跑完整验证，自己指一张：
//
//	FACE_TEST_IMG=/path/to/portrait.jpg go test ./face/ -run RealFace -v

import (
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"testing"
)

// TestParseCascade 级联模型必须能解析出完整结构。
func TestParseCascade(t *testing.T) {
	c, err := parseCascade(frontalFaceXML)
	if err != nil {
		t.Fatalf("解析内嵌级联失败: %v", err)
	}
	if c.WinW != 20 || c.WinH != 20 {
		t.Errorf("窗口尺寸异常: %dx%d", c.WinW, c.WinH)
	}
	if len(c.Stages) < 10 {
		t.Errorf("stage 数量异常: %d", len(c.Stages))
	}
	if len(c.Features) < 100 {
		t.Errorf("feature 数量异常: %d", len(c.Features))
	}
	for i, st := range c.Stages {
		if len(st.Weak) == 0 {
			t.Errorf("stage %d 没有弱分类器", i)
		}
		for j, w := range st.Weak {
			if len(w.Nodes) == 0 {
				t.Errorf("stage %d weak %d 没有节点", i, j)
			}
			if len(w.Leaves) == 0 {
				t.Errorf("stage %d weak %d 没有叶子值", i, j)
			}
			for k, n := range w.Nodes {
				if n.FeatureIndex < 0 || n.FeatureIndex >= len(c.Features) {
					t.Errorf("stage %d weak %d node %d 的特征索引越界: %d", i, j, k, n.FeatureIndex)
				}
				// 叶子索引必须落在 leafValues 范围内
				if n.Left <= 0 && -n.Left >= len(w.Leaves) {
					t.Errorf("stage %d weak %d node %d 的 left 叶子索引越界: %d", i, j, k, n.Left)
				}
				if n.Right <= 0 && -n.Right >= len(w.Leaves) {
					t.Errorf("stage %d weak %d node %d 的 right 叶子索引越界: %d", i, j, k, n.Right)
				}
			}
		}
	}
	t.Logf("级联: %d stages / %d features / 窗口 %dx%d", len(c.Stages), len(c.Features), c.WinW, c.WinH)
}

// TestIntegral 积分图必须和逐像素求和一致。
func TestIntegral(t *testing.T) {
	w, h := 17, 13
	pix := make([]byte, w*h)
	for i := range pix {
		pix[i] = byte(i % 251)
	}
	g := &grayImg{Pix: pix, W: w, H: h}
	in := newIntegral(g)

	for _, tc := range []struct{ x, y, w, h int }{
		{0, 0, 1, 1}, {0, 0, w, h}, {3, 2, 5, 4}, {w - 2, h - 2, 2, 2},
	} {
		var want int32
		for yy := tc.y; yy < tc.y+tc.h && yy < h; yy++ {
			for xx := tc.x; xx < tc.x+tc.w && xx < w; xx++ {
				want += int32(pix[yy*w+xx])
			}
		}
		if got := in.sum(tc.x, tc.y, tc.w, tc.h); got != want {
			t.Errorf("sum(%d,%d,%d,%d) = %d, want %d", tc.x, tc.y, tc.w, tc.h, got, want)
		}
	}
}

// TestEdgeDensity 纯色区域边缘密度必须是 0，高频图案必须显著大于 0。
func TestEdgeDensity(t *testing.T) {
	w, h := 200, 200
	flat := &grayImg{Pix: make([]byte, w*h), W: w, H: h}
	if d := subEdgeDensity(flat, 20, 20, 100, 100); d != 0 {
		t.Errorf("纯色区域边缘密度应为 0，实际 %.4f", d)
	}

	// 竖条纹：每 4px 一条黑白边
	striped := &grayImg{Pix: make([]byte, w*h), W: w, H: h}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x/4)%2 == 0 {
				striped.Pix[y*w+x] = 240
			}
		}
	}
	d := subEdgeDensity(striped, 20, 20, 100, 100)
	if d < 0.1 {
		t.Errorf("条纹区域边缘密度应显著大于 0，实际 %.4f", d)
	}
	t.Logf("条纹边缘密度 = %.4f", d)
}

// TestBlinkDetectorNoFace 纯色图检不出人脸，且不 panic。
func TestBlinkDetectorNoFace(t *testing.T) {
	b := NewBlink(0.10)
	if !b.Available() {
		t.Fatalf("内嵌模型应可用")
	}
	img := image.NewRGBA(image.Rect(0, 0, 400, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 400; x++ {
			img.SetRGBA(x, y, color.RGBA{90, 90, 90, 255})
		}
	}
	faces, closed, minEdge := b.DetectFaces(img)
	if faces != 0 || closed != 0 || minEdge != 0 {
		t.Errorf("纯色图不该检出人脸: faces=%d closed=%d minEdge=%.3f", faces, closed, minEdge)
	}
}

// TestRealFace 真实人像检测（需 FACE_TEST_IMG 指定图片）。
func TestRealFace(t *testing.T) {
	p := os.Getenv("FACE_TEST_IMG")
	if p == "" {
		t.Skip("设置 FACE_TEST_IMG 指向一张真实人像以运行")
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("打开图片失败: %v", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	b := NewBlink(0.10)
	faces, closed, minEdge := b.DetectFaces(img)
	t.Logf("检出人脸 %d 个，闭眼 %d 个，最小眼部边缘密度 %.4f", faces, closed, minEdge)
	if faces == 0 {
		t.Fatalf("这张图明显有人脸，但一个都没检出 —— Haar 实现有问题")
	}
}
