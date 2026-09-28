// Package fare12306 从 12306 官方接口批量抓取真实区间票价。
//
// 接口要点（已实测验证）：
//   - GET /otn/leftTicket/init 的 Set-Cookie 即可用，无需 RAIL_DEVICEID
//   - /otn/leftTicket/query 返回 {"c_url":"leftTicket/queryG"} 做端点重定向
//   - 结果每行按 '|' 分割，第 39 字段 yp_info 每 10 字符一个价格单元：
//     首字符=席别码（O=二等座 M=一等座 9=商务座 3=硬卧 1/2=硬座 W=无座），
//     第 2~6 位 ÷10 = 元，第 7~10 位 ≥3000 表示无座
package fare12306

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	apiBase  = "https://kyfw.12306.cn"
	initURL  = apiBase + "/otn/leftTicket/init"
	uaString = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

var seatCode = map[string]string{
	"O": "second", "M": "first", "9": "business", "P": "business",
	"3": "hard_sleep", "S": "soft_sleep", "6": "soft_sleep",
	"4": "soft_seat", "1": "hard_seat", "2": "hard_seat", "W": "no_seat",
}

// Fetcher 单例抓取器（GUI 进程内运行，goroutine 驱动）
type Fetcher struct {
	mu      sync.Mutex
	running bool
	stop    atomic.Bool

	cookie string
	path   string
	client *http.Client

	Done      atomic.Int64 // 本轮已处理 OD 数
	Total     atomic.Int64 // 本轮 OD 总数
	OKCount   atomic.Int64
	FailCount atomic.Int64
	RowsCount atomic.Int64 // 新增票价行数
	LastErr   atomic.Value // string
	Date      atomic.Value // string 乘车日期
	StartedAt atomic.Value // time.Time
}

// New 创建抓取器
func New() *Fetcher {
	f := &Fetcher{client: &http.Client{
		Timeout: 25 * time.Second,
		// 不跟随重定向：12306 用 302 + JSON body 传递端点变化（{"c_url":...}），
		// 若跟随会落到登录页 HTML，导致"响应非 JSON"
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
	f.LastErr.Store("")
	f.Date.Store("")
	return f
}

// Running 是否正在抓取
func (f *Fetcher) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

// Stop 请求停止（当前 OD 完成后退出，进度已落盘）
func (f *Fetcher) Stop() { f.stop.Store(true) }

// Start 启动抓取。ods 为待抓站对（按热度排序），codes 为站名→电报码。
func (f *Fetcher) Start(ods [][2]string, codes map[string]string, date, outCSV, doneTxt string, minGap float64) error {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return fmt.Errorf("抓取已在运行中")
	}
	f.running = true
	f.stop.Store(false)
	f.mu.Unlock()

	go f.run(ods, codes, date, outCSV, doneTxt, minGap)
	return nil
}

func (f *Fetcher) run(ods [][2]string, codes map[string]string, date, outCSV, doneTxt string, minGap float64) {
	defer func() {
		f.mu.Lock()
		f.running = false
		f.mu.Unlock()
	}()
	f.Date.Store(date)
	f.StartedAt.Store(time.Now())
	f.Total.Store(int64(len(ods)))

	// 断点续抓：读已完成集合
	doneSet := map[string]bool{}
	if b, err := os.ReadFile(doneTxt); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				doneSet[l] = true
			}
		}
	}
	appendMode := false
	if _, err := os.Stat(outCSV); err == nil {
		appendMode = true
	}
	_ = os.MkdirAll(filepath.Dir(outCSV), 0o755)

	var fout *os.File
	var err error
	if appendMode {
		fout, err = os.OpenFile(outCSV, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	} else {
		fout, err = os.Create(outCSV)
		if err == nil {
			_, _ = fout.WriteString("from_station,to_station,train_no,train_type,seat,price,date\n")
		}
	}
	if err != nil {
		f.LastErr.Store("无法打开输出文件: " + err.Error())
		return
	}
	defer fout.Close()
	w := csv.NewWriter(fout)

	fdone, err := os.OpenFile(doneTxt, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		defer fdone.Close()
	}

	f.refreshCookie()
	var lastFlush time.Time

	for _, od := range ods {
		if f.stop.Load() {
			break
		}
		key := od[0] + "|" + od[1]
		if doneSet[key] {
			continue
		}
		fc, tc := codes[od[0]], codes[od[1]]
		if fc == "" || tc == "" || fc == tc {
			continue
		}

		got := false
		for attempt := 0; attempt < 3; attempt++ {
			if f.stop.Load() {
				w.Flush()
				return
			}
			if attempt > 0 {
				time.Sleep(time.Duration(1500*attempt) * time.Millisecond)
				f.refreshCookie()
			}
			if err := f.fetchOne(fc, tc, od[0], od[1], date, w); err != nil {
				f.LastErr.Store(err.Error())
				continue
			}
			got = true
			break
		}
		f.Done.Add(1)
		if got {
			f.OKCount.Add(1)
			doneSet[key] = true
			if fdone != nil {
				_, _ = fdone.WriteString(key + "\n")
			}
		} else {
			f.FailCount.Add(1)
		}
		if time.Since(lastFlush) > 5*time.Second {
			w.Flush()
			_ = fout.Sync()
			lastFlush = time.Now()
		}
		time.Sleep(time.Duration(minGap*1000*(0.8+0.5*rand.Float64())) * time.Millisecond)
	}
	w.Flush()
}

func (f *Fetcher) fetchOne(fc, tc, fromName, toName, date string, w *csv.Writer) error {
	if f.path == "" {
		f.resolvePath(fc, tc, date)
	}
	u := fmt.Sprintf("%s/otn/%s?leftTicketDTO.train_date=%s&leftTicketDTO.from_station=%s&leftTicketDTO.to_station=%s&purpose_codes=ADULT",
		apiBase, f.path, date, urlQueryEscape(fc), urlQueryEscape(tc))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", uaString)
	req.Header.Set("Cookie", f.cookie)
	req.Header.Set("Referer", initURL)
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// 302/301：12306 用它传递新的查询端点（body 里是 {"c_url":...}），更新后由外层重试
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently ||
		resp.StatusCode == http.StatusTemporaryRedirect {
		var d struct {
			CURL string `json:"c_url"`
		}
		if json.Unmarshal(body, &d) == nil && d.CURL != "" {
			f.path = d.CURL
			return fmt.Errorf("端点已变更，重试新端点 %s", d.CURL)
		}
		return fmt.Errorf("HTTP %d 重定向", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var d struct {
		Data struct {
			Result []string `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		// 带上响应开头，便于定位（限流提示 / 登录跳转 / 端点变化等）
		head := string(body)
		if len(head) > 150 {
			head = head[:150]
		}
		return fmt.Errorf("响应非 JSON: %s", strings.ReplaceAll(head, "\n", " "))
	}
	for _, row := range d.Data.Result {
		fl := strings.Split(row, "|")
		if len(fl) < 40 {
			continue
		}
		grade := "普速"
		switch {
		case strings.HasPrefix(fl[3], "G"):
			grade = "高铁"
		case strings.HasPrefix(fl[3], "C"):
			grade = "城际"
		case strings.HasPrefix(fl[3], "D"):
			grade = "动车"
		}
		for _, sp := range parseYPInfo(fl[39]) {
			_ = w.Write([]string{fromName, toName, fl[3], grade, sp.seat,
				fmt.Sprintf("%.1f", sp.price), date})
			f.RowsCount.Add(1)
		}
	}
	return nil
}

func (f *Fetcher) resolvePath(fc, tc, date string) {
	u := fmt.Sprintf("%s/otn/leftTicket/query?leftTicketDTO.train_date=%s&leftTicketDTO.from_station=%s&leftTicketDTO.to_station=%s&purpose_codes=ADULT",
		apiBase, date, urlQueryEscape(fc), urlQueryEscape(tc))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", uaString)
	req.Header.Set("Cookie", f.cookie)
	req.Header.Set("Referer", initURL)
	resp, err := f.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var d struct {
		CURL string `json:"c_url"`
	}
	if json.NewDecoder(resp.Body).Decode(&d) == nil && d.CURL != "" {
		f.path = d.CURL
	}
}

func (f *Fetcher) refreshCookie() {
	req, _ := http.NewRequest("GET", initURL, nil)
	req.Header.Set("User-Agent", uaString)
	resp, err := f.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var jar []string
	for _, c := range resp.Cookies() {
		if c.Name != "" {
			jar = append(jar, c.Name+"="+c.Value)
		}
	}
	if len(jar) > 0 {
		f.cookie = strings.Join(jar, "; ")
	}
}

type pricePair struct {
	seat  string
	price float64
}

func parseYPInfo(s string) []pricePair {
	var out []pricePair
	for i := 0; i+10 <= len(s); i += 10 {
		seg := s[i : i+10]
		seat, ok := seatCode[seg[0:1]]
		if !ok {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(seg[1:6], "%d", &n); err != nil || n <= 0 {
			continue
		}
		p := float64(n) / 10.0
		if p > 0 {
			out = append(out, pricePair{seat: seat, price: p})
		}
	}
	return out
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "&", "%26"), "+", "%2B")
}
