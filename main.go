// PhotoClassifierPro —— 照片质量批量筛片工具（Go 版）。
//
// 一个 exe，零依赖：不需要 Python、不需要 OpenCV、不需要 ffmpeg。
// 内置 8 个检测器（损坏 / 空镜 / 模糊 / 噪点 / 曝光 / 镜头脏污 / 重复 / 闭眼），
// 算法与阈值来自 medreview 的 QC v5（真实素材校准）。
//
// 三种用法：
//
//	PhotoClassifierPro.exe                   浏览器界面（默认，双击即用）
//	PhotoClassifierPro.exe -scan D:\photos   命令行扫描，输出报告
//	PhotoClassifierPro.exe -scan D:\photos -move  扫描并归档废片
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shenglihalisen/PhotoClassifierPro/engine"
	"github.com/shenglihalisen/PhotoClassifierPro/face"
	"github.com/shenglihalisen/PhotoClassifierPro/qc"
	"github.com/shenglihalisen/PhotoClassifierPro/web"
)

// 版本号：与 CHANGELOG 对应。
const version = "v3.0.0-go"

func main() {
	var (
		scanDir   = flag.String("scan", "", "扫描这个目录并输出报告（不启动界面）")
		jsonOut   = flag.Bool("json", false, "结果以 JSON 输出到 stdout")
		csvOut    = flag.String("csv", "", "把结果写成 CSV 到指定文件")
		doMove    = flag.Bool("move", false, "把废片移动到 _待整理/<缺陷>/ 下（默认只报告不动文件）")
		doCopy    = flag.Bool("copy", false, "把废片复制到 _待整理/<缺陷>/ 下")
		recursive = flag.Bool("r", true, "递归子目录")
		withFace  = flag.Bool("face", true, "启用闭眼检测（较慢，可用 -face=false 关闭）")
		workers   = flag.Int("j", 0, "并发数，0=按 CPU 自动")
		port      = flag.Int("port", 8080, "Web 界面端口")
		noBrowser = flag.Bool("no-browser", false, "启动 Web 服务后不自动打开浏览器")
		showVer   = flag.Bool("version", false, "打印版本号")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Printf("PhotoClassifierPro %s (%s/%s, go %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	// 闭眼检测器：整个进程共用一个（Haar 级联解析一次，之后只读）
	if *withFace {
		engine.SetFaceFactory(func() qc.FaceDetector {
			return face.NewBlink(qc.DefaultThresholds().BlinkEdge)
		})
	}

	if *scanDir != "" {
		os.Exit(runScan(*scanDir, *jsonOut, *csvOut, *doMove, *doCopy, *recursive, *withFace, *workers))
	}

	// 默认：Web 界面
	if err := web.Serve(web.Config{
		Addr:      fmt.Sprintf("127.0.0.1:%d", *port),
		Version:   version,
		OpenBrowser: !*noBrowser,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `PhotoClassifierPro %s —— 照片质量批量筛片

用法:
  PhotoClassifierPro.exe                        启动浏览器界面（默认）
  PhotoClassifierPro.exe -scan <目录>            命令行扫描并打印报告
  PhotoClassifierPro.exe -scan <目录> -move      扫描并把废片移到 _待整理/
  PhotoClassifierPro.exe -scan <目录> -csv r.csv 结果写成 CSV

参数:
`, version)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
检测器: 损坏 / 空镜 / 模糊 / 噪点 / 欠曝过曝 / 镜头脏污 / 重复 / 闭眼
`)
}

func runScan(dir string, asJSON bool, csvPath string, move, copyMode, recursive, withFace bool, workers int) int {
	root, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "路径无效: %v\n", err)
		return 2
	}
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "不是有效目录: %s\n", root)
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	start := time.Now()
	var lastLine time.Time
	res, err := engine.Scan(ctx, engine.Options{
		Root:       root,
		Workers:    workers,
		Recursive:  recursive,
		Face:       withFace,
		Thresholds: qc.DefaultThresholds(),
	}, func(p engine.Progress) {
		if asJSON {
			return // JSON 模式下不污染 stdout
		}
		// 进度刷新限流：每 100ms 一行，避免刷屏拖慢扫描
		if time.Since(lastLine) < 100*time.Millisecond {
			return
		}
		lastLine = time.Now()
		fmt.Fprintf(os.Stderr, "\r[%d/%d] %-50s 问题 %d", p.Done, p.Total, truncate(p.Current, 50), p.Issues)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n扫描中断: %v\n", err)
		return 1
	}
	if !asJSON {
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", 90))
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		printReport(res, time.Since(start))
	}

	if csvPath != "" {
		if err := writeCSV(csvPath, res); err != nil {
			fmt.Fprintf(os.Stderr, "写 CSV 失败: %v\n", err)
			return 1
		}
		fmt.Printf("CSV 已写入: %s\n", csvPath)
	}

	if move || copyMode {
		mode := engine.ModeMove
		if copyMode {
			mode = engine.ModeCopy
		}
		actions := engine.Plan(res.Items, engine.OrganizeOptions{Root: root, Mode: mode})
		if len(actions) == 0 {
			fmt.Println("没有需要归档的文件。")
			return 0
		}
		fmt.Printf("\n%s %d 个文件到 _待整理/ ...\n", map[bool]string{true: "复制", false: "移动"}[copyMode], len(actions))
		done := engine.Execute(actions, mode)
		fmt.Printf("完成 %d/%d。\n", done, len(actions))
	}
	return 0
}

func printReport(res *engine.Result, elapsed time.Duration) {
	s := res.Summary
	fmt.Printf("\n扫描完成：共 %d 张，用时 %.1fs\n", s.Total, elapsed.Seconds())
	fmt.Printf("  质量问题 %d 张   损坏 %d 张   重复 %d 张（%d 组）\n",
		s.Issues, s.Failed, s.Duplicate, len(res.Groups))

	if len(s.ByLabel) > 0 {
		type kv struct {
			k string
			v int
		}
		var arr []kv
		for k, v := range s.ByLabel {
			arr = append(arr, kv{k, v})
		}
		sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
		fmt.Print("  分布: ")
		for i, e := range arr {
			if i > 0 {
				fmt.Print("  ")
			}
			fmt.Printf("%s=%d", e.k, e.v)
		}
		fmt.Println()
	}

	// 列出前 20 张有问题的
	n := 0
	for _, it := range res.Items {
		if !it.IsIssue() {
			continue
		}
		if n >= 20 {
			fmt.Printf("  ... 还有更多，用 -csv 导出完整清单\n")
			break
		}
		n++
		fmt.Printf("  [%s] %s\n", strings.Join(it.Labels(), ","), it.Rel)
	}
	if s.Issues == 0 && s.Duplicate == 0 {
		fmt.Println("  没发现问题。")
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}
