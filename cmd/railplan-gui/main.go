// Command railplan-gui 图形界面版：本地离线服务 + 内嵌前端，双击即用。
//
// 启动后自动打开浏览器，输入起点/终点即可查询，支持筛选、排序与分类。
// 全部计算在本机完成，不访问任何网络。
package main

import (
	"bytes"
	"database/sql"
	"embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"railplanner/internal/config"
	"railplanner/internal/console"
	"railplanner/internal/fare12306"
	"railplanner/internal/model"
	"railplanner/internal/service"
	"railplanner/internal/store"
)

// 全局票价抓取器（GUI 进程内 goroutine 运行）
var fareFetcher = fare12306.New()

//go:embed web/*
var webFS embed.FS

//go:embed assets/rail.db
var embeddedDB []byte

//go:embed assets/version.txt
var dbVersion string

func main() {
	console.Init()

	port := flag.Int("port", 17890, "本地服务端口")
	noBrowser := flag.Bool("no-browser", false, "不自动打开浏览器")
	dbPath := flag.String("db", "", "外部数据库路径（默认使用内嵌数据库）")
	flag.Parse()

	dbFile, err := ensureDB(*dbPath)
	if err != nil {
		fatal("准备数据库失败: %v", err)
	}
	st, err := store.Open(dbFile)
	if err != nil {
		fatal("打开数据库失败: %v", err)
	}
	defer st.Close()

	mux := http.NewServeMux()
	registerAPI(mux, st, dbFile)
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		fatal("加载前端资源失败: %v", err)
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(sub))))

	ln, err := listen(*port)
	if err != nil {
		fatal("启动服务失败: %v", err)
	}
	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	fmt.Println("════════════════════════════════════════════════════════")
	fmt.Println("  离线铁路中转路径规划工具 已启动")
	fmt.Printf("  地址：%s\n", url)
	fmt.Println("  全部计算在本机完成，不访问网络；关闭本窗口即退出。")
	fmt.Println("════════════════════════════════════════════════════════")

	if !*noBrowser {
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(url)
		}()
	}
	if err := http.Serve(ln, mux); err != nil {
		fatal("服务异常退出: %v", err)
	}
}

// importFare12306 把 12306 抓取的票价 CSV 导入当前数据库（聚合最低价，覆盖旧记录），
// 并记录抓取日期到 meta。完成后由调用方触发 store.ReloadODFares() 立即生效。
func importFare12306(db *sql.DB, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("尚无抓取数据文件，请先在上方执行抓取")
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true

	type key struct{ from, to, tt, seat string }
	best := map[key]float64{}
	first := true
	date := ""
	for {
		rec, err := rd.Read()
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
		if len(rec) > 6 {
			if d := strings.TrimSpace(rec[6]); d > date {
				date = d
			}
		}
		if old, ok := best[k]; !ok || price < old {
			best[k] = price
		}
	}
	if len(best) == 0 {
		return 0, fmt.Errorf("抓取文件中没有有效票价数据")
	}
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO od_fare
		(from_station, to_station, train_type, seat, km, price)
		VALUES (?,?,?,?,?,
		COALESCE((SELECT km FROM od_fare WHERE from_station=? AND to_station=? AND train_type=? AND seat=?),0),?)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	for k, price := range best {
		if _, err := stmt.Exec(k.from, k.to, k.tt, k.seat,
			k.from, k.to, k.tt, k.seat, price); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('fare_12306_date', ?)`, date); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('fare_12306_count', ?)`,
		strconv.Itoa(len(best))); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(best), nil
}

func fatal(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
	fmt.Println("按回车键退出...")
	_, _ = fmt.Scanln()
	os.Exit(1)
}

// listen 依次尝试 port, port+1, ... 直到绑定成功
func listen(port int) (net.Listener, error) {
	var lastErr error
	for i := 0; i < 20; i++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port+i)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// ensureDB 优先使用外部数据库；否则把内嵌数据库释放到用户缓存目录。
// 缓存文件名带内容哈希，数据库更新后会自动释放新文件（用文件大小判断会在
// 新旧库体积相近时误判，曾导致界面一直显示旧数据）。
func ensureDB(external string) (string, error) {
	if external != "" {
		return external, nil
	}
	if len(embeddedDB) < 1024 {
		return "", fmt.Errorf("内嵌数据库为空，请先执行构建脚本 tools/build_gui.py 再编译")
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "railplan")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ver := strings.TrimSpace(dbVersion)
	if ver == "" {
		ver = "unknown"
	}
	path := filepath.Join(dir, "rail-"+ver+".db")
	if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(embeddedDB)) {
		return path, nil
	}
	if err := os.WriteFile(path, embeddedDB, 0o644); err != nil {
		return "", err
	}
	// 清理旧版本缓存，避免堆积
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasPrefix(e.Name(), "rail-") && e.Name() != "rail-"+ver+".db" {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return path, nil
}

func registerAPI(mux *http.ServeMux, st *store.Store, dbFile string) {
	dataDir := filepath.Dir(dbFile) // 与缓存库同目录，存放票价抓取产物
	csvPath := filepath.Join(dataDir, "od_fare_12306.csv")
	donePath := filepath.Join(dataDir, "fare12306_done.txt")

	// —— 票价数据手动更新：开始 / 进度 / 停止 / 导入 ——
	mux.HandleFunc("/api/update/start", func(w http.ResponseWriter, r *http.Request) {
		if fareFetcher.Running() {
			writeJSON(w, map[string]any{"ok": false, "msg": "抓取已在运行中"})
			return
		}
		ods := st.ODList()
		if len(ods) == 0 {
			writeJSON(w, map[string]any{"ok": false, "msg": "无法生成 OD 清单"})
			return
		}
		date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
		if err := fareFetcher.Start(ods, st.Telecodes(), date, csvPath, donePath, 1.2); err != nil {
			writeJSON(w, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "total": len(ods)})
	})

	mux.HandleFunc("/api/update/stop", func(w http.ResponseWriter, r *http.Request) {
		fareFetcher.Stop()
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/update/status", func(w http.ResponseWriter, r *http.Request) {
		doneTotal := 0
		if b, err := os.ReadFile(donePath); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.TrimSpace(l) != "" {
					doneTotal++
				}
			}
		}
		csvRows := 0
		if b, err := os.ReadFile(csvPath); err == nil {
			csvRows = bytes.Count(b, []byte("\n"))
		}
		allTotal := len(st.ODList())
		writeJSON(w, map[string]any{
			"running":   fareFetcher.Running(),
			"done":      fareFetcher.Done.Load(),
			"total":     fareFetcher.Total.Load(),
			"ok":        fareFetcher.OKCount.Load(),
			"fail":      fareFetcher.FailCount.Load(),
			"rows":      fareFetcher.RowsCount.Load(),
			"lastErr":   fareFetcher.LastErr.Load(),
			"doneTotal": doneTotal,
			"allTotal":  allTotal,
			"csvRows":   csvRows,
			"fareDate":  st.Meta("fare_12306_date"),
			"fareCount": st.Meta("fare_12306_count"),
		})
	})

	mux.HandleFunc("/api/update/import", func(w http.ResponseWriter, r *http.Request) {
		n, err := importFare12306(st.DB(), csvPath)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		st.ReloadODFares()      // 段价立即生效，无需重启
		service.ClearTravelCache() // 旅行模式结果缓存也一并失效，避免沿用旧票价
		writeJSON(w, map[string]any{"ok": true, "count": n,
			"fareDate": st.Meta("fare_12306_date")})
	})

	mux.HandleFunc("/api/seats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, service.SeatOptions())
	})

	// 旅行模式：后台任务 + 进度轮询（深度搜索约 1 秒/目的地）
	mux.HandleFunc("/api/travel/start", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		hs := q.Get("hardSleeper") == "1"
		started, from, err := service.StartTravelJob(st, q.Get("from"), days, limit, hs)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "started": started, "from": from})
	})

	mux.HandleFunc("/api/travel/progress", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, service.TravelStatus(r.URL.Query().Get("from")))
	})

	mux.HandleFunc("/api/suggest", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		writeJSON(w, st.SuggestStations(q, 12))
	})

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		stations := len(st.AllStationNames())
		writeJSON(w, map[string]any{
			"stations":   stations,
			"disclaimer": config.Disclaimer,
			"fareDate":   st.Meta("fare_12306_date"),
			"fareCount":  st.Meta("fare_12306_count"),
			"limits": map[string]any{
				"maxTransfer":   config.MaxTransferCount,
				"maxResult":     config.MaxResultCount,
				"minGap":        config.MinTransferGap,
				"timeMultiple":  config.MaxTimeMultiple,
				"saveThreshold": config.PriceSaveThreshold,
			},
		})
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		req := service.Request{
			From: q.Get("from"),
			To:   q.Get("to"),
			Date: q.Get("date"),
			Seat: model.Seat(q.Get("seat")),
		}
		if t := q.Get("time"); t != "" {
			req.StartMin = store.ClockToMin(t)
		}
		if req.From == "" || req.To == "" {
			writeError(w, "请输入起点和终点")
			return
		}
		res, err := service.Search(st, req)
		if err != nil {
			writeError(w, err.Error())
			return
		}
		writeJSON(w, res)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // 本地工具禁用缓存，避免改了数据仍返回旧响应
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Println("写出 JSON 失败:", err)
	}
}

func writeError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// noCache 本地工具，禁用缓存避免改了前端不生效
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("请手动在浏览器打开：", url)
	}
}
