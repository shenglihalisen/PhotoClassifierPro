// calibrate 用真实人像素材校准闭眼阈值。
//
// 默认阈值 0.10 是在合成素材上定出来的方向值，真实照片上的分布可能不一样。
// 这个工具量出「每张脸左右眼边缘密度较小值」的分位数，再按可接受的误报率
// 给出推荐阈值。
//
//	# 有标注：把睁眼、闭眼照片分放两个目录
//	calibrate -open ./open -closed ./closed
//
//	# 没标注：只看全体分布，自己挑一个保守值
//	calibrate -scan ./portraits
//
// 结果只打印到 stdout，不改写任何配置文件 —— 改不改阈值由人决定。
package main

import (
	"flag"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shenglihalisen/PhotoClassifierPro/face"
	"github.com/shenglihalisen/PhotoClassifierPro/qc"
)

func main() {
	openDir := flag.String("open", "", "睁眼照片目录")
	closedDir := flag.String("closed", "", "闭眼照片目录")
	scanDir := flag.String("scan", "", "无标注目录（只输出分布）")
	flag.Parse()

	if *scanDir != "" {
		vals := collect(*scanDir)
		if len(vals) == 0 {
			fmt.Println("没有检到人脸，检查目录内容和图片格式。")
			os.Exit(1)
		}
		fmt.Printf("全部样本：%d 张脸\n", len(vals))
		printDist(vals)
		fmt.Println("\n没有标注，无法算最优阈值。经验建议取 p5~p10 之间（宁漏勿滥）。")
		return
	}
	if *openDir == "" || *closedDir == "" {
		fmt.Fprintln(os.Stderr, "需要 -open 和 -closed 两个目录，或者用 -scan 只看分布")
		flag.Usage()
		os.Exit(2)
	}

	openVals := collect(*openDir)
	closedVals := collect(*closedDir)
	fmt.Printf("睁眼样本：%d 张脸\n", len(openVals))
	printDist(openVals)
	fmt.Printf("\n闭眼样本：%d 张脸\n", len(closedVals))
	printDist(closedVals)

	if len(openVals) == 0 || len(closedVals) == 0 {
		fmt.Println("\n样本不足，无法给出推荐阈值。")
		return
	}
	fmt.Println("\n── 阈值搜索（目标：闭眼召回 ≥90%，睁眼误报 ≤2%）──")
	best := 0.0
	bestRecall := 0.0
	for th := 0.02; th <= 0.30; th += 0.005 {
		fp := 0
		for _, v := range openVals {
			if v < th {
				fp++
			}
		}
		tp := 0
		for _, v := range closedVals {
			if v < th {
				tp++
			}
		}
		recall := float64(tp) / float64(len(closedVals))
		fpr := float64(fp) / float64(len(openVals))
		if fpr <= 0.02 && recall >= 0.90 {
			best = th
			bestRecall = recall
		}
	}
	if best > 0 {
		fmt.Printf("推荐 blinkEdge = %.3f（该点闭眼召回 %.1f%%，睁眼误报 ≤2%%）\n", best, bestRecall*100)
		fmt.Printf("把 web/static/index.html 与 qc/thresholds.go 里 DefaultThresholds 的 BlinkEdge 改成 %.3f\n", best)
	} else {
		// 退化：找误报最低且至少能抓到一半闭眼的阈值
		for th := 0.02; th <= 0.30; th += 0.005 {
			fp := 0
			for _, v := range openVals {
				if v < th {
					fp++
				}
			}
			tp := 0
			for _, v := range closedVals {
				if v < th {
					tp++
				}
			}
			recall := float64(tp) / float64(len(closedVals))
			if float64(fp)/float64(len(openVals)) <= 0.02 && recall > bestRecall {
				best, bestRecall = th, recall
			}
		}
		fmt.Printf("两类分布重叠，无法同时满足 90%% 召回 + 2%% 误报。\n")
		fmt.Printf("保守可用 blinkEdge = %.3f（召回 %.1f%%）；或考虑换关键点模型。\n", best, bestRecall*100)
	}
}

// collect 扫一个目录，返回每张检出人脸的 min(左眼,右眼) 边缘密度。
func collect(dir string) []float64 {
	b := face.NewBlink(0.10)
	var out []float64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !qc.SupportedExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		img, err := qc.DecodeFile(path)
		if err != nil {
			return nil
		}
		out = append(out, b.FaceMinEdge(img)...)
		return nil
	})
	sort.Float64s(out)
	return out
}

func printDist(v []float64) {
	if len(v) == 0 {
		fmt.Println("  （无样本）")
		return
	}
	sort.Float64s(v)
	q := func(p float64) float64 {
		i := int(p * float64(len(v)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(v) {
			i = len(v) - 1
		}
		return v[i]
	}
	fmt.Printf("  p5=%.4f  p10=%.4f  p25=%.4f  p50=%.4f  p75=%.4f  p90=%.4f  p95=%.4f  max=%.4f\n",
		q(.05), q(.10), q(.25), q(.50), q(.75), q(.90), q(.95), v[len(v)-1])
}

var _ image.Image
