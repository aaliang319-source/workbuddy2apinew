// random.go 排程抖动源：把「第 N 小时触发」变成「第 N 小时窗口内某个随机时刻触发」。
//
// 抖动是**每次排定时现滚的**，不是按日期哈希出来的固定偏移——按 (kind, 日, 小时)
// 哈希出来的偏移虽然也"不是整点"，但每天同一时刻执行，只是把指纹从 :00 挪到了
// :33，仍然是一条可被关联的规律。真正的随机要求每次排定都重新掷。
package scheduler

import (
	"math/rand/v2"
	"sync"
	"time"
)

const (
	// jitterSpanDefault 默认抖动跨度（分钟）：在所配小时的第 0-55 分钟内随机取一刻。
	jitterSpanDefault = 55
	// jitterSpanMax 抖动跨度上限。
	//
	// 为什么封在 55 而不是 60：span=60 时落点可逼近下一个整点（09:00+60min=10:00），
	// 等于跳出了用户配置的小时窗口；对夜猫任务还会顶到 08:00:00，而脚本侧
	// within_night_window() 判定的是 hour < 8，08:00 整属于**窗外**，会被 skip 白跑。
	jitterSpanMax = 55
)

// Randomizer 抖动源 + 各任务最近一次排定的落点。
//
// 落点记忆的用途：定时循环负责「排定」下次触发（真正掷随机数），面板只读这里的值。
// 若面板也去重算，会掷出另一个随机值——显示的时刻与真实执行不符，那正是旧实现
// 必须把抖动做成确定性哈希的原因；把随机权威收在循环侧，两者才能既随机又一致。
type Randomizer struct {
	mu   sync.Mutex
	rnd  *rand.Rand
	span time.Duration
	last map[string]time.Time

	// fired kind → 已真实执行过的小时槽（截断到整点）。执行后重排时同槽作废、
	// 直接掷次日：否则小时窗口内的剩余时间仍在"将来"，nextFire 会重掷出同槽
	// 的另一个落点，导致同一小时窗口重复执行 2~3 次（抖动跨度 55 分钟时尤甚）。
	fired map[string]time.Time
}

// NewRandomizer 构建抖动源；spanMinutes 是抖动跨度（分钟），**0 = 准点执行**。
//
// 「未配置 → 默认 55」的兜底不在这里，而在 ScheduleSpec.JitterSpan()——默认值只在
// 那一处解析。曾经本函数也兜底 0 → 55，结果 jitter_disabled=true 构造出来的实例
// 跨度是 55（要等第一次排定经 setSpanFromSpec 才被纠正），准点配置静默失效。
func NewRandomizer(spanMinutes int) *Randomizer {
	return &Randomizer{
		// PCG 两路种子都取自全局随机源：进程间、重启前后互不相同。
		rnd:  rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
		span: clampSpanMinutes(spanMinutes),
		last: map[string]time.Time{},
		fired: map[string]time.Time{},
	}
}

// ResetForSpec 按新排程规格重置跨度，并清空已排定的落点。
//
// 排程热更新后旧落点一律作废：用户可能改了小时，也可能改了跨度，两种情况下旧值都
// 不再代表"下次执行"。宁可让面板短暂显示「待排定」（循环被唤醒后立刻重掷补上），
// 也不要拿一个按老排程定下的时刻冒充。
func (r *Randomizer) ResetForSpec(spec ScheduleSpec) {
	r.mu.Lock()
	r.span = time.Duration(spec.JitterSpan()) * time.Minute
	r.last = map[string]time.Time{}
	r.fired = map[string]time.Time{}
	r.mu.Unlock()
}

// SyncSpan 按排程规格同步跨度，**保留已记下的落点**。
//
// 用于启动装配：automation 自建实例时（生产路径传的是 scheduler 的实例）先用它对齐
// 跨度，不必把落点清一遍——那时还没有落点，清不清无所谓，但语义上"只是对齐配置"
// 不该有副作用。热更新请用 ResetForSpec（那个必须清）。
func (r *Randomizer) SyncSpan(spec ScheduleSpec) {
	r.setSpanFromSpec(spec)
}

// setSpanFromSpec 按排程规格同步跨度（每次排定前也会调，兜住任何绕过 ResetForSpec
// 直接替换 spec 的路径）。
//
// 跨度未变时只改不动别的——**尤其不清空落点**：循环每次排定都会调到这里，
// 若每次清空，面板就永远读不到值（或读到刚被清的）。
func (r *Randomizer) setSpanFromSpec(spec ScheduleSpec) {
	want := time.Duration(spec.JitterSpan()) * time.Minute
	r.mu.Lock()
	if r.span != want {
		r.span = want
		r.last = map[string]time.Time{}
		r.fired = map[string]time.Time{}
	}
	r.mu.Unlock()
}

// SpanMinutes 当前抖动跨度（分钟，0 = 准点），供面板展示与测试断言。
func (r *Randomizer) SpanMinutes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int(r.span / time.Minute)
}

// clampSpanMinutes 把分钟数夹到 [0, jitterSpanMax]；「未配置」的默认值不在这里解析
// （见 NewRandomizer 的说明）。
func clampSpanMinutes(m int) time.Duration {
	if m < 0 {
		m = 0
	}
	if m > jitterSpanMax {
		m = jitterSpanMax
	}
	return time.Duration(m) * time.Minute
}

// roll 现掷一个偏移，落在 [1s, span]。
//
// 下界取 1s 而非 0：**整点本身永不被选中**。:00:00 是所有自动化脚本的共同指纹，
// 抖动若允许落回整点就等于没改。span<=0（抖动关闭）返回 0，即准点执行。
//
// rnd 为 nil 时按准点处理：**零值 Randomizer 必须可用**——NextWake 在 r==nil 时会
// 就地构造 &Randomizer{} 走"只关心哪个小时"的无抖动路径。
func (r *Randomizer) roll() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rnd == nil {
		return 0
	}
	return rollFn(r.rnd, r.span)
}

// rollFn 偏移派生函数。默认就是掷随机数；仅测试替换为固定值，用来构造
// "两个任务恰好落到同一时刻" 这类确定性场景（包内既有 var-swap 测试惯例，
// 见 school.go 的 newScriptCmd）。生产路径永不改写。
var rollFn = rollLocked

// rollLocked 见 roll；调用方需持有 r.mu（remember 与 roll 不嵌套加锁）。
func rollLocked(rnd *rand.Rand, span time.Duration) time.Duration {
	secs := int64(span / time.Second)
	if secs <= 0 {
		return 0
	}
	return time.Duration(1+rnd.Int64N(secs)) * time.Second
}

// jitteredAt 算出 day 当天 hour 点经随机抖动后的时刻：起点 hour:00:00，偏移现掷。
// 抖动关闭（跨度 0）时偏移为 0，即准点执行。
//
// 落点若跨过 hour 的边界则一律**夹**回本小时最后一秒（hour:59:59），绝不外溢：
// 跨小时会打乱"配了几个小时就是几个小时"的语义。只有一种配置会触发夹取——
// 抖动跨度超过 60 分钟且小时点靠后（如 23 点 + 90 分钟要进位到次日），而 nextFire
// 判断"是否已过"时会把次日误当今日。封顶 55 分钟后该组合不可达，这里只是兜底，
// 保证即使有人放宽上限也不会把任务排到配置之外的时刻上。
func (r *Randomizer) jitteredAt(day time.Time, hour int) time.Time {
	base := time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, day.Location())
	at := base.Add(r.roll())
	if limit := base.Add(time.Hour - time.Second); at.After(limit) {
		return limit
	}
	return at
}

// remember 记下某任务最近一次排定的触发时刻。map 为 nil（零值 Randomizer）时跳过：
// 那种实例只用于"无抖动、只关心哪个小时"的一次性计算，没有面板要读的落点。
func (r *Randomizer) remember(kind string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		return
	}
	r.last[kind] = at
}

// MarkFired 记下某任务类型真实执行完成的小时槽（取执行时刻所在整点）。
// 之后 nextFireKind 对同槽不再参与排定（去重：一个窗口只执行一次）。
// 零值 Randomizer（fired 为 nil）直接跳过——与 remember 同口径。
func (r *Randomizer) MarkFired(kind string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fired == nil {
		return
	}
	r.fired[kind] = time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), 0, 0, 0, at.Location())
}

// firedSlot 判定 kind 在 now 所处时刻之前、是否已执行过 hour 这个小时槽。
// 只比对"执行日 == 今日且槽 hour 与执行槽相同"，跨日的旧记录自然失效
// （每日循环重新起算）。调用方需持有 r.mu 不必要——本函数自取锁。
func (r *Randomizer) firedSlot(kind string, now time.Time, hour int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.fired[kind]
	if !ok {
		return false
	}
	return at.Hour() == hour && sameLocalDay(at, now)
}

// sameLocalDay 判定两个时刻是否同一本地自然日。
func sameLocalDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// Scheduled 返回某任务最近排定的触发时刻。
//
// ok=false 表示循环还没排定过（启动瞬间、该任务已禁用、或刚热更新过）——面板据此
// 显示「待排定」，而不是拿一个过期值或另掷一个随机值冒充下次执行。
func (r *Randomizer) Scheduled(kind string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.last[kind]
	return at, ok
}
