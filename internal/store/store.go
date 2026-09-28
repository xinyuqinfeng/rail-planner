// Package store 负责 SQLite 数据访问：车站解析、出发段查询（联合索引 + 站点级缓存）。
package store

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"railplanner/internal/config"
	"railplanner/internal/model"
)

// Segment 一段同一车次内的乘车区间（可直接购票的一段）
type Segment struct {
	TrainNo       string
	TrainType     string
	From          string
	To            string
	DepartHM      int     // 该站该车次的发车时刻（当日分钟 0-1439）
	ArriveHM      int     // 到达下车站的时刻（当日分钟 0-1439）
	DepartElapsed int     // 自车次始发站累计运行分钟（发车）
	ArriveElapsed int     // 自车次始发站累计运行分钟（到达）
	KM            int     // 本段里程
	Price         float64 // 默认价（本段可用席别中的最低价）
	Fares         model.FareSet // 本段各席别票价（0 = 该段无此席别）
}

// Duration 本段耗时（分钟）
func (s Segment) Duration() int { return s.ArriveElapsed - s.DepartElapsed }

// seatColumns 席别在 stop_schedule 中的列后缀与 FareSet 字段对应关系
var seatColumns = []struct{ Col, Field string }{
	{"hard_seat", "HardSeat"},
	{"hard_sleep_up", "HardSleepUp"},
	{"hard_sleep_mid", "HardSleepMid"},
	{"hard_sleep_low", "HardSleepLow"},
	{"soft_sleep_up", "SoftSleepUp"},
	{"soft_sleep_low", "SoftSleepLow"},
	{"soft_seat", "SoftSeat"},
	{"no_seat", "NoSeat"},
	{"second", "Second"},
	{"first", "First"},
	{"business", "Business"},
}

// seatDiffExpr 生成「段价 = 下车站累计价 - 上车站累计价」的 SQL 片段；
// 仅当上下车站都售该席别时才有效，否则记 0。
func seatDiffExpr(col string) string {
	p := "p_" + col
	return fmt.Sprintf(
		"CASE WHEN b.%s > 0 AND s.%s >= b.%s THEN s.%s - b.%s ELSE 0 END",
		p, p, p, p, p)
}

// DirectInfo 两站间直达的最优信息
type DirectInfo struct {
	Price   float64
	Minutes int
	OK      bool
	Fast    bool   // 基准是否来自高铁/城际/动车
	TrainNo string // 基准车次（展示用）
}

// isFastTrain 高铁 / 城际 / 动车 视为快速车次
func isFastTrain(trainType string) bool {
	switch trainType {
	case "高铁", "城际", "动车":
		return true
	}
	return false
}

// Store 数据访问对象
type Store struct {
	db *sql.DB

	mu       sync.Mutex
	stations map[string]model.Station
	cities   map[string][]string
	segCache map[string][]Segment
	odFares  map[string]float64      // key = 出发站|到达站|车次类型|席别 → 真实票价
	odList   [][2]string             // 全路网有直达车次的站对（按车次数降序，懒加载）
	telecodes map[string]string      // 站名 → 电报码（懒加载）
}

// Open 打开数据库
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(4)
	s := &Store{
		db:       db,
		stations: make(map[string]model.Station),
		cities:   make(map[string][]string),
		segCache: make(map[string][]Segment),
		odFares:  make(map[string]float64),
	}
	if err := s.loadStations(); err != nil {
		return nil, err
	}
	_ = s.loadODFares() // 真实 OD 票价（优先数据源）
	return s, nil
}

// loadODFares 载入真实 OD 票价表
func (s *Store) loadODFares() error {
	rows, err := s.db.Query(`SELECT from_station, to_station, train_type, seat, price FROM od_fare`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var from, to, tt, seat string
		var price float64
		if err := rows.Scan(&from, &to, &tt, &seat, &price); err != nil {
			return err
		}
		s.odFares[from+"|"+to+"|"+tt+"|"+seat] = price
	}
	return rows.Err()
}

// pickFare 计算段票价，按可信度依次取：
//   ① 真实 OD 票价表 —— 该 OD 被某趟车从始发站直达覆盖，是 12306 实际可售价格，最准
//   ② 累计价差值 —— 来自同一趟车的真实累计票价之差。短途段与独立计价基本一致
//      （汉中→广元 138km 差值 ¥66 = 真实值）；只有跨线长交路的中途段会因递远
//      递减略偏低。曾试过"全局里程曲线"与"按车次曲线"两种推算，实测均不如差值法
//      （曲线会把不同线路的票价率混在一起：138km 段被算成 ¥96，高估 45%），故不再使用。
// rawDiff 同时用于判断该段是否售该席别（<=0 表示不售）。
func (s *Store) pickFare(from, to, trainType string, seat model.Seat, km int, rawDiff float64) float64 {
	if rawDiff <= 0 {
		return 0
	}
	if v, ok := s.odFares[from+"|"+to+"|"+trainType+"|"+string(seat)]; ok && v > 0 {
		return math.Round(v*10) / 10
	}
	return math.Round(rawDiff*10) / 10
}

// Close 关闭数据库
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) loadStations() error {
	rows, err := s.db.Query(`SELECT station_name, COALESCE(telegraph_code,''), city_name FROM station`)
	if err != nil {
		return fmt.Errorf("读取车站表失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st model.Station
		if err := rows.Scan(&st.Name, &st.TelegraphCode, &st.City); err != nil {
			return err
		}
		s.stations[st.Name] = st
		s.cities[st.City] = append(s.cities[st.City], st.Name)
	}
	return rows.Err()
}

// AllStationNames 返回全部站名（供提示/调试）
func (s *Store) AllStationNames() []string {
	out := make([]string, 0, len(s.stations))
	for n := range s.stations {
		out = append(out, n)
	}
	return out
}

// ResolveStations 把「城市名 / 车站名」解析为车站列表。
// 优先级：城市名（展开该城市全部车站）> 精确站名 > 站名前缀匹配。
//
// 城市优先于站名：像「西安」「北京」这类既是城市名、又是主站名（普速站）的输入，
// 用户意图通常是整个城市——高铁多在西安北、北京西等站，若按单站解析会漏掉大量车次
// （曾导致「西安→成都」只搜到 15 小时的普速车，看不到西成高铁）。
// 需要精确到某站时，输入完整站名（如「西安北」）即可。
func (s *Store) ResolveStations(input string) []string {
	q := strings.TrimSpace(input)
	if q == "" {
		return nil
	}
	if list, ok := s.cities[q]; ok && len(list) > 0 { // 1. 城市 → 多车站
		out := append([]string(nil), list...)
		sortStations(out)
		return out
	}
	if _, ok := s.stations[q]; ok { // 2. 精确站名
		return []string{q}
	}
	// 3. 城市名带后缀（如「重庆市」）或站名前缀
	matches := map[string]bool{}
	for name, st := range s.stations {
		if strings.HasPrefix(name, q) {
			matches[name] = true
			continue
		}
		if st.City != "" && (strings.HasPrefix(st.City, q) || strings.Contains(st.City, q)) {
			matches[name] = true
		}
	}
	out := make([]string, 0, len(matches))
	for n := range matches {
		out = append(out, n)
	}
	sortStations(out)
	return out
}

func sortStations(list []string) {
	// 主站（无方位后缀）优先，再按名称排序：重庆 > 重庆北 > 重庆西
	rank := func(n string) int {
		if len(n) <= 3 {
			return 0
		}
		return 1
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if rank(a) < rank(b) || (rank(a) == rank(b) && a <= b) {
				break
			}
			list[j-1], list[j] = b, a
		}
	}
}

// SegmentsFrom 返回某站出发的全部乘车段（首次访问查库，之后走内存缓存）。
// 走 idx_stop_station_depart 联合索引，不做全表扫描。
func (s *Store) SegmentsFrom(station string) ([]Segment, error) {
	s.mu.Lock()
	if segs, ok := s.segCache[station]; ok {
		s.mu.Unlock()
		return segs, nil
	}
	s.mu.Unlock()

	cols := make([]string, 0, len(seatColumns))
	for _, sc := range seatColumns {
		cols = append(cols, seatDiffExpr(sc.Col))
	}
	query := `
		SELECT b.train_no, b.train_type, b.depart_time, b.depart_elapsed,
		       s.station_name, s.arrive_time, s.arrive_elapsed,
		       s.km - b.km,
		       ` + strings.Join(cols, ",\n\t\t       ") + `
		FROM stop_schedule b
		JOIN stop_schedule s
		  ON s.full_code = b.full_code
		 AND s.stop_seq > b.stop_seq
		 AND s.stop_seq <= b.stop_seq + ?
		WHERE b.station_name = ?
		ORDER BY b.depart_time, s.stop_seq`

	rows, err := s.db.Query(query, config.MaxSegmentsPerStation, station)
	if err != nil {
		return nil, fmt.Errorf("查询 %s 出发车次失败: %w", station, err)
	}
	defer rows.Close()

	var segs []Segment
	var prices [11]float64
	for rows.Next() {
		var (
			seg   Segment
			dep   string
			arr   string
		)
		seg.From = station
		args := []any{&seg.TrainNo, &seg.TrainType, &dep, &seg.DepartElapsed,
			&seg.To, &arr, &seg.ArriveElapsed, &seg.KM}
		for i := range prices {
			prices[i] = 0
			args = append(args, &prices[i])
		}
		if err := rows.Scan(args...); err != nil {
			return nil, err
		}
		seg.DepartHM = ClockToMin(dep)
		seg.ArriveHM = ClockToMin(arr)
		if seg.KM < 0 {
			seg.KM = 0
		}
		// 席别可用性由累计价差值判断；价格优先取真实 OD 票价，其次曲线（国铁按里程独立计价）
		tt := seg.TrainType
		km := seg.KM
		fr, to := seg.From, seg.To
		seg.Fares = model.FareSet{
			HardSeat:     s.pickFare(fr, to, tt, model.SeatHardSeat, km, prices[0]),
			HardSleepUp:  s.pickFare(fr, to, tt, model.SeatHardSleepUp, km, prices[1]),
			HardSleepMid: s.pickFare(fr, to, tt, model.SeatHardSleepMid, km, prices[2]),
			HardSleepLow: s.pickFare(fr, to, tt, model.SeatHardSleepLow, km, prices[3]),
			SoftSleepUp:  s.pickFare(fr, to, tt, model.SeatSoftSleepUp, km, prices[4]),
			SoftSleepLow: s.pickFare(fr, to, tt, model.SeatSoftSleepLow, km, prices[5]),
			SoftSeat:     s.pickFare(fr, to, tt, model.SeatSoftSeat, km, prices[6]),
			NoSeat:       s.pickFare(fr, to, tt, model.SeatNoSeat, km, prices[7]),
			Second:       s.pickFare(fr, to, tt, model.SeatSecond, km, prices[8]),
			First:        s.pickFare(fr, to, tt, model.SeatFirst, km, prices[9]),
			Business:     s.pickFare(fr, to, tt, model.SeatBusiness, km, prices[10]),
		}
		seg.Fares = normalizeFares(seg.Fares)
		seg.Price = seg.Fares.Min()
		segs = append(segs, seg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.segCache[station] = segs
	s.mu.Unlock()
	return segs, nil
}

// DirectBest 计算「起点站集合 → 终点站集合」的直达基准。
// 规则：优先采用高铁/城际/动车的直达作为基准，只有完全没有快速直达时才回退到普速；
// 否则基准会被普速慢车拉低（例如汉中→成都 普速 ¥48／8.5 小时，会掩盖动车 ¥149／2.3 小时）。
// seat 为指定席别时只统计售该席别的车次（auto/空 = 按最低价席别）。
func (s *Store) DirectBest(froms, tos []string, seat model.Seat) DirectInfo {
	goal := make(map[string]bool, len(tos))
	for _, t := range tos {
		goal[t] = true
	}
	if best := s.scanDirect(froms, goal, seat, true); best.OK {
		best.Fast = true
		return best
	}
	return s.scanDirect(froms, goal, seat, false)
}

// scanDirect 扫描直达段，取**票价最低的那一趟**作为基准（价格与耗时来自同一真实车次）。
// 取最低价而非最短耗时：比价场景下用户真正会比较的是「最便宜的那班快速车」；
// 若取「耗时最短的那趟」，往往是贵车或只到远郊站，会把所有方案都衬托成"更省钱"。
// fastOnly = true 时只统计高铁/城际/动车。
func (s *Store) scanDirect(froms []string, goal map[string]bool, seat model.Seat, fastOnly bool) DirectInfo {
	var best DirectInfo
	for _, from := range froms {
		segs, err := s.SegmentsFrom(from)
		if err != nil {
			continue
		}
		for _, seg := range segs {
			if !goal[seg.To] {
				continue
			}
			if fastOnly && !isFastTrain(seg.TrainType) {
				continue
			}
			price := segPriceFor(seg, seat)
			if price <= 0 {
				continue
			}
			d := seg.Duration()
			if !best.OK || price < best.Price || (price == best.Price && d < best.Minutes) {
				best = DirectInfo{Price: price, Minutes: d, OK: true, TrainNo: seg.TrainNo}
			}
		}
	}
	return best
}

// MinDirectPriceTo 从 station 直达终点集合的最低票价（A* 启发下界，无直达返回 0）
func (s *Store) MinDirectPriceTo(station string, goal map[string]bool, seat model.Seat) float64 {
	segs, err := s.SegmentsFrom(station)
	if err != nil {
		return 0
	}
	var min float64
	found := false
	for _, seg := range segs {
		if !goal[seg.To] {
			continue
		}
		v := segPriceFor(seg, seat)
		if v <= 0 {
			continue
		}
		if !found || v < min {
			min = v
			found = true
		}
	}
	if !found {
		return 0
	}
	return min
}

// segPriceFor 取该段在指定席别下的票价（auto/空 = 最低价席别）
func segPriceFor(seg Segment, seat model.Seat) float64 {
	if seat == "" || seat == model.SeatAuto {
		return seg.Price
	}
	return seg.Fares.Get(seat)
}

// Meta 读取元信息（票价抓取日期等），不存在返回空串
func (s *Store) Meta(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return v
}

// DB 暴露底层连接（供数据更新模块写入）
func (s *Store) DB() *sql.DB { return s.db }

// ODList 全路网有直达车次的站对（按车次数降序），首次调用后缓存
func (s *Store) ODList() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.odList != nil {
		return s.odList
	}
	rows, err := s.db.Query(`
		SELECT b.station_name, s2.station_name, COUNT(*) AS c
		FROM stop_schedule b
		JOIN stop_schedule s2 ON s2.full_code = b.full_code AND s2.stop_seq > b.stop_seq
		GROUP BY 1, 2
		ORDER BY c DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type kv struct {
		a, b string
		n    int
	}
	var list []kv
	for rows.Next() {
		var k kv
		if rows.Scan(&k.a, &k.b, &k.n) == nil {
			list = append(list, k)
		}
	}
	out := make([][2]string, 0, len(list))
	for _, k := range list {
		out = append(out, [2]string{k.a, k.b})
	}
	s.odList = out
	return out
}

// Telecodes 站名 → 电报码，首次调用后缓存
func (s *Store) Telecodes() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.telecodes != nil {
		return s.telecodes
	}
	m := map[string]string{}
	rows, err := s.db.Query(`SELECT station_name, telegraph_code FROM station WHERE telegraph_code <> ''`)
	if err == nil {
		for rows.Next() {
			var n, c string
			if rows.Scan(&n, &c) == nil {
				m[n] = c
			}
		}
		rows.Close()
	}
	s.telecodes = m
	return m
}

// ReloadODFares 票价导入后重新加载缓存并清空段缓存（立即生效，无需重启）
func (s *Store) ReloadODFares() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.odFares = make(map[string]float64)
	_ = s.loadODFares()
	s.segCache = make(map[string][]Segment)
}

// SuggestStations 车站/城市联想：优先前缀匹配，其次包含匹配。
// 返回形如 "重庆（城市·4站）" / "重庆北" 的候选项。
func (s *Store) SuggestStations(q string, limit int) []string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	if limit <= 0 {
		limit = 10
	}
	type cand struct {
		text string
		rank int
	}
	var out []cand
	seen := map[string]bool{}

	// 城市优先
	for city, list := range s.cities {
		if city == q || strings.HasPrefix(city, q) {
			if !seen[city] {
				seen[city] = true
				out = append(out, cand{fmt.Sprintf("%s（城市·%d站）", city, len(list)), 0})
			}
		}
	}
	// 车站（站名与所属城市同名时跳过：城市项已覆盖该站，避免下拉出现两个「西安」）
	for name, st := range s.stations {
		if name == st.City {
			continue
		}
		if name == q || strings.HasPrefix(name, q) {
			if !seen[name] {
				seen[name] = true
				out = append(out, cand{name, 1})
			}
		} else if strings.Contains(name, q) {
			if !seen[name] {
				seen[name] = true
				out = append(out, cand{name, 2})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].rank != out[j].rank {
			return out[i].rank < out[j].rank
		}
		return out[i].text < out[j].text
	})
	if len(out) > limit {
		out = out[:limit]
	}
	res := make([]string, 0, len(out))
	for _, c := range out {
		res = append(res, c.text)
	}
	return res
}

// normalizeFares 修正段价：保证同族席别价格序（硬座 ≤ 硬卧上 ≤ 中 ≤ 下 ≤ 软卧；
// 二等座 ≤ 一等座 ≤ 商务座），避免源数据偶发倒挂导致「商务座比二等座便宜」这类结果。
func normalizeFares(f model.FareSet) model.FareSet {
	asc := func(vals ...*float64) {
		prev := 0.0
		for _, p := range vals {
			if *p <= 0 {
				continue
			}
			if prev > 0 && *p < prev {
				*p = prev
			}
			prev = *p
		}
	}
	asc(&f.HardSeat, &f.HardSleepUp, &f.HardSleepMid, &f.HardSleepLow)
	asc(&f.HardSleepLow, &f.SoftSleepUp, &f.SoftSleepLow)
	asc(&f.Second, &f.First, &f.Business)
	return f
}

// ClockToMin 'HH:MM' → 分钟；无法解析返回 0
func ClockToMin(s string) int {
	var h, m int
	if n, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || n != 2 {
		return 0
	}
	if h < 0 || h > 47 || m < 0 || m > 59 { // 容忍个别 >24 的脏数据
		return 0
	}
	return h*60 + m
}
