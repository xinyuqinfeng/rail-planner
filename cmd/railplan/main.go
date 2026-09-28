// Command railplan 离线铁路高性价比中转路径规划工具（命令行）。
//
// 用法：
//
//	railplan -from 汉中 -to 重庆
//	railplan -from 汉中 -to 重庆 -time 08:00 -seat hard_sleep_mid -top 10
//
// 路线计算阶段全程离线，零网络请求。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"railplanner/internal/config"
	"railplanner/internal/console"
	"railplanner/internal/model"
	"railplanner/internal/service"
	"railplanner/internal/store"
)

func main() {
	console.Init()
	from := flag.String("from", "", "出发地（城市名或车站名）")
	to := flag.String("to", "", "目的地（城市名或车站名）")
	date := flag.String("date", time.Now().Format("2006-01-02"), "出行日期（离线参考）")
	departAfter := flag.String("time", "", "最早出发时刻 HH:MM（可选，默认全天）")
	seat := flag.String("seat", "auto", "席别：auto/hard_seat/hard_sleep_up/hard_sleep_mid/hard_sleep_low/soft_sleep_up/soft_sleep_low/second/first/business")
	dbPath := flag.String("db", filepath.Join("data", "rail.db"), "数据库路径")
	top := flag.Int("top", 20, "展示方案条数")
	verbose := flag.Bool("v", false, "输出搜索统计与直达基准")
	listSeats := flag.Bool("list-seats", false, "列出全部席别代号后退出")
	flag.Parse()

	if *listSeats {
		fmt.Println("可选席别：")
		for _, o := range service.SeatOptions() {
			fmt.Printf("  %-16s %s\n", o.Value, o.Label)
		}
		return
	}

	if *from == "" || *to == "" {
		fmt.Println("用法示例：railplan -from 汉中 -to 重庆 [-time 08:00] [-seat hard_sleep_mid] [-top 20]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Println("打开数据库失败：", err)
		fmt.Println("请先执行：raildata -src data/raw/extracted -db data/rail.db")
		os.Exit(1)
	}
	defer st.Close()

	req := service.Request{
		From: *from,
		To:   *to,
		Date: *date,
		Seat: model.Seat(*seat),
		Top:  *top,
	}
	if *departAfter != "" {
		req.StartMin = store.ClockToMin(*departAfter)
	}

	res, err := service.Search(st, req)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("════════════════════════════════════════════════════════════════════")
	fmt.Printf("  %s  →  %s      参考日期 %s\n", res.From, res.To, res.Date)
	fmt.Printf("  出发车站：%s\n", strings.Join(res.StartStations, " / "))
	fmt.Printf("  到达车站：%s\n", strings.Join(res.GoalStations, " / "))
	fmt.Printf("  席别：%s\n", model.SeatName(model.Seat(res.Seat)))
	if res.HasDirect {
		kind := "普速"
		if res.DirectFast {
			kind = "高铁/动车"
		}
		fmt.Printf("  直达基准（%s %s）：¥%.1f ／ %s\n",
			kind, res.DirectTrain, res.DirectPrice, minutesText(res.DirectMinutes))
	} else {
		fmt.Println("  直达基准：无直达车次（按综合成本排序）")
	}
	fmt.Println("════════════════════════════════════════════════════════════════════")

	if len(res.Routes) == 0 {
		fmt.Println("  未找到满足约束的方案。")
	}
	for i, r := range res.Routes {
		printRoute(i+1, r, res.HasDirect)
	}

	fmt.Printf("\n共 %d 条方案（上限 %d 条）｜ 计算耗时 %dms\n",
		res.TotalRoutes, config.MaxResultCount, res.ElapsedMs)
	if *verbose {
		s := res.Stats
		fmt.Printf("搜索统计：扩展节点 %d，生成分支 %d，命中方案 %d；剪枝 深度 %d / 时间 %d / 支配 %d / 价格 %d\n",
			s.Expanded, s.Generated, s.Collected,
			s.PruneDepth, s.PruneTime, s.PruneDominated, s.PrunePrice)
	}
	fmt.Printf("\n【免责声明】%s\n\n", res.Disclaimer)
}

func printRoute(idx int, r service.RouteView, hasDirect bool) {
	flagText := ""
	if r.IsCheap {
		flagText = "  ★高性价比低价中转"
	}
	tLabel := fmt.Sprintf("%d 次中转", r.Transfers)
	if r.Transfers == 0 {
		tLabel = "直达"
	}
	fmt.Printf("\n【方案 %d】%s%s\n", idx, tLabel, flagText)
	if hasDirect {
		if r.SavePercent >= 0 {
			fmt.Printf("   参考总价 ¥%.1f（较最低直达省 %.1f%%） ｜ 全程 %s ｜ 得分 %.1f\n",
				r.TotalPrice, r.SavePercent, minutesText(r.TotalMin), r.Score)
		} else {
			fmt.Printf("   参考总价 ¥%.1f（比最低直达贵 %.1f%%） ｜ 全程 %s ｜ 得分 %.1f\n",
				r.TotalPrice, -r.SavePercent, minutesText(r.TotalMin), r.Score)
		}
	} else {
		fmt.Printf("   参考总价 ¥%.1f ｜ 全程 %s ｜ 得分 %.1f\n",
			r.TotalPrice, minutesText(r.TotalMin), r.Score)
	}
	for i, leg := range r.Legs {
		mark := "├─"
		if i == len(r.Legs)-1 {
			mark = "└─"
		}
		kmText := ""
		if leg.KM > 0 {
			kmText = fmt.Sprintf("%dkm ", leg.KM)
		}
		fmt.Printf("   %s %d) %-7s %s  %s %s → %s %s   %s¥%.1f\n",
			mark, i+1, leg.TrainNo, leg.TrainType,
			leg.From, leg.Depart, leg.To, leg.Arrive,
			kmText, leg.Price)
		fmt.Printf("          %s  用时 %s", leg.SeatName, minutesText(leg.Duration))
		if len(leg.Fares) > 1 {
			var parts []string
			for _, f := range leg.Fares {
				parts = append(parts, fmt.Sprintf("%s ¥%.1f", f.Label, f.Price))
			}
			fmt.Printf(" ｜可选席别：%s", strings.Join(parts, "、"))
		}
		fmt.Println()
		if i < len(r.Legs)-1 {
			fmt.Printf("          ⇅ 换乘 %s  等待 %s\n",
				leg.To, minutesText(r.Legs[i+1].WaitMin))
		}
	}
}

func minutesText(m int) string {
	if m < 0 {
		m = 0
	}
	h, mm := m/60, m%60
	if h == 0 {
		return fmt.Sprintf("%d分", mm)
	}
	return fmt.Sprintf("%d小时%02d分", h, mm)
}
