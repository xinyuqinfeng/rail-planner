package store

// 旅行模式数据查询（与常规查询完全独立的新增路径）

import (
	"strings"
)

// TravelDestRow 一个目的站的真实最低价（任意席别/车次类型取最低）
type TravelDestRow struct {
	ToStation string
	City      string
	TrainType string
	Seat      string
	Price     float64
	Km        int
}

// TravelDests 查询从出发站集合可达的所有目的站真实最低价。
// 数据源为 od_fare（12306 官方价 / 车次详情页真实可售价），未覆盖的区间不返回。
func (s *Store) TravelDests(froms []string, limit int) []TravelDestRow {
	if len(froms) == 0 {
		return nil
	}
	ph := strings.Repeat("?,", len(froms))
	ph = ph[:len(ph)-1]
	args := make([]any, 0, len(froms))
	for _, f := range froms {
		args = append(args, f)
	}
	// 注意：LIMIT 作用于「目的站 × 车次类型 × 席别」的分组行数，
	// 而非目的站数量——同一站可能有多行。调用方请给足余量，否则
	// 排序靠后（较贵）的目的站会被截断。
	rows, err := s.db.Query(`
		SELECT f.to_station,
		       COALESCE(s.city_name, f.to_station),
		       f.train_type, f.seat, MIN(f.price), MAX(f.km)
		FROM od_fare f
		LEFT JOIN station s ON s.station_name = f.to_station
		WHERE f.from_station IN (`+ph+`) AND f.price > 0
		GROUP BY f.to_station, f.train_type, f.seat
		ORDER BY 5 ASC
		LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	// 同一目的站可能有多条（不同席别/车次类型），保留最低价那条
	best := map[string]TravelDestRow{}
	order := []string{}
	for rows.Next() {
		var r TravelDestRow
		var city string
		if err := rows.Scan(&r.ToStation, &city, &r.TrainType, &r.Seat, &r.Price, &r.Km); err != nil {
			continue
		}
		r.City = city
		if old, ok := best[r.ToStation]; !ok || r.Price < old.Price {
			if !ok {
				order = append(order, r.ToStation)
			}
			best[r.ToStation] = r
		}
	}
	out := make([]TravelDestRow, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}
