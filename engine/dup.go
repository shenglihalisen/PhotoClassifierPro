package engine

// dup.go 跨文件重复检测。
//
// 判据三层（和 medreview 的 QC v5 对齐）：
//  1. pHash 汉明距离 ≤ 5 —— 只是**候选**，不是结论；
//  2. 像素二次确认 —— 把两张图都缩到 64×64，显著不同的像素超过 5% 就判「不同」；
//  3. 连拍豁免 —— 时间接近、构图相似但主体不同（比如同一背景连拍十张，人物姿势各异）
//     在第 2 步就会被拦下来。
//
// 为什么必须做第 2 步：单靠 pHash 会把「同机位不同主体」全部判成重复。
// medreview 实测里，被误判的连拍对汉明距离只有 3~5，和真副本几乎重合；
// 但像素差异 34%~91%，两侧余量极大。

import (
	"image"
	"sort"
	"sync"

	"github.com/shenglihalisen/PhotoClassifierPro/qc"
)

// DupGroup 一组内容相同的照片。
type DupGroup struct {
	ID    int     `json:"id"`
	Hash  string  `json:"hash"`
	Items []*Item `json:"items"`
	// Bytes 这组里除第一张以外的冗余字节数（= 可以省下的空间）。
	Bytes int64 `json:"bytes"`
}

// dupPairBudget 单个 hash 桶内最多做多少次像素确认。
// 桶太大说明这一批图高度相似（比如整段连拍），逐个解码确认会非常慢；
// 超预算时保守地**不判重复**（宁漏勿滥）。
const dupPairBudget = 24

// DetectDuplicates 在已检测结果里找出内容重复的照片。
//
// 返回的每组第一张是「保留项」（体积最大/时间最早），其余是冗余副本。
func DetectDuplicates(items []*Item) []*DupGroup {
	// 只留有有效哈希的
	type cand struct {
		idx  int
		hash uint64
		it   *Item
	}
	var cs []cand
	for i, it := range items {
		if it == nil || it.Result == nil || it.Result.DupHash == "" {
			continue
		}
		h, ok := qc.ParseHash(it.Result.DupHash)
		if !ok {
			continue
		}
		cs = append(cs, cand{i, h, it})
	}
	if len(cs) < 2 {
		return nil
	}

	// LSH 分桶：64 位切成 4 段 16 位。汉明距离 ≤5 的两个哈希，
	// 按鸽笼原理至少有一段完全相同，所以只需在 4 个桶内找候选。
	buckets := make([]map[uint16][]int, 4)
	for i := range buckets {
		buckets[i] = map[uint16][]int{}
	}
	for i, c := range cs {
		for s := 0; s < 4; s++ {
			key := uint16((c.hash >> (16 * s)) & 0xFFFF)
			buckets[s][key] = append(buckets[s][key], i)
		}
	}

	// 并查集
	parent := make([]int, len(cs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}

	// 像素确认需要重新解码，做成缓存避免同一对图重复解
	type pair struct{ a, b int }
	checked := map[pair]bool{}

	for s := 0; s < 4; s++ {
		for _, bucket := range buckets[s] {
			if len(bucket) < 2 {
				continue
			}
			budget := dupPairBudget
			for i := 0; i < len(bucket) && budget > 0; i++ {
				for j := i + 1; j < len(bucket) && budget > 0; j++ {
					a, b := bucket[i], bucket[j]
					if find(a) == find(b) {
						continue
					}
					p := pair{a, b}
					if checked[p] {
						continue
					}
					checked[p] = true
					budget--

					if qc.Hamming(cs[a].hash, cs[b].hash) > qc.DupHammingThresh {
						continue
					}
					// 像素二次确认
					same, err := pixelSame(cs[a].it.Path, cs[b].it.Path)
					if err != nil || !same {
						continue // 确认不了就当不同（宁漏勿滥）
					}
					union(a, b)
				}
			}
		}
	}

	// 归组
	groups := map[int][]*Item{}
	for i := range cs {
		r := find(i)
		groups[r] = append(groups[r], cs[i].it)
	}

	var out []*DupGroup
	id := 0
	for _, its := range groups {
		if len(its) < 2 {
			continue
		}
		// 组内排序：体积大的在前（通常是有损压缩的原始那份）
		sort.Slice(its, func(i, j int) bool {
			if its[i].Size != its[j].Size {
				return its[i].Size > its[j].Size
			}
			return its[i].ModTime.Before(its[j].ModTime)
		})
		var redundant int64
		for _, it := range its[1:] {
			redundant += it.Size
		}
		id++
		out = append(out, &DupGroup{ID: id, Hash: its[0].Result.DupHash, Items: its, Bytes: redundant})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// pixelSame 像素二次确认：64×64 灰度上显著不同的像素占比 ≤ 5% 判相同。
func pixelSame(pathA, pathB string) (bool, error) {
	ga, err := smallGray(pathA)
	if err != nil {
		return false, err
	}
	gb, err := smallGray(pathB)
	if err != nil {
		return false, err
	}
	if len(ga) != len(gb) || len(ga) == 0 {
		return false, nil
	}
	const significant = 25 // 灰度差超过 25 算「显著不同」
	diff := 0
	for i := range ga {
		d := int(ga[i]) - int(gb[i])
		if d < 0 {
			d = -d
		}
		if d > significant {
			diff++
		}
	}
	return float64(diff)/float64(len(ga)) <= qc.DupPixelDiffThresh, nil
}

// smallGrayCache 64×64 灰度缓存，避免同一张图在多个桶里被反复解码。
var smallGrayCache sync.Map // map[string][]byte

func smallGray(path string) ([]byte, error) {
	if v, ok := smallGrayCache.Load(path); ok {
		return v.([]byte), nil
	}
	img, err := qc.DecodeFile(path)
	if err != nil {
		return nil, err
	}
	g := qc.GrayThumb(img, 64)
	smallGrayCache.Store(path, g)
	return g, nil
}

// ResetCache 扫描新目录前清掉缓存，避免长期占内存。
func ResetCache() { smallGrayCache = sync.Map{} }

var _ = image.Rect
