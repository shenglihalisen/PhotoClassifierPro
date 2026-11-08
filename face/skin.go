package face

// skin.go 肤色预筛。
//
// 目的只有一个：让「明显没人脸」的图在 2 毫秒内出局，不必去跑 0.2 秒的
// 金字塔扫描。风景、建筑、文档、截图这些占相册的绝大多数。
//
// 判据用 YCbCr 的经典肤色区间（Cr 133~173、Cb 77~127）。这个区间宽松得离谱，
// 但在这里恰好合适：它是**放行**判据，宁可多放一些图进正式检测，
// 也不能把人像挡在门外。真正的精度判断交给 Viola-Jones。

import "image"

// skinRatio 统计肤色像素占比（在 64×64 的缩略图上数）。
func skinRatio(img image.Image) float64 {
	if img == nil {
		return 0
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return 0
	}
	const n = 64
	skin := 0
	for dy := 0; dy < n; dy++ {
		sy := b.Min.Y + dy*h/n
		for dx := 0; dx < n; dx++ {
			sx := b.Min.X + dx*w/n
			r16, g16, b16, _ := img.At(sx, sy).RGBA()
			r := float64(r16 >> 8)
			g := float64(g16 >> 8)
			bl := float64(b16 >> 8)
			cb := 128 - 0.168736*r - 0.331264*g + 0.5*bl
			cr := 128 + 0.5*r - 0.418688*g - 0.081312*bl
			if cr >= 133 && cr <= 173 && cb >= 77 && cb <= 127 {
				skin++
			}
		}
	}
	return float64(skin) / float64(n*n)
}
