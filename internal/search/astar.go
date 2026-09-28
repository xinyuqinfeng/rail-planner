// Package search 实现 A* 启发式搜索：状态 = (当前车站, 到达时间, 累计票价, 已中转次数, 路径)
package search

import (
	"container/heap"
	"fmt"
	"strings"

	"railplanner/internal/config"
	"railplanner/internal/model"
	"railplanner/internal/store"
)

// Options 搜索参数
type Options struct {
	StartMin     int        // 出行时刻（当日分钟，0 = 不限时刻，全天可乘）
	Seat         model.Seat // 席别（model.SeatAuto = 每段自动取最低价席别）
	MaxExpand    int        // 最大扩展节点数（防卡死）
	MaxRawRoutes int        // 收集方案上限（排序前）
}

// DefaultOptions 默认搜索参数
func DefaultOptions() Options {
	return Options{StartMin: 0, Seat: model.SeatAuto, MaxExpand: 300000, MaxRawRoutes: 2000}
}

// Stats 搜索统计
type Stats struct {
	Expanded  int
	Generated int
	Collected int
	Prune     PruneResult
}

// node 搜索状态
type node struct {
	station   string
	arriveAbs int // 绝对分钟（相对出行日 00:00，可 >1440 表示跨天）
	price     float64
	transfers int
	legs      []model.Leg
	via       []string
	visited   []string
	priority  float64
	index     int
}

// Searcher A* 搜索器
type Searcher struct {
	st       *store.Store
	starts   []string
	goals    map[string]bool
	opt      Options
	direct   store.DirectInfo
	hCache   map[string]float64
	tCache   map[string]int
	stats    Stats
	routes   []model.Route
	seenKeys map[string]bool
}

// New 创建搜索器
func New(st *store.Store, starts, goals []string, opt Options) *Searcher {
	goalSet := make(map[string]bool, len(goals))
	for _, g := range goals {
		goalSet[g] = true
	}
	if opt.MaxExpand <= 0 {
		opt.MaxExpand = DefaultOptions().MaxExpand
	}
	if opt.MaxRawRoutes <= 0 {
		opt.MaxRawRoutes = DefaultOptions().MaxRawRoutes
	}
	s := &Searcher{
		st:       st,
		starts:   starts,
		goals:    goalSet,
		opt:      opt,
		hCache:   make(map[string]float64),
		tCache:   make(map[string]int),
		seenKeys: make(map[string]bool),
	}
	s.direct = st.DirectBest(starts, goals, opt.Seat)
	return s
}

// Direct 返回直达基准信息（供打分使用）
func (s *Searcher) Direct() store.DirectInfo { return s.direct }

// Stats 返回搜索统计
func (s *Searcher) Stats() Stats { return s.stats }

// heuristic 启发函数：当前站直达终点的最低票价，作为剩余费用下界（无直达则为 0）
func (s *Searcher) heuristic(station string) float64 {
	if v, ok := s.hCache[station]; ok {
		return v
	}
	v := s.st.MinDirectPriceTo(station, s.goals, s.opt.Seat)
	s.hCache[station] = v
	return v
}

// restTimeLowerBound 当前站直达终点的最短耗时，作为剩余时间下界（无直达为 0）
func (s *Searcher) restTimeLowerBound(station string) int {
	if v, ok := s.tCache[station]; ok {
		return v
	}
	segs, err := s.st.SegmentsFrom(station)
	best := 0
	if err == nil {
		for _, seg := range segs {
			if !s.goals[seg.To] {
				continue
			}
			if d := seg.Duration(); best == 0 || d < best {
				best = d
			}
		}
	}
	s.tCache[station] = best
	return best
}

// Search 执行 A* 搜索，返回原始方案列表（未排序、未截断）
func (s *Searcher) Search() []model.Route {
	pq := &nodeHeap{}
	heap.Init(pq)
	pareto := NewParetoTable()

	for _, st := range s.starts {
		if s.goals[st] {
			continue // 出发地即目的地
		}
		n := &node{station: st, arriveAbs: s.opt.StartMin, visited: []string{st}}
		n.priority = s.heuristic(st)
		heap.Push(pq, n)
	}

	for pq.Len() > 0 {
		if s.stats.Expanded >= s.opt.MaxExpand || len(s.routes) >= s.opt.MaxRawRoutes {
			break
		}
		cur := heap.Pop(pq).(*node)
		s.stats.Expanded++

		// ① 深度剪枝：已 3 次中转，不再拓展（避免出现 4 次中转）
		if PruneDepth(cur.transfers) {
			s.stats.Prune.Depth++
			continue
		}

		// ③ 价格剪枝：已出发且累计票价已达直达最低价 → 继续只会更贵
		if PrunePrice(cur.price, s.direct.Price, s.direct.OK, len(cur.legs) > 0) {
			s.stats.Prune.Price++
			continue
		}

		segs, err := s.st.SegmentsFrom(cur.station)
		if err != nil {
			continue
		}

		for _, seg := range segs {
			departAbs := nextClock(cur.arriveAbs, seg.DepartHM)
			wait := departAbs - cur.arriveAbs

			if len(cur.legs) > 0 {
				// 基础过滤：换乘间隔 < MIN_TRANSFER_GAP 视为赶不上车
				if wait < config.MinTransferGap {
					continue
				}
				if wait > config.MaxBoardingWindowMin {
					continue // 过晚的班次，无意义扩展
				}
			} else if departAbs < s.opt.StartMin {
				continue
			}

			// 环检测：不走回头路
			if contains(cur.visited, seg.To) && !s.goals[seg.To] {
				continue
			}

			// 席别选择：auto = 本段可用席别中的最低价；指定席别时该段必须售此席别
			segPrice := seg.Price
			usedSeat := seg.Fares.MinSeat()
			if s.opt.Seat != "" && s.opt.Seat != model.SeatAuto {
				v := seg.Fares.Get(s.opt.Seat)
				if v <= 0 {
					continue // 本段不售该席别，跳过
				}
				segPrice, usedSeat = v, s.opt.Seat
			}
			if segPrice <= 0 {
				continue // 无有效票价（数据缺失）
			}
			legWait := 0
			if len(cur.legs) > 0 {
				legWait = wait
			}

			arriveAbs := departAbs + seg.Duration()
			newPrice := cur.price + segPrice
			newTransfers := cur.transfers
			if len(cur.legs) > 0 {
				newTransfers = cur.transfers + 1
			}
			// 全程耗时口径 = 首段发车 → 末段到达（不含出发前的候车时间）
			firstDepart := departAbs
			if len(cur.legs) > 0 {
				firstDepart = cur.legs[0].DepartMin
			}
			elapsed := arriveAbs - firstDepart

			// ② 时间硬剪枝：仅作用于「中转方案」。
			// 直达方案（首段即到终点）必须全部保留——直达基准往往来自最便宜的慢车，
			// 若把它剪掉就会出现「基准显示 ¥48、结果里最便宜却是 ¥149」的矛盾。
			if len(cur.legs) > 0 {
				rest := 0
				if !s.goals[seg.To] { // 已到终点时剩余为 0
					rest = s.restTimeLowerBound(seg.To)
				}
				if PruneTime(elapsed, rest, s.direct.Minutes, s.direct.OK) {
					s.stats.Prune.Time++
					continue
				}
				// 无直达基准（或所选席别无直达）时的兜底耗时上限，防止搜索空间失控
				if !s.direct.OK && elapsed > config.MaxDurationNoDirect {
					s.stats.Prune.Time++
					continue
				}
			}

			// ④ 支配剪枝：同 (车站, 中转次数) 已有「到达更早、票价更低、总耗时更短」的状态
			if pareto.Dominated(ParetoKey{Station: seg.To, Transfers: newTransfers},
				firstDepart, arriveAbs, newPrice) {
				s.stats.Prune.Dominated++
				continue
			}

			s.stats.Generated++

			// 构造新路径（深拷贝 slice，避免别名污染）
			newLegs := make([]model.Leg, len(cur.legs)+1)
			copy(newLegs, cur.legs)
			newLegs[len(cur.legs)] = model.Leg{
				TrainNo:   seg.TrainNo,
				TrainType: seg.TrainType,
				From:      seg.From,
				To:        seg.To,
				DepartMin: departAbs,
				ArriveMin: arriveAbs,
				Price:     segPrice,
				Seat:      usedSeat,
				Fares:     seg.Fares,
				WaitMin:   legWait,
				KM:        seg.KM,
			}
			var newVia []string
			if len(cur.legs) > 0 {
				newVia = append(append([]string{}, cur.via...), cur.station)
			}
			newVisited := append(append([]string{}, cur.visited...), seg.To)

			// 到达终点：记录方案（不再向后扩展）
			if s.goals[seg.To] {
				route := model.Route{
					Legs:       newLegs,
					TransferAt: newVia,
					TotalPrice: newPrice,
					TotalMin:   arriveAbs - newLegs[0].DepartMin,
					Seat:       s.opt.Seat,
				}
				if key := routeKey(route); !s.seenKeys[key] {
					s.seenKeys[key] = true
					s.routes = append(s.routes, route)
				}
				continue
			}

			child := &node{
				station:   seg.To,
				arriveAbs: arriveAbs,
				price:     newPrice,
				transfers: newTransfers,
				legs:      newLegs,
				via:       newVia,
				visited:   newVisited,
			}
			child.priority = newPrice + s.heuristic(seg.To)
			heap.Push(pq, child)
		}
	}

	s.stats.Collected = len(s.routes)
	return s.routes
}

// nodeHeap 按 f = 累计票价 + 启发下界 的小顶堆
type nodeHeap []*node

func (h nodeHeap) Len() int { return len(h) }

func (h nodeHeap) Less(i, j int) bool {
	if h[i].priority != h[j].priority {
		return h[i].priority < h[j].priority
	}
	return h[i].arriveAbs < h[j].arriveAbs
}

func (h nodeHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *nodeHeap) Push(x any) {
	n := x.(*node)
	n.index = len(*h)
	*h = append(*h, n)
}

func (h *nodeHeap) Pop() any {
	old := *h
	n := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return n
}

// routeKey 方案去重键（车次 + 发车时刻序列）
func routeKey(r model.Route) string {
	var b strings.Builder
	for i, l := range r.Legs {
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%s@%d", l.TrainNo, l.DepartMin)
	}
	return b.String()
}

// nextClock 求「不早于 abs 的、当日时刻为 hm 的最近绝对分钟」
func nextClock(abs, hm int) int {
	t := (abs/1440)*1440 + hm
	for t < abs {
		t += 1440
	}
	return t
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
