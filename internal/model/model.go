// Package model 定义核心数据结构。
package model

import "fmt"

// Seat 席别标识
type Seat string

// 支持的席别（auto = 每段自动取最低价席别）
const (
	SeatAuto         Seat = "auto"
	SeatHardSeat     Seat = "hard_seat"      // 硬座
	SeatHardSleepUp  Seat = "hard_sleep_up"  // 硬卧上铺
	SeatHardSleepMid Seat = "hard_sleep_mid" // 硬卧中铺
	SeatHardSleepLow Seat = "hard_sleep_low" // 硬卧下铺
	SeatSoftSleepUp  Seat = "soft_sleep_up"  // 软卧上铺
	SeatSoftSleepLow Seat = "soft_sleep_low" // 软卧下铺
	SeatSoftSeat     Seat = "soft_seat"      // 软座
	SeatNoSeat       Seat = "no_seat"        // 无座
	SeatSecond       Seat = "second"         // 二等座
	SeatFirst        Seat = "first"          // 一等座
	SeatBusiness     Seat = "business"       // 商务座
)

// SeatNames 席别显示名
var SeatNames = map[Seat]string{
	SeatAuto:         "自动（选最低价席别）",
	SeatHardSeat:     "硬座",
	SeatHardSleepUp:  "硬卧上铺",
	SeatHardSleepMid: "硬卧中铺",
	SeatHardSleepLow: "硬卧下铺",
	SeatSoftSleepUp:  "软卧上铺",
	SeatSoftSleepLow: "软卧下铺",
	SeatSoftSeat:     "软座",
	SeatNoSeat:       "无座",
	SeatSecond:       "二等座",
	SeatFirst:        "一等座",
	SeatBusiness:     "商务座",
}

// SeatOrder 席别展示顺序（从经济到舒适）
var SeatOrder = []Seat{
	SeatHardSeat, SeatNoSeat, SeatHardSleepUp, SeatHardSleepMid, SeatHardSleepLow,
	SeatSoftSeat, SeatSoftSleepUp, SeatSoftSleepLow, SeatSecond, SeatFirst, SeatBusiness,
}

// SeatName 取席别显示名
func SeatName(s Seat) string {
	if n, ok := SeatNames[s]; ok {
		return n
	}
	return string(s)
}

// FareSet 一组席别票价（金额语义由使用方决定：可为「自始发站累计价」或「区间段价」）
type FareSet struct {
	HardSeat     float64
	HardSleepUp  float64
	HardSleepMid float64
	HardSleepLow float64
	SoftSleepUp  float64
	SoftSleepLow float64
	SoftSeat     float64
	NoSeat       float64
	Second       float64
	First        float64
	Business     float64
}

// Get 取指定席别价格（0 表示该车次无此席别）
func (f FareSet) Get(s Seat) float64 {
	switch s {
	case SeatHardSeat:
		return f.HardSeat
	case SeatHardSleepUp:
		return f.HardSleepUp
	case SeatHardSleepMid:
		return f.HardSleepMid
	case SeatHardSleepLow:
		return f.HardSleepLow
	case SeatSoftSleepUp:
		return f.SoftSleepUp
	case SeatSoftSleepLow:
		return f.SoftSleepLow
	case SeatSoftSeat:
		return f.SoftSeat
	case SeatNoSeat:
		return f.NoSeat
	case SeatSecond:
		return f.Second
	case SeatFirst:
		return f.First
	case SeatBusiness:
		return f.Business
	}
	return 0
}

// Min 最低价（仅统计有效席别）
func (f FareSet) Min() float64 {
	min := 0.0
	for _, s := range SeatOrder {
		v := f.Get(s)
		if v <= 0 {
			continue
		}
		if min == 0 || v < min {
			min = v
		}
	}
	return min
}

// MinSeat 最低价对应的席别
func (f FareSet) MinSeat() Seat {
	var best Seat
	min := 0.0
	for _, s := range SeatOrder {
		v := f.Get(s)
		if v <= 0 {
			continue
		}
		if min == 0 || v < min {
			min, best = v, s
		}
	}
	return best
}

// Available 该席别是否可用（价格 > 0）
func (f FareSet) Available(s Seat) bool { return f.Get(s) > 0 }

// Sub 求差集（用于累计价 → 段价）
func (f FareSet) Sub(o FareSet) FareSet {
	return FareSet{
		HardSeat:     diffOrZero(f.HardSeat, o.HardSeat),
		HardSleepUp:  diffOrZero(f.HardSleepUp, o.HardSleepUp),
		HardSleepMid: diffOrZero(f.HardSleepMid, o.HardSleepMid),
		HardSleepLow: diffOrZero(f.HardSleepLow, o.HardSleepLow),
		SoftSleepUp:  diffOrZero(f.SoftSleepUp, o.SoftSleepUp),
		SoftSleepLow: diffOrZero(f.SoftSleepLow, o.SoftSleepLow),
		SoftSeat:     diffOrZero(f.SoftSeat, o.SoftSeat),
		NoSeat:       diffOrZero(f.NoSeat, o.NoSeat),
		Second:       diffOrZero(f.Second, o.Second),
		First:        diffOrZero(f.First, o.First),
		Business:     diffOrZero(f.Business, o.Business),
	}
}

func diffOrZero(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	d := a - b
	if d < 0 {
		return 0
	}
	return d
}

// Station 车站
type Station struct {
	Name          string
	TelegraphCode string
	City          string
}

// Train 车次
type Train struct {
	TrainNo      string
	TrainType    string // 高铁 / 城际 / 动车 / 普速 / 普客
	StartStation string
	EndStation   string
}

// Leg 一段行程（同车次的一段直达）
type Leg struct {
	TrainNo   string
	TrainType string
	From      string
	To        string
	DepartMin int     // 绝对分钟数（可 >1440，表示次日）
	ArriveMin int     // 绝对分钟数
	Price     float64 // 本段实际采用席别的票价
	Seat      Seat    // 实际采用的席别
	Fares     FareSet // 本段可用席别票价（段价）
	WaitMin   int     // 上一段到达后到本段发车的等待（首段为 0）
	KM        int     // 本段里程
}

// Route 一条完整方案
type Route struct {
	Legs       []Leg
	TransferAt []string // 各换乘站名
	TotalPrice float64
	TotalMin   int // 全程总耗时（分钟）
	Score      float64
	IsCheap    bool // 高性价比低价中转标记
	SaveRatio  float64
	Seat       Seat // 采用的席别策略
}

// Transfers 中转次数
func (r *Route) Transfers() int { return len(r.Legs) - 1 }

// DurationText 格式化总耗时
func (r *Route) DurationText() string {
	h, m := r.TotalMin/60, r.TotalMin%60
	return fmt.Sprintf("%d小时%02d分", h, m)
}

// MinToClock 绝对分钟 → HH:MM，超过一天的加 (+1天) 标记
func MinToClock(abs int) string {
	day := 0
	for abs >= 1440 {
		abs -= 1440
		day++
	}
	s := fmt.Sprintf("%02d:%02d", abs/60, abs%60)
	if day > 0 {
		s += fmt.Sprintf(" (+%d天)", day)
	}
	return s
}
