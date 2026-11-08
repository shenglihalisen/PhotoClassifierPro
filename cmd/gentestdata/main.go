// gentestdata 生成一组合成测试照片，用来验收检测器方向是否正确。
//
//	gentestdata -out ./testdata
//
// 合成图只能验证「该报的报、不该报的不报」，不能用来定阈值
// （阈值是 medreview 在真实素材上校准的，见 README）。
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "testdata", "输出目录")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	save := func(name string, img image.Image) {
		p := filepath.Join(*out, name)
		f, err := os.Create(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		defer f.Close()
		if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Println("wrote", p)
	}

	// 场景图：上部渐变天空（大面积平坦）+ 下部纹理地面 + 几条锐利边缘。
	// 必须带平坦区，否则「噪声底线取最平坦 10%」这条判据无从落地 ——
	// 纯棋盘格是极端人造纹理，真实相片里不存在。
	scene := makeScene(1400, 1000)
	save("ok_scene.jpg", scene)
	save("bad_blur.jpg", boxBlur(scene, 7))
	save("bad_blank.jpg", solid(1200, 900, 128))
	save("bad_dark.jpg", clipped(1200, 900, true))
	save("bad_bright.jpg", clipped(1200, 900, false))
	save("bad_noise.jpg", addNoise(makeScene(1200, 900), 46))
	save("dup_a.jpg", scene)
	save("dup_b.jpg", scene)
}

func checker(w, h, cell int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(28)
			if ((x/cell)+(y/cell))%2 == 0 {
				v = 228
			}
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func solid(w, h int, v uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func boxBlur(src *image.RGBA, r int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	tmp := make([]int, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var s, c int
			for d := -r; d <= r; d++ {
				nx := x + d
				if nx < 0 || nx >= w {
					continue
				}
				cr, _, _, _ := src.At(nx, y).RGBA()
				s += int(cr >> 8)
				c++
			}
			tmp[y*w+x] = s / c
		}
	}
	dst := image.NewRGBA(b)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var s, c int
			for d := -r; d <= r; d++ {
				ny := y + d
				if ny < 0 || ny >= h {
					continue
				}
				s += tmp[ny*w+x]
				c++
			}
			v := uint8(s / c)
			dst.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return dst
}

func clipped(w, h int, dark bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(11))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var v uint8
			if dark {
				if rng.Float64() < 0.96 {
					v = 0
				} else {
					v = 16
				}
			} else {
				if rng.Float64() < 0.96 {
					v = 255
				} else {
					v = 205
				}
			}
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func addNoise(base *image.RGBA, amp float64) *image.RGBA {
	b := base.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(b)
	rng := rand.New(rand.NewSource(5))
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

// makeScene 构造一张「像照片」的测试图：
//   - 上部 55%：天空渐变（大面积平坦，用来测噪声底线）
//   - 下部 45%：细密纹理的地面（提供高频）
//   - 中间几条锐利横线（建筑轮廓，提供强边缘）
func makeScene(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	horizon := int(float64(h) * 0.55)
	rng := rand.New(rand.NewSource(9))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var v float64
			if y < horizon {
				// 天空：自上而下缓慢变亮
				v = 90 + 70*float64(y)/float64(horizon)
			} else {
				// 地面：中灰 + 细密随机纹理（不是纯噪声，幅度小）
				base := 70 + 25*float64(y-horizon)/float64(h-horizon)
				v = base + (rng.Float64()*2-1)*9
			}
			vv := uint8(math.Max(0, math.Min(255, v)))
			img.SetRGBA(x, y, color.RGBA{vv, vv, vv, 255})
		}
	}
	// 地平线上加一条锐利的暗线（强边缘，保证 lapStd 高）
	for x := 0; x < w; x++ {
		for dy := 0; dy < 3; dy++ {
			img.SetRGBA(x, horizon+dy, color.RGBA{20, 20, 20, 255})
		}
	}
	// 几根竖直柱子（结构边缘）
	for _, cx := range []int{w / 5, w / 2, w * 4 / 5} {
		for y := 0; y < h; y++ {
			for dx := 0; dx < 4; dx++ {
				img.SetRGBA(cx+dx, y, color.RGBA{235, 235, 235, 255})
			}
		}
	}
	return img
}
