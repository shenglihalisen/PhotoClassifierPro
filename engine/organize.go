package engine

// organize.go 把废片按缺陷类型归档。
//
// 这是整个工具里**唯一会改动用户文件**的动作，所以规矩定得死一点：
//   - 只移动/复制，永不删除。删东西是用户的事。
//   - 默认干跑（Plan），只返回「打算怎么做」，真正执行要显式 Execute。
//   - 目标目录固定在源目录下的 `_待整理/`，不跨盘、不改父级。
//   - 同名文件不覆盖，自动加序号。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// OrganizeMode 归档方式。
type OrganizeMode string

const (
	// ModeMove 移动（省空间，源目录被清理）。
	ModeMove OrganizeMode = "move"
	// ModeCopy 复制（保守，源文件不动）。
	ModeCopy OrganizeMode = "copy"
)

// OrganizeOptions 归档参数。
type OrganizeOptions struct {
	// Root 扫描根目录（目标目录建在这下面）。
	Root string
	// Mode 移动还是复制。
	Mode OrganizeMode
	// DestDir 目标目录名，默认 "_待整理"。
	DestDir string
	// KeepDuplicates 是否把重复项也归档（默认只归档质量问题，重复单独处理）。
	KeepDuplicates bool
}

// Action 一条计划中的归档动作。
type Action struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Label 触发归档的缺陷名（损坏/模糊/...）。
	Label string `json:"label"`
	// Skip 非空表示这条被跳过及原因。
	Skip string `json:"skip,omitempty"`
}

// Plan 计算归档计划（不碰文件）。
func Plan(items []*Item, opt OrganizeOptions) []Action {
	if opt.DestDir == "" {
		opt.DestDir = "_待整理"
	}
	if opt.Mode == "" {
		opt.Mode = ModeMove
	}
	var out []Action
	for _, it := range items {
		if it == nil || it.Result == nil {
			continue
		}
		labels := it.Result.Labels()
		if len(labels) == 0 {
			continue
		}
		// 只由「重复」触发的项默认不动：重复不是坏片，删哪张该由人决定。
		if !opt.KeepDuplicates && len(labels) == 1 && labels[0] == "重复" {
			continue
		}
		label := labels[0]
		dest := filepath.Join(opt.Root, opt.DestDir, label, filepath.Base(it.Path))
		dest = uniquePath(dest)
		out = append(out, Action{From: it.Path, To: dest, Label: label})
	}
	return out
}

// Execute 执行归档计划。返回实际完成的动作数。
//
// 任何单条失败都不会中断整体，失败原因写回 Action.Skip。
func Execute(actions []Action, mode OrganizeMode) (done int) {
	for i := range actions {
		a := &actions[i]
		if a.Skip != "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(a.To), 0o755); err != nil {
			a.Skip = "建目录失败: " + err.Error()
			continue
		}
		var err error
		switch mode {
		case ModeCopy:
			err = copyFile(a.From, a.To)
		default:
			err = os.Rename(a.From, a.To)
			if err != nil {
				// 跨盘时 Rename 会失败，退回复制 + 删除源
				if cerr := copyFile(a.From, a.To); cerr != nil {
					a.Skip = "移动失败: " + cerr.Error()
					continue
				}
				if rerr := os.Remove(a.From); rerr != nil {
					a.Skip = "已复制但源文件删除失败: " + rerr.Error()
					continue
				}
			}
		}
		if err != nil {
			a.Skip = err.Error()
			continue
		}
		done++
	}
	return done
}

// uniquePath 目标已存在时加 _1 / _2 后缀，绝不覆盖。
func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return p
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
