-- ============================================================
-- 离线铁路中转路径规划工具 - 数据库结构
-- 单文件 SQLite，仅本地读取；路线计算阶段零网络请求
--   base_price     : 自该车次始发站起的「累计基准票价」，段票价 = 下车站累计价 - 上车站累计价
--   *_elapsed      : 自该车次始发站起的「累计运行分钟」，用于精确计算跨天区间耗时
--   full_code      : 12306 车次内部唯一编号（同一车次上下行变号时仍保持唯一）
--   时刻统一 'HH:MM' 定长文本，字符串比较等价于时间比较
-- ============================================================

PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;

-- 车站表：站名 / 电报码 / 归属城市（城市名用于「输入城市→多车站展开」）
CREATE TABLE IF NOT EXISTS station (
    station_name   TEXT PRIMARY KEY,
    telegraph_code TEXT,
    city_name      TEXT NOT NULL
);

-- 车次表
CREATE TABLE IF NOT EXISTS train (
    full_code     TEXT PRIMARY KEY,   -- 内部唯一编号
    train_no      TEXT NOT NULL,      -- 对外车次号（如 D1927）
    train_type    TEXT NOT NULL,      -- 高铁 / 动车 / 城际 / 普速
    grade         TEXT NOT NULL,      -- 原始等级字母 G/D/C/K/T/Z/P/S/Y
    start_station TEXT,
    end_station   TEXT,
    stop_count    INTEGER DEFAULT 0
);

-- 元信息（票价抓取时间、数据版本等）
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- 真实 OD 票价（**优先数据源**）
--   来自各车次「始发站 → 第 N 站」的实际可售票价（12306 直接可售，非推算值），
--   同一 OD 被多趟车覆盖时取中位数。查询段票价时优先查此表，
--   查不到时由查询端用「累计价差值」兜底（票价率因线路而异，全局曲线会有偏差，故不使用曲线）。
CREATE TABLE IF NOT EXISTS od_fare (
    from_station TEXT    NOT NULL,
    to_station   TEXT    NOT NULL,
    train_type   TEXT    NOT NULL,
    seat         TEXT    NOT NULL,
    km           INTEGER NOT NULL,
    price        REAL    NOT NULL,
    PRIMARY KEY (from_station, to_station, train_type, seat)
);

-- 经停时刻表（核心表）
--   km           : 自始发站累计里程（km），用于票价补算与展示
--   base_price   : 该站起最低价席别的累计基准票价（兼容/默认）
--   p_*          : 各席别「自始发站累计基准票价」，0 表示该车次无此席别
--                  段票价 = 下车站 p_x - 上车站 p_x
CREATE TABLE IF NOT EXISTS stop_schedule (
    full_code      TEXT    NOT NULL,
    stop_seq       INTEGER NOT NULL,   -- 站序，判定行车方向（只能 seq 小 → seq 大）
    train_no       TEXT    NOT NULL,   -- 冗余车次号，避免查询时 join
    train_type     TEXT    NOT NULL,
    station_name   TEXT    NOT NULL,
    arrive_time    TEXT    NOT NULL,   -- 'HH:MM'
    depart_time    TEXT    NOT NULL,   -- 'HH:MM'
    arrive_elapsed INTEGER NOT NULL,   -- 自始发站累计分钟（到达）
    depart_elapsed INTEGER NOT NULL,   -- 自始发站累计分钟（发车）
    km             INTEGER NOT NULL DEFAULT 0,
    base_price     REAL    NOT NULL DEFAULT 0,
    p_hard_seat      REAL NOT NULL DEFAULT 0,  -- 硬座
    p_hard_sleep_up  REAL NOT NULL DEFAULT 0,  -- 硬卧上铺
    p_hard_sleep_mid REAL NOT NULL DEFAULT 0,  -- 硬卧中铺
    p_hard_sleep_low REAL NOT NULL DEFAULT 0,  -- 硬卧下铺
    p_soft_sleep_up  REAL NOT NULL DEFAULT 0,  -- 软卧上铺
    p_soft_sleep_low REAL NOT NULL DEFAULT 0,  -- 软卧下铺
    p_soft_seat      REAL NOT NULL DEFAULT 0,  -- 软座
    p_no_seat        REAL NOT NULL DEFAULT 0,  -- 无座
    p_second         REAL NOT NULL DEFAULT 0,  -- 二等座
    p_first          REAL NOT NULL DEFAULT 0,  -- 一等座
    p_business       REAL NOT NULL DEFAULT 0,  -- 商务座
    PRIMARY KEY (full_code, stop_seq)
);

-- 【性能核心】联合索引：某站某时刻之后出发的车次，毫秒级命中，避免全表扫描
CREATE INDEX IF NOT EXISTS idx_stop_station_depart ON stop_schedule(station_name, depart_time);
CREATE INDEX IF NOT EXISTS idx_stop_train_seq      ON stop_schedule(full_code, stop_seq);
-- 城市 → 多车站展开
CREATE INDEX IF NOT EXISTS idx_station_city        ON station(city_name);
