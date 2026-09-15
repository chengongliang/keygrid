-- ============================================================
-- seed_usage.sql —— 用量看板测试数据
--
-- 用途：本地/演示环境给 usage_logs 灌过去 30 天的测试数据，
--       让用量页（/usage）与管理看板（/admin/usage）的图表有数据可看。
--
-- 数据形态：
--   - 用户：现有首个真实用户（量最大）+ 2 个 seed 用户（alice/bob，撑起 TOP 排行）
--   - 模型：5 个（权重不同），甜甜圈"Top7+其他"有层次
--   - 时间：上海时区过去 30 天；9-19 点为高峰、凌晨低峰；今天只落在已过去的小时
--   - 走势：请求数缓慢上升，周末约为工作日一半（热力图有形状、环比有涨跌）
--   - 失败：约 3.5% 请求返回 429/500/502（看板 failed_requests / 成功率列有值）
--   - 费用：按 model_prices 当前价算价格快照（与转发计费同公式），未配置的模型用
--     脚本内置兜底价；失败请求 cost=0；本批费用同步累加进 api_keys.quota_used
--
-- 用法：
--   docker exec -i keygrid-postgres-1 psql -U keygrid -d keygrid -v ON_ERROR_STOP=1 < scripts/seed_usage.sql
--
-- 注意：
--   - 重复执行会叠加数据（真实用户的 usage_logs 无标记字段，脚本不做去重删除）
--   - 清理方式见文件末尾注释
-- ============================================================

BEGIN;

-- ---- 1. seed 用户（幂等）----
INSERT INTO users (email, name, role, status, created_at)
VALUES
  ('seed-alice@demo.local', 'Alice (seed)', 'user', 'active', now() - interval '35 days'),
  ('seed-bob@demo.local',   'Bob (seed)',   'user', 'active', now() - interval '33 days')
ON CONFLICT (email) DO NOTHING;

-- ---- 2. seed 用户渠道 + API key（幂等；key_hash 为占位值，仅造数用，不可用于真实鉴权）----
INSERT INTO providers (user_id, name, kind, protocol, base_url, model_map, priority, enabled, created_at)
SELECT u.id, 'Seed Channel', 'api_key', 'openai', 'https://seed.invalid/v1',
       '{"kimi-k2.5":"kimi-k2.5","deepseek-v4":"deepseek-v4","glm-5.3-flash":"glm-5.3-flash"}'::jsonb,
       0, true, now() - interval '32 days'
FROM users u
WHERE u.email IN ('seed-alice@demo.local', 'seed-bob@demo.local')
  AND NOT EXISTS (SELECT 1 FROM providers p WHERE p.user_id = u.id AND p.name = 'Seed Channel');

INSERT INTO api_keys (user_id, name, key_hash, prefix, quota_tokens, enabled, created_at)
SELECT u.id, 'seed-key',
       encode(sha256(('seed-' || u.email)::bytea), 'hex'), 'sk-seed0000',
       -1, true, now() - interval '32 days'
FROM users u
WHERE u.email IN ('seed-alice@demo.local', 'seed-bob@demo.local')
  AND NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.user_id = u.id AND k.name = 'seed-key');

-- ---- 3. 生成 30 天用量（含费用 + quota_used 累加）----
WITH anchor AS (
  -- 以上海时区“今天”为锚，保证按 Asia/Shanghai 聚合时日期边界正确
  SELECT (now() AT TIME ZONE 'Asia/Shanghai')::date AS today,
         EXTRACT(HOUR FROM now() AT TIME ZONE 'Asia/Shanghai')::int AS now_h
),
people AS (
  -- (uid, kid, pid, 每日基础请求数, 每日增长系数)
  -- 真实用户：首个非 seed 用户；seed 用户量级递减
  (
    SELECT u.id AS uid,
           (SELECT k.id FROM api_keys k WHERE k.user_id = u.id ORDER BY k.id LIMIT 1) AS kid,
           (SELECT pr.id FROM providers pr WHERE pr.user_id = u.id ORDER BY pr.id LIMIT 1) AS pid,
           30::numeric AS base_n, 0.7::numeric AS grow
    FROM users u
    WHERE u.email NOT LIKE 'seed-%@demo.local'
    ORDER BY u.id
    LIMIT 1
  )
  UNION ALL
  SELECT u.id,
         (SELECT k.id FROM api_keys k WHERE k.user_id = u.id ORDER BY k.id LIMIT 1),
         (SELECT pr.id FROM providers pr WHERE pr.user_id = u.id ORDER BY pr.id LIMIT 1),
         v.base_n, v.grow
  FROM users u
  JOIN (VALUES ('seed-alice@demo.local', 16::numeric, 0.35::numeric),
               ('seed-bob@demo.local',    9::numeric, 0.20::numeric)
       ) v(email, base_n, grow) ON v.email = u.email
),
plan AS (
  -- 每人每天一行：请求数 n（波动 + 上升趋势，周末减半）+ 当天上海 0 点的绝对时刻
  SELECT
    p.uid, p.kid, p.pid,
    g.d AS d,
    ((a.today - g.d)::timestamp AT TIME ZONE 'Asia/Shanghai') AS day_start,
    GREATEST(1, ROUND(
      (p.base_n * (0.7 + 0.6 * random()) + p.grow * (29 - g.d))
      * CASE WHEN EXTRACT(DOW FROM (a.today - g.d)) IN (0, 6) THEN 0.5 ELSE 1.0 END
    ))::int AS n
  FROM anchor a
  CROSS JOIN generate_series(0, 29) AS g(d)
  CROSS JOIN people p
),
logs AS (
  -- 展开为逐请求行；小时：72% 落在上海时间 9-19，其余全天。
  -- r：单次 random() 供模型分层与失败标记共用（嵌套分布）；必须经 CROSS JOIN
  -- LATERAL 每行独立求值——若写成普通子查询会被 PG 折叠成常量，随机性全部失效。
  SELECT
    pl.uid, pl.kid, pl.pid, pl.d, pl.day_start,
    a.now_h,
    (CASE WHEN random() < 0.72 THEN 9 + floor(random() * 11) ELSE floor(random() * 24) END)::int AS sh_h,
    floor(random() * 60)::int AS mi,
    r.r,
    CASE
      WHEN r.r < 0.28 THEN 'glm-5.3-flash'
      WHEN r.r < 0.52 THEN 'qwen3.8-flash'
      WHEN r.r < 0.72 THEN 'qwen3.8-27b'
      WHEN r.r < 0.87 THEN 'deepseek-v4'
      ELSE 'kimi-k2.5'
    END AS model
  FROM plan pl
  JOIN anchor a ON true
  CROSS JOIN LATERAL generate_series(1, pl.n)
  CROSS JOIN LATERAL (SELECT random() AS r) r
),
raw AS (
  -- token/延迟/输出占比按模型分层；单价取 model_prices 当前价（价格快照语义，
  -- 与转发计费一致），价格表未配置的模型回退到内置兜底价，保证演示数据有费用形状。
  SELECT
    l.uid, l.kid, l.pid, l.day_start,
    CASE WHEN l.d = 0 THEN LEAST(l.sh_h, l.now_h) ELSE l.sh_h END AS sh_h,
    l.mi, l.model, l.r,
    COALESCE(mp.prompt_price, CASE
      WHEN l.model = 'glm-5.3-flash' THEN 0.075
      WHEN l.model = 'qwen3.8-flash' THEN 0.15
      WHEN l.model = 'qwen3.8-27b'  THEN 0.42
      WHEN l.model = 'deepseek-v4'  THEN 0.28
      ELSE 0.45                              -- kimi-k2.5
    END) AS pp,
    COALESCE(mp.completion_price, CASE
      WHEN l.model = 'glm-5.3-flash' THEN 0.25
      WHEN l.model = 'qwen3.8-flash' THEN 0.47
      WHEN l.model = 'qwen3.8-27b'  THEN 3.0
      WHEN l.model = 'deepseek-v4'  THEN 0.42
      ELSE 2.25                              -- kimi-k2.5
    END) AS cp,
    (CASE
      WHEN l.r < 0.28 THEN 300 + floor(random() * 2200)    -- glm-5.3-flash
      WHEN l.r < 0.52 THEN 300 + floor(random() * 1700)    -- qwen3.8-flash
      WHEN l.r < 0.72 THEN 800 + floor(random() * 5200)    -- qwen3.8-27b
      WHEN l.r < 0.87 THEN 600 + floor(random() * 4400)    -- deepseek-v4
      ELSE 1000 + floor(random() * 7000)                   -- kimi-k2.5
    END)::int AS pt,
    (CASE
      WHEN l.r < 0.28 THEN 260 + floor(random() * 640)
      WHEN l.r < 0.52 THEN 300 + floor(random() * 600)
      WHEN l.r < 0.72 THEN 1600 + floor(random() * 2600)
      WHEN l.r < 0.87 THEN 1100 + floor(random() * 2400)
      ELSE 2000 + floor(random() * 4000)
    END)::int AS lat,
    (CASE
      WHEN l.r < 0.28 THEN 0.2 + random() * 0.4
      WHEN l.r < 0.52 THEN 0.2 + random() * 0.4
      WHEN l.r < 0.72 THEN 0.3 + random() * 0.7
      WHEN l.r < 0.87 THEN 0.3 + random() * 0.7
      ELSE 0.4 + random() * 0.8
    END) AS out_ratio
  FROM logs l
  LEFT JOIN model_prices mp ON mp.model = l.model
),
base AS (
  -- 输出 tokens + 费用：失败请求 tokens=0 → cost=0（与 relay 记账语义一致）；
  -- 成功 cost = prompt/1e6*pp + completion/1e6*cp（USD，价格快照随记录落库）。
  SELECT
    uid, kid, pid, day_start, sh_h, mi, model,
    (r > 0.965) AS fail,   -- 约 3.5% 失败（r 复用，与模型分层独立）
    pt, lat,
    GREATEST(1, (pt * out_ratio)::int) AS ct,
    pt/1e6 * pp + GREATEST(1, (pt * out_ratio)::int)/1e6 * cp AS cost
  FROM raw
),
ins AS (
  INSERT INTO usage_logs (user_id, api_key_id, provider_id, model, prompt_tokens,
                          completion_tokens, status_code, latency_ms, cost, created_at)
  SELECT
    b.uid, b.kid, b.pid, b.model,
    b.pt,
    CASE WHEN b.fail THEN 0 ELSE b.ct END,
    CASE WHEN b.fail THEN (ARRAY[429, 500, 502])[1 + floor(random() * 3)::int] ELSE 200 END,
    CASE WHEN b.fail THEN 100 + floor(random() * 2900)::int ELSE b.lat END,
    CASE WHEN b.fail THEN 0 ELSE b.cost END,
    b.day_start + b.sh_h * interval '1 hour' + b.mi * interval '1 min'
  FROM base b
  RETURNING api_key_id, cost
)
-- 本批费用同步累加进 api_keys.quota_used（DB 权威值，与 UsageWriter flush 维护方式一致）
UPDATE api_keys k
SET quota_used = k.quota_used + d.delta
FROM (SELECT api_key_id, sum(cost) AS delta FROM ins GROUP BY api_key_id HAVING sum(cost) > 0) d
WHERE k.id = d.api_key_id;

-- ---- 4. 概览输出 ----
SELECT
  count(*)                                    AS total_rows,
  count(DISTINCT user_id)                     AS users,
  count(*) FILTER (WHERE status_code >= 400)  AS failed_rows,
  round(sum(cost)::numeric, 4)                AS total_cost,
  min(created_at AT TIME ZONE 'Asia/Shanghai')::date AS from_day,
  max(created_at AT TIME ZONE 'Asia/Shanghai')::date AS to_day
FROM usage_logs;

SELECT COALESCE(u.email, '?') AS email, count(*) AS rows,
       sum(l.prompt_tokens + l.completion_tokens) AS tokens,
       round(sum(l.cost)::numeric, 4) AS cost
FROM usage_logs l JOIN users u ON u.id = l.user_id
GROUP BY u.email ORDER BY rows DESC;

COMMIT;

-- ============================================================
-- 清理本脚本插入的数据：
--
--   -- 只清 seed 用户的：
--   DELETE FROM usage_logs      WHERE user_id IN (SELECT id FROM users WHERE email LIKE 'seed-%@demo.local');
--   DELETE FROM api_keys        WHERE user_id IN (SELECT id FROM users WHERE email LIKE 'seed-%@demo.local');
--   DELETE FROM providers       WHERE user_id IN (SELECT id FROM users WHERE email LIKE 'seed-%@demo.local');
--   DELETE FROM users           WHERE email LIKE 'seed-%@demo.local';
--
--   -- 真实用户的测试用量无标记字段，如需一并清空：
--   TRUNCATE usage_logs;
-- ============================================================
