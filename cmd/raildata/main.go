// Command raildata 把抓取到的车次详情宽表导入本地 SQLite。
//
// 数据源：data/csv/stop_detail.csv —— 由 tools/fetch_fares.py 从车次详情页解析而来，
// 一行 = 一个停靠站，含累计里程、累计运行时长、以及全部席别的「自始发站累计票价」。
//
// 用法（在工程根目录执行）：
//
//	raildata -details data/csv/stop_detail.csv -db data/rail.db
//
// 本工具与查询主程序完全解耦；除 -fetch-stationcodes 外不做任何网络访问。
package main

import (
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"railplanner/internal/console"
	"railplanner/internal/model"
)

// seatCols CSV 中席别列顺序 ↔ FareSet 字段
var seatCols = []struct{ CSV, Field string }{
	{"硬座", "HardSeat"},
	{"硬卧上", "HardSleepUp"},
	{"硬卧中", "HardSleepMid"},
	{"硬卧下", "HardSleepLow"},
	{"软卧上", "SoftSleepUp"},
	{"软卧下", "SoftSleepLow"},
	{"软座", "SoftSeat"},
	{"无座", "NoSeat"},
	{"二等座", "Second"},
	{"一等座", "First"},
	{"商务座", "Business"},
}

// stopRow 一行停靠站数据
type stopRow struct {
	Seq        int
	Station    string
	KM         int
	ArriveHM   int
	DepartHM   int
	ArrElapsed int
	DepElapsed int
	Fares      model.FareSet
}

func main() {
	console.Init()
	detailFile := flag.String("details", filepath.Join("data", "csv", "stop_detail.csv"), "车次详情宽表（时刻表+里程+席别票价）")
	odFile := flag.String("odfare", filepath.Join("data", "csv", "od_fare.csv"), "真实 OD 票价表（优先数据源）")
	od12306 := flag.String("odfare12306", filepath.Join("data", "csv", "od_fare_12306.csv"), "12306 官方抓取的真实票价（优先级最高）")
	outDB := flag.String("db", filepath.Join("data", "rail.db"), "输出 SQLite 路径")
	schemaFile := flag.String("schema", "schema.sql", "建表 SQL 路径")
	codeFile := flag.String("stationcodes", filepath.Join("data", "csv", "station_code.csv"), "车站电报码表")
	fetchCodes := flag.Bool("fetch-stationcodes", false, "联网抓取一次 12306 车站码表（低频，仅此一处联网）")
	flag.Parse()

	if *fetchCodes {
		if err := fetchStationCodes(*codeFile); err != nil {
			fmt.Println("抓取车站码表失败:", err)
		}
	}

	if _, err := os.Stat(*detailFile); err != nil {
		fatal(fmt.Errorf("未找到 %s，请先运行 tools/fetch_fares.py 抓取车次详情", *detailFile))
	}
	if err := os.MkdirAll(filepath.Dir(*outDB), 0o755); err != nil {
		fatal(err)
	}
	// 重建数据库：必须先删掉旧库。此处**不能静默忽略失败**——
	// 若旧库被占用（例如 railplan-gui 正在运行），删除会失败，而随后的
	// CREATE TABLE IF NOT EXISTS + INSERT OR REPLACE 会与旧数据混合，
	// 出现"旧车次残留、票价半新半旧"的脏数据。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := *outDB + suffix
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			fatal(fmt.Errorf("无法删除旧数据库 %s：%v\n请先关闭正在使用它的程序（如 railplan-gui / railplan）后重试", p, err))
		}
	}

	db, err := sql.Open("sqlite", *outDB)
	if err != nil {
		fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if err := execSchema(db, *schemaFile); err != nil {
		fatal(err)
	}

	codes := loadStationCodes(*codeFile)
	fmt.Printf("车站码表 %d 条\n", len(codes))

	start := time.Now()
	trains, stops, err := importFromDetail(db, *detailFile, codes)
	if err != nil {
		fatal(err)
	}
	if err := refineCities(db); err != nil {
		fatal(err)
	}
	if n, err := importODFare(db, *odFile); err != nil {
		fatal(err)
	} else if n > 0 {
		fmt.Printf("真实 OD 票价：%d 条\n", n)
	}
	// 12306 官方抓取的票价优先级最高，放在最后导入以覆盖前面的估算值
	fareDate := time.Now().Format("2006-01-02")
	if n, err := importODFare12306(db, *od12306, fareDate); err != nil {
		fatal(err)
	} else if n > 0 {
		fmt.Printf("12306 官方票价：%d 条（覆盖同 OD 记录）\n", n)
	}
	if err := postProcess(db); err != nil {
		fatal(err)
	}

	fi, _ := os.Stat(*outDB)
	fmt.Printf("完成：车次 %d，经停记录 %d，耗时 %s，数据库 %.1f MB\n",
		trains, stops, time.Since(start).Round(time.Millisecond),
		float64(fi.Size())/1024/1024)
	fmt.Printf("数据库：%s\n", *outDB)
}

func fatal(err error) {
	fmt.Println("错误:", err)
	os.Exit(1)
}

func execSchema(db *sql.DB, schemaFile string) error {
	b, err := os.ReadFile(schemaFile)
	if err != nil {
		return fmt.Errorf("读取 schema 失败: %w", err)
	}
	if _, err := db.Exec(string(b)); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}
	return nil
}

// ── 导入 ────────────────────────────────────────────────────

func importFromDetail(db *sql.DB, path string, codes map[string]string) (int, int, error) {
	byTrain, types, err := loadDetail(path)
	if err != nil {
		return 0, 0, err
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	const insTrainSQL = `INSERT OR REPLACE INTO train
		(full_code, train_no, train_type, grade, start_station, end_station, stop_count)
		VALUES (?,?,?,?,?,?,?)`
	const insStopSQL = `INSERT OR REPLACE INTO stop_schedule
		(full_code, stop_seq, train_no, train_type, station_name, arrive_time, depart_time,
		 arrive_elapsed, depart_elapsed, km, base_price,
		 p_hard_seat, p_hard_sleep_up, p_hard_sleep_mid, p_hard_sleep_low,
		 p_soft_sleep_up, p_soft_sleep_low, p_soft_seat, p_no_seat,
		 p_second, p_first, p_business)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	insTrain, err := tx.Prepare(insTrainSQL)
	if err != nil {
		return 0, 0, err
	}
	insStop, err := tx.Prepare(insStopSQL)
	if err != nil {
		return 0, 0, err
	}
	insStation, err := tx.Prepare(`INSERT OR REPLACE INTO station
		(station_name, telegraph_code, city_name) VALUES (?,?,?)`)
	if err != nil {
		return 0, 0, err
	}

	stationSet := map[string]bool{}
	trainCount, stopCount, skipped := 0, 0, 0
	for _, trainNo := range sortedKeys(byTrain) {
		rows := byTrain[trainNo]
		if len(rows) < 2 {
			skipped++
			continue
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
		trainType := types[trainNo]
		grade := trainNo[:1]

		// 里程与累计耗时做单调性修正（脏数据保护）
		for i := 1; i < len(rows); i++ {
			if rows[i].KM < rows[i-1].KM {
				rows[i].KM = rows[i-1].KM
			}
			if rows[i].ArrElapsed < rows[i-1].DepElapsed {
				rows[i].ArrElapsed = rows[i-1].DepElapsed
			}
			if rows[i].DepElapsed < rows[i].ArrElapsed {
				rows[i].DepElapsed = rows[i].ArrElapsed
			}
		}
		if _, err := insTrain.Exec(trainNo, trainNo, trainType, grade,
			rows[0].Station, rows[len(rows)-1].Station, len(rows)); err != nil {
			return trainCount, stopCount, err
		}
		for i, r := range rows {
			f := r.Fares
			if _, err := insStop.Exec(trainNo, i+1, trainNo, trainType, r.Station,
				minToClock(r.ArriveHM), minToClock(r.DepartHM),
				r.ArrElapsed, r.DepElapsed, r.KM, f.Min(),
				f.HardSeat, f.HardSleepUp, f.HardSleepMid, f.HardSleepLow,
				f.SoftSleepUp, f.SoftSleepLow, f.SoftSeat, f.NoSeat,
				f.Second, f.First, f.Business); err != nil {
				return trainCount, stopCount, err
			}
			if !stationSet[r.Station] {
				stationSet[r.Station] = true
				if _, err := insStation.Exec(r.Station, codes[r.Station], inferCity(r.Station)); err != nil {
					return trainCount, stopCount, err
				}
			}
			stopCount++
		}
		trainCount++
		if trainCount%2000 == 0 {
			if err := tx.Commit(); err != nil {
				return trainCount, stopCount, err
			}
			if tx, err = db.Begin(); err != nil {
				return trainCount, stopCount, err
			}
			insTrain, _ = tx.Prepare(insTrainSQL)
			insStop, _ = tx.Prepare(insStopSQL)
			insStation, _ = tx.Prepare(`INSERT OR REPLACE INTO station
				(station_name, telegraph_code, city_name) VALUES (?,?,?)`)
		}
	}
	if err := tx.Commit(); err != nil {
		return trainCount, stopCount, err
	}
	fmt.Printf("车站 %d 座（跳过站数不足的车次 %d 个）\n", len(stationSet), skipped)
	return trainCount, stopCount, nil
}

// refineCities 二次归并：站名以某个「已知城市名」开头时归入该城市。
// 只按方位后缀推断会漏掉「北京朝阳」「上海金山」这类站名，导致输入城市名时检索不到。
func refineCities(db *sql.DB) error {
	rows, err := db.Query(`SELECT DISTINCT city_name FROM station`)
	if err != nil {
		return err
	}
	cities := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return err
		}
		cities = append(cities, c)
	}
	rows.Close()
	// 只保留 ≥2 个汉字的城市名，避免把「汉」「南」这类单字误当成城市前缀
	kept := cities[:0]
	for _, c := range cities {
		if len(c) >= 6 {
			kept = append(kept, c)
		}
	}
	cities = kept
	sort.Slice(cities, func(i, j int) bool { return len(cities[i]) > len(cities[j]) })

	stRows, err := db.Query(`SELECT station_name, city_name FROM station`)
	if err != nil {
		return err
	}
	type upd struct{ name, city string }
	var updates []upd
	for stRows.Next() {
		var name, city string
		if err := stRows.Scan(&name, &city); err != nil {
			stRows.Close()
			return err
		}
		for _, c := range cities { // 已按长度降序，首个命中即最长匹配
			if _, special := citySpecial[name]; special {
				break // 特例表（机场等）优先，不参与归并
			}
			if c != name && strings.HasPrefix(name, c) {
				if c != city {
					updates = append(updates, upd{name, c})
				}
				break
			}
		}
	}
	stRows.Close()

	if len(updates) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE station SET city_name=? WHERE station_name=?`)
	if err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := stmt.Exec(u.city, u.name); err != nil {
			return err
		}
	}
	fmt.Printf("城市归并：修正 %d 个车站的归属城市\n", len(updates))
	return tx.Commit()
}

// importODFare12306 导入 12306 官方抓取的真实票价（优先级最高）。
// 同一 (出发站,到达站,车次类型,席别) 有多趟车时取**最低价**——比价场景下用户关心最便宜的。
func importODFare12306(db *sql.DB, path, date string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Printf("提示：未找到 12306 官方票价 %s（可用 tools/fetch_12306_fare.py 抓取）\n", path)
		return 0, nil
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	type key struct{ from, to, tt, seat string }
	best := map[key]float64{}
	first := true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(rec) < 6 {
			continue
		}
		if first {
			first = false
			if strings.EqualFold(strings.TrimSpace(rec[0]), "from_station") {
				continue
			}
		}
		k := key{strings.TrimSpace(rec[0]), strings.TrimSpace(rec[1]),
			strings.TrimSpace(rec[3]), strings.TrimSpace(rec[4])}
		price, perr := strconv.ParseFloat(strings.TrimSpace(rec[5]), 64)
		if k.from == "" || k.to == "" || k.seat == "" || perr != nil || price <= 0 {
			continue
		}
		if old, ok := best[k]; !ok || price < old {
			best[k] = price
		}
	}
	if len(best) == 0 {
		return 0, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO od_fare
		(from_station, to_station, train_type, seat, km, price) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	n := 0
	for k, price := range best {
		// 里程沿用已有记录（若没有则记 0，查询端不依赖它）
		var km int
		_ = tx.QueryRow(`SELECT km FROM od_fare WHERE from_station=? AND to_station=? AND train_type=? AND seat=?`,
			k.from, k.to, k.tt, k.seat).Scan(&km)
		if _, err := stmt.Exec(k.from, k.to, k.tt, k.seat, km, price); err != nil {
			return n, err
		}
		n++
	}
	// 记录 12306 票价的抓取日期（用于界面提示数据时效）
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('fare_12306_date', ?)`, date); err != nil {
		return n, err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('fare_12306_count', ?)`, strconv.Itoa(n)); err != nil {
		return n, err
	}
	if err := tx.Commit(); err != nil {
		return n, err
	}
	return n, nil
}

// importODFare 导入真实 OD 票价表
func importODFare(db *sql.DB, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Printf("提示：未找到 OD 票价表 %s，将退回曲线估算\n", path)
		return 0, nil
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO od_fare
		(from_station, to_station, train_type, seat, km, price) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	n, first := 0, true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(rec) < 6 {
			continue
		}
		if first {
			first = false
			if strings.EqualFold(strings.TrimSpace(rec[0]), "from_station") {
				continue
			}
		}
		km, e1 := strconv.Atoi(strings.TrimSpace(rec[4]))
		price, e2 := strconv.ParseFloat(strings.TrimSpace(rec[5]), 64)
		if e1 != nil || e2 != nil || price <= 0 {
			continue
		}
		if _, err := stmt.Exec(strings.TrimSpace(rec[0]), strings.TrimSpace(rec[1]),
			strings.TrimSpace(rec[2]), strings.TrimSpace(rec[3]), km, price); err != nil {
			return n, err
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return n, err
	}
	return n, nil
}

// loadDetail 读取宽表 CSV → 车次 → 停靠站列表
func loadDetail(path string) (map[string][]stopRow, map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	byTrain := map[string][]stopRow{}
	types := map[string]string{}
	first := true
	line := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		line++
		if first {
			first = false
			if strings.EqualFold(strings.TrimSpace(rec[0]), "train_no") {
				continue
			}
		}
		if len(rec) < 9+len(seatCols) {
			continue
		}
		trainNo := strings.TrimSpace(rec[0])
		if trainNo == "" {
			continue
		}
		seq, _ := strconv.Atoi(strings.TrimSpace(rec[2]))
		km, _ := strconv.Atoi(strings.TrimSpace(rec[4]))
		aEl, _ := strconv.Atoi(strings.TrimSpace(rec[7]))
		dEl, _ := strconv.Atoi(strings.TrimSpace(rec[8]))
		var fs model.FareSet
		for i, sc := range seatCols {
			v, _ := strconv.ParseFloat(strings.TrimSpace(rec[9+i]), 64)
			fs = setSeat(fs, sc.Field, v)
		}
		byTrain[trainNo] = append(byTrain[trainNo], stopRow{
			Seq:        seq,
			Station:    strings.TrimSpace(rec[3]),
			KM:         km,
			ArriveHM:   hm(strings.TrimSpace(rec[5])),
			DepartHM:   hm(strings.TrimSpace(rec[6])),
			ArrElapsed: aEl,
			DepElapsed: dEl,
			Fares:      fs,
		})
		if types[trainNo] == "" {
			types[trainNo] = strings.TrimSpace(rec[1])
		}
	}
	if len(byTrain) == 0 {
		return nil, nil, fmt.Errorf("CSV 中没有有效记录（读入 %d 行）", line)
	}
	return byTrain, types, nil
}

// setSeat 按 FareSet 字段名写入票价
func setSeat(fs model.FareSet, field string, v float64) model.FareSet {
	switch field {
	case "HardSeat":
		fs.HardSeat = v
	case "HardSleepUp":
		fs.HardSleepUp = v
	case "HardSleepMid":
		fs.HardSleepMid = v
	case "HardSleepLow":
		fs.HardSleepLow = v
	case "SoftSleepUp":
		fs.SoftSleepUp = v
	case "SoftSleepLow":
		fs.SoftSleepLow = v
	case "SoftSeat":
		fs.SoftSeat = v
	case "NoSeat":
		fs.NoSeat = v
	case "Second":
		fs.Second = v
	case "First":
		fs.First = v
	case "Business":
		fs.Business = v
	}
	return fs
}

func sortedKeys(m map[string][]stopRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── 车站 ────────────────────────────────────────────────────

var citySpecial = map[string]string{
	"上海虹桥": "上海", "上海南": "上海", "北京丰台": "北京", "北京大兴": "北京",
	"深圳机场": "深圳", "深圳机场北": "深圳", "江北机场": "重庆", "天河机场": "武汉",
	"大兴机场": "北京", "中川机场": "兰州", "吴圩机场": "南宁", "双流机场": "成都",
	"新郑机场": "郑州", "郑州航空港": "郑州", "天河机场北": "武汉",
}

// inferCity 由站名推断归属城市（去掉东/南/西/北等方位后缀）
func inferCity(name string) string {
	if c, ok := citySpecial[name]; ok {
		return c
	}
	r := []rune(name)
	if len(r) > 2 {
		switch string(r[len(r)-1]) {
		case "东", "南", "西", "北":
			if len(r)-1 >= 2 {
				return string(r[:len(r)-1])
			}
		}
	}
	return name
}

func loadStationCodes(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	first := true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(rec) < 2 {
			continue
		}
		if first {
			first = false
			if strings.EqualFold(strings.TrimSpace(rec[0]), "station_name") {
				continue // 跳过表头，否则会写入一条「station_name→telegraph_code」脏记录
			}
		}
		out[strings.TrimSpace(rec[0])] = strings.TrimSpace(rec[1])
	}
	return out
}

// fetchStationCodes 低频抓取一次 12306 静态车站码表（唯一联网动作）
func fetchStationCodes(out string) error {
	url := "https://kyfw.12306.cn/otn/resources/js/framework/station_name.js"
	fmt.Println("抓取 12306 车站码表（单次请求）...")
	time.Sleep(time.Second) // 请求间隔 ≥1s，低频访问
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	txt := string(body)
	start := strings.Index(txt, "@")
	if start < 0 {
		return fmt.Errorf("码表格式异常")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"station_name", "telegraph_code"})
	count := 0
	for _, item := range strings.Split(txt[start+1:], "@") {
		parts := strings.Split(item, "|")
		if len(parts) < 3 {
			continue
		}
		_ = w.Write([]string{parts[1], parts[2]})
		count++
	}
	w.Flush()
	fmt.Printf("车站码表已保存 %d 条 → %s\n", count, out)
	return nil
}

// ── 收尾优化 ───────────────────────────────────────────────

func postProcess(db *sql.DB) error {
	fmt.Println("重建索引 / VACUUM / WAL ...")
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_stop_station_depart ON stop_schedule(station_name, depart_time)`,
		`CREATE INDEX IF NOT EXISTS idx_stop_train_seq      ON stop_schedule(full_code, stop_seq)`,
		`CREATE INDEX IF NOT EXISTS idx_station_city        ON station(city_name)`,
		`ANALYZE`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`VACUUM`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// ── 工具函数 ───────────────────────────────────────────────

func hm(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "---") {
		return 0
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0
	}
	return h*60 + m
}

func minToClock(m int) string {
	if m < 0 {
		m = 0
	}
	return fmt.Sprintf("%02d:%02d", (m/60)%24, m%60)
}
