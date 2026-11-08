package engine

import "runtime"

// defaultWorkers 并发数：按 CPU 核数，但不超过 8。
//
// 检测是纯 CPU 计算，开太多协程只会在解码峰值时抢内存
// （一张 4000×3000 的图解码要 36MB，8 路并发就是 300MB）。
func defaultWorkers() int {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}
