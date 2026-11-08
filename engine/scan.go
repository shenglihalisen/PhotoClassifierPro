// Package engine 批量扫描编排：遍历目录 → 并发检测 → 聚合重复 → 输出结果。
//
// 两条硬规则：
//   - 源文件只读。扫描阶段绝不移动、改名、删除任何文件；
//     整理动作要用户显式点（见 organize.go），而且默认是干跑。
//   - 「没测成」和「测过没问题」必须能区分。任何解码失败都会留下 Error，
//     不会静默算作一张好照片。
package engine

// scan.go 扫描编排。

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shenglihalisen/PhotoClassifierPro/qc"
)

// Options 扫描参数。
type Options struct {
	// Root 扫描根目录。
	Root string
	// Workers 并发解码/检测的协程数，0 表示按 CPU 核数自动定。
	Workers int
	// Recursive 是否递归子目录。
	Recursive bool
	// Face 是否跑人脸/闭眼检测（较慢，风景图会被肤色预筛跳过）。
	Face bool
	// Thresholds 判定阈值（零值表示用默认）。
	Thresholds qc.Thresholds
	// GlobalDup 是否启用跨扫描重复索引（默认关，需要显式开）。
	GlobalDup bool
	// IndexPath 索引文件路径；空则用 DefaultIndexPath()。
	IndexPath string
}

// Item 一张照片的扫描结果。
type Item struct {
	Path    string     `json:"path"`
	Rel     string     `json:"rel"`
	Size    int64      `json:"size"`
	ModTime time.Time  `json:"modTime"`
	Result  *qc.Result `json:"result"`
	// DupGroup 重复组编号（0 表示不重复），同一组内的照片内容相同。
	DupGroup int `json:"dupGroup,omitempty"`
}

// Labels 命中的缺陷名（转发给前端/CSV）。
func (it *Item) Labels() []string {
	if it == nil || it.Result == nil {
		return nil
	}
	return it.Result.Labels()
}

// IsIssue 是否命中任何质量问题（不含重复）。
func (it *Item) IsIssue() bool {
	return it != nil && it.Result != nil && it.Result.AnyIssue()
}

// Progress 扫描进度（回调给 UI）。
type Progress struct {
	Total   int           `json:"total"`
	Done    int           `json:"done"`
	Current string        `json:"current"`
	Issues  int           `json:"issues"`
	Elapsed time.Duration `json:"elapsed"`
	// Failed 解码失败/损坏的数量。
	Failed int `json:"failed"`
}

// Summary 扫描汇总。
type Summary struct {
	Total     int `json:"total"`
	Issues    int `json:"issues"`
	Failed    int `json:"failed"`
	Duplicate int `json:"duplicate"`
	// Cross 跨目录重复候选数（仅开启全库去重时非 0）。
	Cross     int            `json:"cross"`
	ByLabel   map[string]int `json:"byLabel"`
	ElapsedMS int64          `json:"elapsedMs"`
}

// Scan 扫描目录并逐张检测。
//
// onProgress 会在每次完成一张时被调用（可为 nil）。调用保证是串行的，
// 所以 UI 侧不用自己加锁。
func Scan(ctx context.Context, opt Options, onProgress func(Progress)) (*Result, error) {
	if opt.Workers <= 0 {
		opt.Workers = defaultWorkers()
	}
	files, err := collectFiles(opt)
	if err != nil {
		return nil, err
	}

	a := qc.New()
	if opt.Thresholds != (qc.Thresholds{}) {
		a.SetThresholds(opt.Thresholds)
	}
	if opt.Face {
		// 人脸检测器由 main 侧注入（face 包依赖放在上层，避免 qc ↔ face 循环）
		if factory != nil {
			a.SetFace(factory())
		}
	}

	start := time.Now()
	total := len(files)
	items := make([]*Item, total)
	var done, issues, failed int64

	// 任务队列
	type job struct {
		idx  int
		path string
		rel  string
		size int64
		mod  time.Time
	}
	jobs := make(chan job)

	var wg sync.WaitGroup
	for w := 0; w < opt.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				res := a.AnalyzeFile(j.path)
				it := &Item{Path: j.path, Rel: j.rel, Size: j.size, ModTime: j.mod, Result: res}
				items[j.idx] = it

				n := atomic.AddInt64(&done, 1)
				if res.Has(qc.FlagCorrupted) || res.Error != "" {
					atomic.AddInt64(&failed, 1)
				}
				if res.AnyIssue() {
					atomic.AddInt64(&issues, 1)
				}
				if onProgress != nil {
					onProgress(Progress{
						Total:   total,
						Done:    int(n),
						Current: j.rel,
						Issues:  int(atomic.LoadInt64(&issues)),
						Failed:  int(atomic.LoadInt64(&failed)),
						Elapsed: time.Since(start),
					})
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i, f := range files {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{idx: i, path: f.abs, rel: f.rel, size: f.size, mod: f.mod}:
			}
		}
	}()

	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 去掉因取消而未处理的空位
	out := items[:0]
	for _, it := range items {
		if it != nil {
			out = append(out, it)
		}
	}

	// 重复检测（跨文件）
	dupGroups := DetectDuplicates(out)
	for _, g := range dupGroups {
		for _, it := range g.Items {
			it.DupGroup = g.ID
			it.Result.Flags |= qc.FlagDup
		}
	}

	sum := buildSummary(out, dupGroups, start)
	res := &Result{Items: out, Groups: dupGroups, Summary: sum}

	if opt.GlobalDup {
		idxPath := opt.IndexPath
		if idxPath == "" {
			idxPath = DefaultIndexPath()
		}
		idx := LoadDupIndex(idxPath)
		res.CrossDups = idx.MatchCross(out, opt.Root)
		idx.Update(out, opt.Root)
		// 索引写失败不该让扫描白跑：结果已经拿到了，下次再补写
		_ = idx.Save(idxPath)
		sum.Cross = len(res.CrossDups)
		res.Summary = sum
	}
	return res, nil
}

// Result 扫描结果。
type Result struct {
	Items  []*Item     `json:"items"`
	Groups []*DupGroup `json:"groups"`
	// CrossDups 与历史扫描目录重复的照片（候选，仅开启全库去重时有值）。
	CrossDups []CrossDup `json:"crossDups,omitempty"`
	Summary   Summary    `json:"summary"`
}

type fileEntry struct {
	abs, rel string
	size     int64
	mod      time.Time
}

// collectFiles 遍历目录收集图片文件。
//
// 这里只按扩展名预筛，不做内容嗅探：真正的判断在解码阶段，
// 解不开就记成「损坏」，用户能看见而不是被悄悄跳过。
func collectFiles(opt Options) ([]fileEntry, error) {
	var out []fileEntry
	root := filepath.Clean(opt.Root)
	walk := func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// 单个目录没权限不该让整个扫描失败
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if !opt.Recursive {
				return filepath.SkipDir
			}
			name := d.Name()
			// 跳过明显的系统目录，避免扫进回收站之类的地方
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "@eaDir" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !qc.SupportedExts[ext] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		out = append(out, fileEntry{abs: path, rel: rel, size: info.Size(), mod: info.ModTime()})
		return nil
	}
	if err := filepath.WalkDir(root, walk); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

func buildSummary(items []*Item, groups []*DupGroup, start time.Time) Summary {
	s := Summary{Total: len(items), ByLabel: map[string]int{}}
	for _, it := range items {
		if it.Result == nil {
			continue
		}
		if it.Result.Has(qc.FlagCorrupted) || it.Result.Error != "" {
			s.Failed++
		}
		if it.Result.AnyIssue() {
			s.Issues++
		}
		for _, l := range it.Result.Labels() {
			s.ByLabel[l]++
		}
	}
	for _, g := range groups {
		s.Duplicate += len(g.Items)
	}
	s.ElapsedMS = time.Since(start).Milliseconds()
	return s
}
