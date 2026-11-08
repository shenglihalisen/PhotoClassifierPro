package engine

// dupindex.go 跨扫描的重复索引。
//
// 单次扫描内的重复检测见 dup.go；这里解决的是「上次扫了 D:/2025，
// 这次扫 D:/2026，两边有没有同一张照片」。
//
// 索引存在用户配置目录，不往照片库里写文件。默认关闭，要显式开 -global-dup
// 才启用 —— 悄悄在后台建全盘索引不是这个工具该干的事。
//
// 判据比单次扫描更严（汉明 ≤2 且不做像素二次确认），结果标成**候选**：
// 跨目录没法保证两边文件此刻都还在，也不能为了确认就去解码几百张历史图。
// 宁可少报几个，也不要给人一堆「可能重复」的噪音。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/shenglihalisen/PhotoClassifierPro/qc"
)

const crossHammingMax = 2

// IndexEntry 一条历史记录。
type IndexEntry struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Mod  int64  `json:"mod"`
	Root string `json:"root"`
}

// DupIndex 持久索引。
type DupIndex struct {
	Version int                    `json:"version"`
	Entries map[string]*IndexEntry `json:"entries"`
}

// CrossDup 一条跨目录重复候选。
type CrossDup struct {
	Current  string `json:"current"`  // 本次扫描里的相对路径
	Other    string `json:"other"`    // 历史扫描里的绝对路径
	Distance int    `json:"distance"` // pHash 汉明距离
}

var indexMu sync.Mutex

// DefaultIndexPath 索引文件的默认位置。
func DefaultIndexPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "photoclassifier", "dupindex.json")
}

// LoadDupIndex 读索引；文件不存在或损坏时返回空索引，不让扫描因此失败。
func LoadDupIndex(path string) *DupIndex {
	empty := &DupIndex{Version: qc.Version, Entries: map[string]*IndexEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var idx DupIndex
	if err := json.Unmarshal(data, &idx); err != nil || idx.Entries == nil {
		return empty
	}
	// 算法版本变了，旧哈希口径不同，整份作废
	if idx.Version != qc.Version {
		return empty
	}
	return &idx
}

// Save 原子写回，避免写一半留下坏 JSON。
func (i *DupIndex) Save(path string) error {
	indexMu.Lock()
	defer indexMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(i)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MatchCross 在索引里找「其他目录」中与本次扫描重复的照片。
//
// 用和单次扫描相同的 4 段 LSH 分桶，只是候选来自历史条目。
func (i *DupIndex) MatchCross(items []*Item, root string) []CrossDup {
	type hist struct {
		e *IndexEntry
		h uint64
	}
	var hs []hist
	for _, e := range i.Entries {
		if e == nil || filepath.Clean(e.Root) == filepath.Clean(root) {
			continue // 同一目录的由本次扫描内的去重处理
		}
		h, ok := qc.ParseHash(e.Hash)
		if !ok {
			continue
		}
		hs = append(hs, hist{e, h})
	}
	if len(hs) == 0 {
		return nil
	}

	buckets := make([]map[uint16][]int, 4)
	for k := range buckets {
		buckets[k] = map[uint16][]int{}
	}
	for n, x := range hs {
		for s := 0; s < 4; s++ {
			key := uint16((x.h >> (16 * s)) & 0xFFFF)
			buckets[s][key] = append(buckets[s][key], n)
		}
	}

	var out []CrossDup
	seen := map[string]bool{}
	for _, it := range items {
		if it == nil || it.Result == nil {
			continue
		}
		h, ok := qc.ParseHash(it.Result.DupHash)
		if !ok {
			continue
		}
		best := -1
		bestD := 99
		for s := 0; s < 4; s++ {
			key := uint16((h >> (16 * s)) & 0xFFFF)
			for _, n := range buckets[s][key] {
				d := qc.Hamming(h, hs[n].h)
				if d <= crossHammingMax && d < bestD {
					best, bestD = n, d
				}
			}
		}
		if best < 0 {
			continue
		}
		key := it.Path + "|" + hs[best].e.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, CrossDup{Current: it.Rel, Other: hs[best].e.Path, Distance: bestD})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Current < out[b].Current })
	return out
}

// Update 把本次扫描结果并进索引（同路径覆盖，文件已删除的条目暂不清理）。
func (i *DupIndex) Update(items []*Item, root string) {
	for _, it := range items {
		if it == nil || it.Result == nil || it.Result.DupHash == "" {
			continue
		}
		i.Entries[it.Path] = &IndexEntry{
			Path: it.Path,
			Hash: it.Result.DupHash,
			Size: it.Size,
			Mod:  it.ModTime.UnixNano(),
			Root: root,
		}
	}
}

// IndexStats 给 CLI 报告用的一行摘要。
func (i *DupIndex) IndexStats() string {
	return fmt.Sprintf("%d 条历史记录", len(i.Entries))
}
