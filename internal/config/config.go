// Package config 集中定义全部可调常量，方便按需修改。
package config

const (
	// MaxTransferCount 最大中转次数（≥4 次直接截断，即最多 4 段行程）
	MaxTransferCount = 3

	// MaxTimeMultiple 时间硬剪枝系数：存在直达方案时，总耗时 > 直达最短耗时 × 该系数 则剪枝
	MaxTimeMultiple = 2.0

	// PriceSaveThreshold 性价比标记阈值：比直达最低价便宜 ≥10% 标记为「高性价比低价中转」
	PriceSaveThreshold = 0.10

	// MinTransferGap 最小换乘间隔（分钟），低于此值视为赶不上车，方案作废
	MinTransferGap = 25

	// MaxResultCount 结果输出上限
	MaxResultCount = 200

	// MaxSegmentsPerStation 单站出发时，单个车次最多向下枚举的后续站数（防小站普速车 40+ 站导致膨胀）
	MaxSegmentsPerStation = 30

	// MaxBoardingWindowMin 同一次换乘最多向后搜索的时间窗（分钟），避免为极晚班次做无意义扩展
	MaxBoardingWindowMin = 20 * 60

	// MaxDurationNoDirect 无直达方案（或所选席别无直达）时的兜底全程耗时上限（分钟）。
	// 此时没有「直达 ×2」作为剪枝基准，用该上限防止搜索空间失控（普速卧铺方案可能很长）。
	MaxDurationNoDirect = 36 * 60
)

// ScoreWeights 打分权重（第六节公式）
const (
	ScoreSaveWeightPerPercent = 100.0 // save_ratio * 100
	ScoreExtraHourPenalty     = 15.0  // - extra_hour * 15
	ScoreNoDirectPriceWeight  = 0.01  // 无直达：-总票价 * 0.01
	ScoreNoDirectHourWeight   = 5.0   // 无直达：-(总耗时/3600) * 5
)

// Disclaimer 强制输出的免责声明
const Disclaimer = "本工具仅为路线规划参考，票价为国铁基准公布价；动车组存在市场化浮动折扣，临客/调图可能变动，真实票价、余票请前往12306官方核验"
