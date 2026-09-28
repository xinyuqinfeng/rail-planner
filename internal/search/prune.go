// Package search 实现 A* 启发式搜索与四层剪枝。
package search

import (
	"railplanner/internal/config"
)

// ParetoKey 支配剪枝的状态键：同一车站 + 同一中转次数
type ParetoKey struct {
	Station   string
	Transfers int
}

type paretoPoint struct {
	start  int // 首段发车绝对分钟（决定总耗时口径）
	arrive int
	price  float64
}

// ParetoTable 支配剪枝表：同一 (车站, 中转次数) 下只保留非支配状态。
// 支配判据为三维：到达更早、票价更低、且出发不更早（总耗时不会更长）。
// 第三维不可省——否则「凌晨出发、到得早但耗时极长」的状态会把
// 「早上出发、到得稍晚但耗时合理」的状态剪掉，导致可行换乘被误杀。
type ParetoTable struct {
	m map[ParetoKey][]paretoPoint
}

// NewParetoTable 创建支配剪枝表
func NewParetoTable() *ParetoTable {
	return &ParetoTable{m: make(map[ParetoKey][]paretoPoint)}
}

// Dominated 判断新状态是否被已有状态支配；未被支配则插入并淘汰被支配的旧状态。
// 返回 true 表示应剪枝。
func (p *ParetoTable) Dominated(key ParetoKey, start, arrive int, price float64) bool {
	pts := p.m[key]
	for _, q := range pts {
		if q.arrive <= arrive && q.price <= price && q.start >= start {
			return true // 已有「到达不晚 + 不更贵 + 总耗时更短」的状态
		}
	}
	// 淘汰被新状态支配的旧点
	kept := pts[:0]
	for _, q := range pts {
		if arrive <= q.arrive && price <= q.price && start >= q.start {
			continue
		}
		kept = append(kept, q)
	}
	p.m[key] = append(kept, paretoPoint{start: start, arrive: arrive, price: price})
	return false
}

// PruneResult 剪枝统计
type PruneResult struct {
	Depth     int
	Time      int
	Dominated int
	Price     int
}

// ── 四层剪枝 ───────────────────────────────────────────────

// PruneDepth ① 深度剪枝：中转次数 ≥ MAX_TRANSFER_COUNT(3) 直接终止该分支（即最多 4 段行程）
func PruneDepth(transfers int) bool {
	return transfers >= config.MaxTransferCount
}

// PruneTime ② 时间硬剪枝：存在直达方案时，累计耗时（或「累计耗时 + 剩余最短耗时」下界）
// 超过「直达最短耗时 × MAX_TIME_MULTIPLE」则剪枝。
func PruneTime(elapsedMin, restLowerBoundMin, directMin int, hasDirect bool) bool {
	if !hasDirect || directMin <= 0 {
		return false
	}
	limit := float64(directMin) * config.MaxTimeMultiple
	return float64(elapsedMin+restLowerBoundMin) > limit
}

// PrunePrice ③ 价格剪枝：已在中转途中，且累计票价已达/超过直达最低票价（票价单调递增，
// 继续中转只会更贵）→ 剪枝。起点未出发（无行程）与无直达场景不适用。
func PrunePrice(price, directPrice float64, hasDirect, departed bool) bool {
	if !hasDirect || !departed {
		return false
	}
	return price >= directPrice
}
