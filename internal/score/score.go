// Package score 实现性价比打分与排序。
package score

import (
	"sort"
	"strings"

	"railplanner/internal/config"
	"railplanner/internal/model"
	"railplanner/internal/store"
)

// Rank 计算性价比得分、过滤硬约束、排序并截断到 MAX_RESULT_COUNT。
func Rank(routes []model.Route, direct store.DirectInfo) []model.Route {
	out := make([]model.Route, 0, len(routes))
	for _, r := range routes {
		// 结果侧二次校验：中转方案的耗时不得超过直达 × MAX_TIME_MULTIPLE；
		// 直达方案本身即基准，全部保留（含耗时较长的普速低价车）。
		if direct.OK && direct.Minutes > 0 && r.Transfers() > 0 {
			limit := float64(direct.Minutes) * config.MaxTimeMultiple
			if float64(r.TotalMin) > limit {
				continue
			}
		}
		if direct.OK && direct.Price > 0 {
			r.SaveRatio = (direct.Price - r.TotalPrice) / direct.Price
			r.IsCheap = r.SaveRatio >= config.PriceSaveThreshold
			extraHour := float64(r.TotalMin-direct.Minutes) / 60.0
			r.Score = r.SaveRatio*config.ScoreSaveWeightPerPercent - extraHour*config.ScoreExtraHourPenalty
		} else {
			r.Score = -r.TotalPrice*config.ScoreNoDirectPriceWeight -
				(float64(r.TotalMin)/60.0)*config.ScoreNoDirectHourWeight
		}
		out = append(out, r)
	}
	// 得分降序；同分时总价低者优先，再比总耗时
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].TotalPrice != out[j].TotalPrice {
			return out[i].TotalPrice < out[j].TotalPrice
		}
		return out[i].TotalMin < out[j].TotalMin
	})

	// 多样性控制：同一「换乘站序列 + 末段车次」最多保留 maxSameSkeleton 条，
	// 避免同类方案（仅首段班次不同）占满结果上限，把其他可行组合挤出去。
	out = diversify(out, maxSameSkeleton)

	if len(out) > config.MaxResultCount {
		out = out[:config.MaxResultCount]
	}
	return out
}

const maxSameSkeleton = 3

func diversify(routes []model.Route, limitPerSkeleton int) []model.Route {
	count := make(map[string]int, len(routes))
	kept := routes[:0]
	for _, r := range routes {
		k := skeletonKey(r)
		if count[k] >= limitPerSkeleton {
			continue
		}
		count[k]++
		kept = append(kept, r)
	}
	return kept
}

// skeletonKey 换乘骨架：中转站序列 + 末段车次
func skeletonKey(r model.Route) string {
	var b strings.Builder
	for i, l := range r.Legs {
		b.WriteString(l.To)
		b.WriteByte('>')
		if i == len(r.Legs)-1 {
			b.WriteString(l.TrainNo)
		}
	}
	return b.String()
}
