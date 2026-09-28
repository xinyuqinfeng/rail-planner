package service

// 旅行模式：出发地 → 全国目的地往返比价。
// 每个目的地调用完整中转比价引擎（直达与中转同场竞争），取综合最优方案。
// 深度搜索耗时较长（约 1 秒/目的地），以「后台任务 + 进度轮询」方式运行。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"railplanner/internal/store"
)

// TravelItem 一个目的地的往返方案
type TravelItem struct {
	City          string  `json:"city"`
	DestStation   string  `json:"destStation"`
	Km            int     `json:"km"`
	LegsText      string  `json:"legsText"`      // 方案车次链（含中转次数标注）
	Transfers     int     `json:"transfers"`     // 中转次数（0=直达）
	OneWayPrice   float64 `json:"oneWayPrice"`   // 单程最优方案总价
	OneWayMinutes int     `json:"oneWayMinutes"` // 单程最优方案总耗时
	Total         float64 `json:"total"`         // 往返总价（上下行同价 ×2）
	SavePercent   float64 `json:"savePercent"`   // 该方案较直达基准省的百分比
	DirectPrice   float64 `json:"directPrice"`   // 直达基准最低价（无直达为 0）
	Score         float64 `json:"score"`         // 性价比分：均速 ÷ 往返每公里单价
	UnitPrice     float64 `json:"unitPrice"`
	AvgSpeed      float64 `json:"avgSpeed"`
}

// TravelProgress 旅行模式任务的实时进度
type TravelProgress struct {
	Running   bool         `json:"running"`
	From      string       `json:"from"`
	Done      int          `json:"done"`
	Total     int          `json:"total"`
	Finished  bool         `json:"finished"`
	DestCount int          `json:"destCount"`
	Items     []TravelItem `json:"items,omitempty"`
	Err       string       `json:"err,omitempty"`
}

type travelJob struct {
	st             *store.Store
	from           string
	days           int
	limit          int
	hardSleeperOnly bool
	starts         []string
	pend           []store.TravelDestRow
	destN          int
}

var (
	travelMu      sync.Mutex
	travelRunning bool
	travelFrom    string
	travelDone    int
	travelTotal   int
	travelItems   []TravelItem
	travelDestN   int
	travelErr     string
	travelCur     *travelJob
)

var travelCache sync.Map // from -> travelCacheEntry

type travelCacheEntry struct {
	items     []TravelItem
	destCount int
}

// StartTravelJob 启动深度搜索后台任务。
// 返回 (是否新启动, 出发地, 错误)。同出发地正在运行则幂等返回；缓存命中直接置为完成态。
func StartTravelJob(st *store.Store, from string, days, limit int, hardSleeperOnly bool) (bool, string, error) {
	if days < 0 {
		days = 0 // 0 = 当天往返
	}
	travelMu.Lock()
	defer travelMu.Unlock()
	if travelRunning {
		return false, travelFrom, nil
	}
	key := strings.TrimSpace(from)
	if hardSleeperOnly {
		key += "|hs"
	}
	if v, ok := travelCache.Load(key); ok {
		e := v.(travelCacheEntry)
		travelRunning, travelFrom = false, key
		travelItems, travelDestN = e.items, e.destCount
		travelDone, travelTotal, travelErr = e.destCount, e.destCount, ""
		travelCur = nil
		return false, key, nil
	}

	starts := st.ResolveStations(from)
	if len(starts) == 0 {
		return false, "", fmt.Errorf("无法识别出发地：%s", from)
	}
	startSet := map[string]bool{}
	for _, s := range starts {
		startSet[s] = true
	}
	// 20000 是「目的站×车次类型×席别」的分组行数上限（非目的站数），给足余量避免截断
	dests := st.TravelDests(starts, 20000)
	if len(dests) == 0 {
		return false, "", fmt.Errorf("票价库中暂无从「%s」出发的真实票价，请先在页面底部抓取该方向数据", from)
	}
	pend := make([]store.TravelDestRow, 0, len(dests))
	for _, d := range dests {
		if !startSet[d.ToStation] {
			pend = append(pend, d)
		}
	}

	travelRunning = true
	travelFrom = key
	travelDone, travelTotal = 0, len(pend)
	travelItems, travelDestN, travelErr = nil, len(dests), ""
	travelCur = &travelJob{st: st, from: key, days: days, limit: limit, hardSleeperOnly: hardSleeperOnly, starts: starts, pend: pend, destN: len(dests)}

	go runTravel(travelCur)
	return true, key, nil
}

// ClearTravelCache 票价数据更新后清空旅行模式缓存，保证下次搜索用新票价。
// 注意：必须用 Range+Delete 逐项清除，不能给包级变量重新赋值——
// 后者在查询/抓取 goroutine 并发 Load/Store 时是数据竞争。
func ClearTravelCache() {
	travelCache.Range(func(k, _ any) bool {
		travelCache.Delete(k)
		return true
	})
}

// TravelProgress 查询当前任务进度；完成后携带结果
func TravelStatus(from string) TravelProgress {
	travelMu.Lock()
	defer travelMu.Unlock()
	p := TravelProgress{
		Running:   travelRunning,
		From:      travelFrom,
		Done:      travelDone,
		Total:     travelTotal,
		DestCount: travelDestN,
		Err:       travelErr,
	}
	if !travelRunning && travelFrom != "" && travelErr == "" && len(travelItems) > 0 {
		p.Finished = true
		p.Items = travelItems
	}
	_ = from // 当前为单任务模型
	return p
}

func runTravel(j *travelJob) {
	type res struct {
		dest        store.TravelDestRow
		routes      []RouteView
		directPrice float64
	}
	var mu sync.Mutex
	results := make([]res, 0, len(j.pend))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	deadline := time.Now().Add(90 * time.Second)

	for _, d := range j.pend {
		if time.Now().After(deadline) {
			break
		}
		wg.Add(1)
		go func(d store.TravelDestRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req := Request{From: j.starts[0], To: d.ToStation, Top: 5}
			r, err := Search(j.st, req)
			mu.Lock()
			travelDone++ // 每完成一个目的地即上报进度
			mu.Unlock()
			if err != nil || r == nil || len(r.Routes) == 0 {
				return
			}
			mu.Lock()
			results = append(results, res{dest: d, routes: r.Routes, directPrice: r.DirectPrice})
			mu.Unlock()
		}(d)
	}
	wg.Wait()

	items := make([]TravelItem, 0, len(results))
	for _, rr := range results {
		rv := &rr.routes[0]
		onePrice := rv.TotalPrice
		if j.hardSleeperOnly {
			// 绿皮口径：对**每一个**候选方案（含单方案）按「普速段≥5h硬卧/<5h硬座」重算，
			// 取重算后总价最低者。注意：硬卧价高于硬座价，绝不能拿它去跟最低价口径比较
			// （否则永远选不中硬卧方案）——绿皮口径下必须强制采用重算价。
			onePrice = greenRecalc(rv)
			for i := 1; i < len(rr.routes); i++ {
				if g := greenRecalc(&rr.routes[i]); g < onePrice {
					onePrice = g
					rv = &rr.routes[i]
				}
			}
		}
		km := rr.dest.Km
		if km <= 0 {
			for _, l := range rv.Legs {
				km += l.KM
			}
		}
		it := TravelItem{
			City:          rr.dest.City,
			DestStation:   rr.dest.ToStation,
			Km:            km,
			LegsText:      legsText(rv),
			Transfers:     rv.Transfers,
			OneWayPrice:   onePrice,
			OneWayMinutes: rv.TotalMin,
			Total:         onePrice * 2, // 上下行同价，往返 ×2
			SavePercent:   rv.SavePercent,
			DirectPrice:   rr.directPrice,
		}
		if j.hardSleeperOnly && rr.directPrice > 0 {
			// 绿皮口径下「较直达省」须按重算后价格重算，避免沿用最低价口径的省钱比例
			it.SavePercent = (rr.directPrice - onePrice) / rr.directPrice * 100
		}
		if it.Km > 0 && it.Total > 0 {
			it.UnitPrice = it.Total / float64(it.Km)
			mins := it.OneWayMinutes
			if mins < 1 {
				mins = 1
			}
			it.AvgSpeed = float64(it.Km) / float64(mins) * 60.0
			if it.UnitPrice > 0 {
				it.Score = it.AvgSpeed / it.UnitPrice
			}
		}
		items = append(items, it)
	}

	// 排序：性价比降序；同分（两位小数一致）按往返总价升序
	sort.SliceStable(items, func(i, j int) bool {
		si, sj := round2(items[i].Score), round2(items[j].Score)
		if si != sj {
			return si > sj
		}
		if items[i].Total != items[j].Total {
			return items[i].Total < items[j].Total
		}
		return items[i].Km < items[j].Km
	})

	travelMu.Lock()
	travelItems = items
	travelErr = ""
	travelRunning = false
	travelMu.Unlock()
	travelCache.Store(j.from, travelCacheEntry{items: items, destCount: j.destN})
}

func legsText(rv *RouteView) string {
	parts := make([]string, 0, len(rv.Legs))
	for _, l := range rv.Legs {
		parts = append(parts, l.TrainNo+"("+l.From+"→"+l.To+")")
	}
	t := strings.Join(parts, " + ")
	if rv.Transfers > 0 {
		t += fmt.Sprintf("（%d 次中转）", rv.Transfers)
	} else {
		t += "（直达）"
	}
	return t
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

// greenRecalc 按「绿皮口径」重算方案总价：
// 普速（绿皮）段乘车 ≥5 小时用硬卧价（睡卧铺），<5 小时用硬座价；
// 高铁/动车/城际段维持原价（二等座口径）。该段无硬卧票时退回硬座价。
func greenRecalc(rv *RouteView) float64 {
	total := 0.0
	for _, l := range rv.Legs {
		if l.TrainType == "普速" && l.Duration >= 300 {
			p := 0.0
			for _, f := range l.Fares {
				if strings.HasPrefix(f.Seat, "hard_sleep") && (p == 0 || f.Price < p) {
					p = f.Price
				}
			}
			if p > 0 {
				total += p
				continue
			}
		}
		total += l.Price
	}
	return total
}
