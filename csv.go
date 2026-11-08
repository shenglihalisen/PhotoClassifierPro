package main

// csv.go 把扫描结果写成 CSV（Excel 直接打开，UTF-8 BOM 避免中文乱码）。

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/shenglihalisen/PhotoClassifierPro/engine"
)

func writeCSV(path string, res *engine.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// UTF-8 BOM：不加的话 Excel 会把中文读成乱码
	if _, err := f.WriteString("\xEF\xBB\xBF"); err != nil {
		return err
	}
	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"文件", "大小(KB)", "问题", "重复组",
		"拉普拉斯标准差", "噪声底线", "亮度均值", "近黑占比", "近白占比",
		"人脸数", "闭眼数", "眼部最小边缘密度",
		"宽", "高", "错误",
	}
	if err := w.Write(header); err != nil {
		return err
	}

	for _, it := range res.Items {
		if it == nil || it.Result == nil {
			continue
		}
		r := it.Result
		labels := strings.Join(r.Labels(), " ")
		if labels == "" {
			labels = "正常"
		}
		faces, closed := 0, 0
		minEdge := 0.0
		if r.Blink != nil {
			faces, closed, minEdge = r.Blink.Faces, r.Blink.Closed, r.Blink.MinEdge
		}
		row := []string{
			it.Rel,
			strconv.FormatInt(it.Size/1024, 10),
			labels,
			strconv.Itoa(it.DupGroup),
			fmt.Sprintf("%.2f", r.LapStd),
			fmt.Sprintf("%.2f", r.NoiseFloor),
			fmt.Sprintf("%.1f", r.ExpMean),
			fmt.Sprintf("%.3f", r.ExpClipLow),
			fmt.Sprintf("%.3f", r.ExpClipHi),
			strconv.Itoa(faces),
			strconv.Itoa(closed),
			fmt.Sprintf("%.4f", minEdge),
			strconv.Itoa(r.Width),
			strconv.Itoa(r.Height),
			r.Error,
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}
