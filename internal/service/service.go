// Package service 封装「解析站点 → A* 搜索 → 打分排序 → 结果视图」的完整流程，
// 供命令行（railplan）与图形界面（railplan-gui）共用。
package service

import (
	"fmt"
	"time"

	"railplanner/internal/config"
	"railplanner/internal/model"
	"railplanner/internal/score"
	"railplanner/internal/search"
	"railplanner/internal/store"
)

// Request 查询请求
type Request struct {
	From     string
	To       string
	Date     string
	StartMin int        // 最早出发时刻（当日分钟，0 = 全天）
	Seat     model.Seat // 席别；空或 auto = 每段自动取最低价席别
	Top      int        // 返回方案条数上限（0 = 全部，最大 MaxResultCount）
}

// Result 查询结果（JSON 友好，可直接喂给前端）
type Result struct {
	From          string      `json:"from"`
	To            string      `json:"to"`
	Date          string      `json:"date"`
	StartStations []string    `json:"startStations"`
	GoalStations  []string    `json:"goalStations"`
	HasDirect     bool        `json:"hasDirect"`
	DirectPrice   float64     `json:"directPrice"`
	DirectMinutes int         `json:"directMinutes"`
	DirectFast    bool        `json:"directFast"` // 基准是否来自高铁/动车直达
	DirectTrain   string      `json:"directTrain"` // 基准车次
	Routes        []RouteView `json:"routes"`
	TotalRoutes   int         `json:"totalRoutes"`
	ElapsedMs     int64       `json:"elapsedMs"`
	Stats         StatsView   `json:"stats"`
	Seat          string      `json:"seat"`
	Disclaimer    string      `json:"disclaimer"`
}

// RouteView 单条方案的展示视图
type RouteView struct {
	Transfers   int       `json:"transfers"`
	TotalPrice  float64   `json:"totalPrice"`
	TotalMin    int       `json:"totalMin"`
	Score       float64   `json:"score"`
	IsCheap     bool      `json:"isCheap"`
	SaveRatio   float64   `json:"saveRatio"`
	SavePercent float64   `json:"savePercent"`
	FirstDepart int       `json:"firstDepart"`
	LastArrive  int       `json:"lastArrive"`
	Legs        []LegView `json:"legs"`
}

// LegView 单段行程视图
type LegView struct {
	TrainNo   string          `json:"trainNo"`
	TrainType string          `json:"trainType"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	Depart    string          `json:"depart"`
	DepartMin int             `json:"departMin"`
	Arrive    string          `json:"arrive"`
	ArriveMin int             `json:"arriveMin"`
	Duration  int             `json:"duration"`
	Price     float64         `json:"price"`
	Seat      string          `json:"seat"`
	SeatName  string          `json:"seatName"`
	WaitMin   int             `json:"waitMin"`
	KM        int             `json:"km"`
	Fares     []SeatPriceView `json:"fares"`
}

// SeatPriceView 席别价格
type SeatPriceView struct {
	Seat  string  `json:"seat"`
	Label string  `json:"label"`
	Price float64 `json:"price"`
}

// StatsView 搜索与剪枝统计
type StatsView struct {
	Expanded     int `json:"expanded"`
	Generated    int `json:"generated"`
	Collected    int `json:"collected"`
	PruneDepth   int `json:"pruneDepth"`
	PruneTime    int `json:"pruneTime"`
	PruneDominated int `json:"pruneDominated"`
	PrunePrice   int `json:"prunePrice"`
}

// Search 执行一次完整查询。starts/goals 为空时返回错误。
func Search(st *store.Store, req Request) (*Result, error) {
	starts := st.ResolveStations(req.From)
	goals := st.ResolveStations(req.To)
	if len(starts) == 0 {
		return nil, fmt.Errorf("未找到出发地：%s", req.From)
	}
	if len(goals) == 0 {
		return nil, fmt.Errorf("未找到目的地：%s", req.To)
	}

	opt := search.DefaultOptions()
	opt.StartMin = req.StartMin
	opt.Seat = req.Seat
	if opt.Seat == "" {
		opt.Seat = model.SeatAuto
	}

	t0 := time.Now()
	sw := search.New(st, starts, goals, opt)
	raw := sw.Search()
	direct := sw.Direct()
	ranked := score.Rank(raw, direct)
	elapsed := time.Since(t0)

	n := len(ranked)
	if req.Top > 0 && req.Top < n {
		n = req.Top
	}

	res := &Result{
		From:          req.From,
		To:            req.To,
		Date:          req.Date,
		StartStations: starts,
		GoalStations:  goals,
		HasDirect:     direct.OK,
		DirectPrice:   direct.Price,
		DirectMinutes: direct.Minutes,
		DirectFast:    direct.Fast,
		DirectTrain:   direct.TrainNo,
		TotalRoutes:   len(ranked),
		ElapsedMs:     elapsed.Milliseconds(),
		Seat:          string(opt.Seat),
		Disclaimer:    config.Disclaimer,
		Routes:        make([]RouteView, 0, n),
	}
	s := sw.Stats()
	res.Stats = StatsView{
		Expanded: s.Expanded, Generated: s.Generated, Collected: s.Collected,
		PruneDepth: s.Prune.Depth, PruneTime: s.Prune.Time,
		PruneDominated: s.Prune.Dominated, PrunePrice: s.Prune.Price,
	}
	for _, r := range ranked[:n] {
		res.Routes = append(res.Routes, toRouteView(r))
	}
	return res, nil
}

func toRouteView(r model.Route) RouteView {
	rv := RouteView{
		Transfers:   r.Transfers(),
		TotalPrice:  r.TotalPrice,
		TotalMin:    r.TotalMin,
		Score:       r.Score,
		IsCheap:     r.IsCheap,
		SaveRatio:   r.SaveRatio,
		SavePercent: r.SaveRatio * 100,
	}
	if len(r.Legs) > 0 {
		rv.FirstDepart = r.Legs[0].DepartMin
		rv.LastArrive = r.Legs[len(r.Legs)-1].ArriveMin
	}
	for _, l := range r.Legs {
		lv := LegView{
			TrainNo:   l.TrainNo,
			TrainType: l.TrainType,
			From:      l.From,
			To:        l.To,
			Depart:    model.MinToClock(l.DepartMin),
			DepartMin: l.DepartMin,
			Arrive:    model.MinToClock(l.ArriveMin),
			ArriveMin: l.ArriveMin,
			Duration:  l.ArriveMin - l.DepartMin,
			Price:     l.Price,
			Seat:      string(l.Seat),
			SeatName:  model.SeatName(l.Seat),
			WaitMin:   l.WaitMin,
			KM:        l.KM,
		}
		for _, seat := range model.SeatOrder {
			if v := l.Fares.Get(seat); v > 0 {
				lv.Fares = append(lv.Fares, SeatPriceView{
					Seat: string(seat), Label: model.SeatName(seat), Price: v,
				})
			}
		}
		rv.Legs = append(rv.Legs, lv)
	}
	return rv
}

// SeatOptions 返回全部可选席别（供前端下拉框）
func SeatOptions() []SeatOption {
	out := []SeatOption{{Value: string(model.SeatAuto), Label: model.SeatName(model.SeatAuto)}}
	for _, s := range model.SeatOrder {
		out = append(out, SeatOption{Value: string(s), Label: model.SeatName(s)})
	}
	return out
}

// SeatOption 席别选项
type SeatOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
