package gameserver

import (
	"context"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 档案、技能树与推送树构造。

// 通行证使用的固定时间常量。
const (
	pass745F2                       = uint64(131765184000000000)
	f32Const801F2                   = uint32(1092571843) // 801.f2 位型
	castDepthBits                   = uint32(1091032928) // 158.f2 = f32 8.4900818
	profileFlags                    = uint32(17 | 0x100) // 0x100：跳过绑定邮箱提示
	defaultPlayerConditionPrimary   = store.DefaultPlayerConditionPrimary
	defaultPlayerConditionSecondary = store.DefaultPlayerConditionSecondary
)

// seasonBounds 返回当前赛季的起止 FILETIME。
// 以每月 22 日 00:00 UTC 为起点滚动到下月 22 日 09:00 UTC，保证时段覆盖当前时刻；
// 5013 时段过期会让客户端把资格节点判为锁定。
func seasonBounds() (uint64, uint64) {
	n := time.Now().UTC()
	year, month, day := n.Date()
	start := time.Date(year, month, 22, 0, 0, 0, 0, time.UTC)
	if day < 22 {
		start = start.AddDate(0, -1, 0)
	}
	end := start.AddDate(0, 1, 0).Add(9 * time.Hour)
	return uint64(proto.UnixToFiletime(start.Unix())), uint64(proto.UnixToFiletime(end.Unix()))
}

// BuildPose308 玩家姿态块（2/2 与 307.f12 共用；f7=世界时钟 FILETIME）。
func BuildPose308() *proto.Value {
	return wiregen.MustMakeObj("pnbphpdmppe", map[int]*proto.Value{
		0: proto.F32(0.85), 1: proto.F32(0.9946), 2: proto.F32(0.0),
		3: proto.F32(1.0), 4: proto.F32(0.0), 5: proto.F32(11.375), 6: proto.F32(0.0),
		7: proto.U64(uint64(proto.WindowsFiletimeNow())),
		8: proto.U32(0),
	})
}

// buildPose308 使用 SQLite 中的玩家状态构造姿态块；读库失败才回退到协议默认值。
// 非 2/2 档案响应只读取当前持久化值，不做在线钟推进；环境感知与推进统一由
// 2/2 保活负责，避免任意 307 以空环境抢走舒适度漂移区间。
// f3=舒适度、f4=幸运惩罚由 player_condition 透传。
func (s *Server) buildPose308(ctx context.Context, uid int64) *proto.Value {
	c := s.peekPlayerCondition(ctx, uid)
	return wiregen.MustMakeObj("pnbphpdmppe", map[int]*proto.Value{
		0: proto.F32(float32(c.Primary)), 1: proto.F32(float32(c.Secondary)), 2: proto.F32(0.0),
		3: proto.F32(float32(c.Comfort)), 4: proto.F32(float32(c.LuckPenalty)),
		5: proto.F32(11.375), 6: proto.F32(0.0),
		7: proto.U64(uint64(proto.WindowsFiletimeNow())),
		8: proto.U32(0),
	})
}

// build315 构造 322.f4 使用的统计子块。
func build315() *proto.Value {
	return wiregen.MustMakeObj("ifoifbocoom", map[int]*proto.Value{
		0: proto.F32(0.0),
		1: proto.U32(4),
		2: proto.StrVal(""),
		3: proto.U32(2),
		4: proto.StrVal(""),
	})
}

// BuildStats322 构造档案内的状态块。
// f0 不是最大渔获重量，而是客户端世界时钟的游戏日内分钟；客户端在
// 接收 307 后以它校准 24 倍速时钟。把鱼重写进这里会导致篝火等所有
// 依赖世界时钟的功能把鱼重误当成时间。
func BuildStats322(_ context.Context, _ *store.DB, _ int64) *proto.Value {
	return wiregen.MustMakeObj("kkmhfbeldpg", map[int]*proto.Value{
		0: proto.U32(gameMinuteOfDayAt(time.Now())),
		4: build315(),
	})
}

// buildStats322W 同上，但 f0 下发连续游戏分钟（不 mod 1440）：客户端世界
// 时钟基址 = f13.f0，之后 ×24 流逝。钓鱼游锚点（f23.oieepfcgjgn）用同一
// 连续坐标（见 f23 注释），基址回绕会让跨午夜的锚点与 W 错位 1440 分钟。
// 昼夜时刻 = W mod 1440，连续值 mod 回后与原日内分钟一致。
func buildStats322W(wNow int64) *proto.Value {
	return wiregen.MustMakeObj("kkmhfbeldpg", map[int]*proto.Value{
		0: proto.U32(uint32(wNow)),
		4: build315(),
	})
}

// build745 构造通行证状态块。
func build745() *proto.Value {
	return wiregen.MustMakeObj("dnijhkbdfan", map[int]*proto.Value{
		0: proto.U32(0),
		1: proto.U32(0),
		2: proto.U64(pass745F2),
	})
}

// build5013 构造一条赛季起止时段。
// Idx0 是赛季阶段（0=无/哨兵，5=新年活动，6=生日/周年活动）；客户端
// kkaadaemdkb.bbhdipfopfk 按 Idx0 索引 5013 条目，所以 5012.f0 可同时下发多阶段。
// 阶段 0 的哨兵条目恒发，供永久资格节点使用；发 6 会点亮主菜单「周年纪念任务」入口。
//
// Idx4 是周年面板倒计时的结束时间（iibalkfkhml.nehplcfdejp 从 dto+0x28 读
// FILETIME 后减当前时间），由后台 event_anniversary_end_unix 配置；
// 填 0 会让客户端回退到内置 2022-08-06，显示 -1555 天。
// Idx3 是 OPT（1 字节 flag，PackU8），放不下 FILETIME，交由 wiregen 按 ClassOps 填零。
func build5013(stage uint32, end uint64) *proto.Value {
	start, seasonEnd := seasonBounds()
	return wiregen.MustMakeObj("nkakjeigjla", map[int]*proto.Value{
		0: proto.U32(stage),
		1: proto.U64(start),
		2: proto.U64(seasonEnd),
		4: proto.U64(end),
		5: proto.Null(),
	})
}

// build5012 构造包含各阶段 5013 条目的赛季容器。
func build5012(stages []uint32, end uint64) *proto.Value {
	items := make([]*proto.Value, 0, len(stages))
	for _, stage := range stages {
		items = append(items, build5013(stage, end))
	}
	return wiregen.MustMakeObj("fkcmcjlagdh", map[int]*proto.Value{
		0: proto.Args(proto.S("210"), items...),
	})
}

// BuildProfile307 全量本人档案（801.f7、807 等会话内嵌 profile 也共用）。
// wNow 是构档案时刻的客户端会话游戏分钟（sessionGameMin），用于校准钓鱼游
// 启表基准；登录时刻的调用传 0。 See BuildProfile307W。
func (s *Server) BuildProfile307(ctx context.Context, uid int64) (*proto.Value, error) {
	return s.BuildProfile307W(ctx, uid, 0)
}

// BuildProfile307W 同上，wNow 为会话游戏时钟（登录时为 0，会话中递增）。
// 会话内下发（1/14 响应、2/1 推送、3/38 响应等）必须带当前 W，否则客户端
// 会把已启表钓鱼游的剩余算成负数（立刻显示"已过期"）。
func (s *Server) BuildProfile307W(ctx context.Context, uid int64, wNow int64) (*proto.Value, error) {
	p, err := s.DB.EnsureProfile(ctx, uid)
	if err != nil {
		return nil, err
	}
	places, _ := s.DB.PlaceAccessList(ctx, uid)
	placeItems := make([]*proto.Value, 0, len(places))
	for _, pl := range places {
		placeItems = append(placeItems, proto.BytesOf([]byte(pl)))
	}
	// f20 携带已拥有房屋（实测 2026-11-05：客户端据此把已购房屋显示为
	// "进入"；f22 房屋条目无效）。编码与 f22 一致，为地点/物品键的 hex。
	houses, _ := s.DB.HouseAccessList(ctx, uid)
	houseItems := make([]*proto.Value, 0, len(houses))
	for _, h := range houses {
		houseItems = append(houseItems, proto.BytesOf([]byte(h)))
	}

	membership := store.MembershipAt(p.Membership, p.MembershipExp, time.Now().Unix())
	var membershipKind uint32
	var membershipExp uint64
	if membership.Active {
		membershipKind = 1
		if membership.Permanent {
			membershipKind = 2
		} else {
			membershipExp = uint64(proto.UnixToFiletime(membership.ExpiresAt))
		}
	}
	totalPts := skillPointsTotal(p.Level)

	// 322 里的等级经验拆分
	base := baseXp(p.Level)
	var inLevel uint64
	if p.XP > base {
		inLevel = uint64(p.XP - base)
	}
	// 5012 赛季块：阶段与结束时间都取自运行参数。
	season := s.buildSeason5012()

	// f23 钓鱼游（wire 类 hajlpdpmnfe/1506）：客户端世界地图按 dhaenhabecc
	// 匹配地图键（hncnmlgnhjm.hlcjincbjma），据此渲染"✈还剩下N天"徽章、
	// 免入场费划线价、等级锁绕过（反编译 Assembly-CSharp 证实）。
	//
	// 剩余公式（il2csharp gcepbaokfie.pioghgjeelp 实码 + 2026-11-05 rf4_hook
	// decode=4 注入实测，客户端解析的 knp/start/dur/bought 与服务端逐字段吻合）：
	//   剩余 = ecdgbohiooi(f2) + min(oieepfcgjgn(f4), W+3) − W − 3
	// knpoijanngk(f3)=false 时直接返回全额 f2；W 是客户端世界时钟（2/2
	// keepalive FILETIME 每 5 秒校准，世界纪元总分钟坐标，与 worldTotalGameMin
	// 同源）。f4 若发"到期锚点 W+剩余"则 min 恒饱和于 W+3，剩余恒=f2/0 值错位
	// ——首轮实测徽章"48分钟"恒定不变即此（hook dump start=142192696=W购+48）。
	// 因此下发编码取：**f2=剩余游戏分钟、f4=W+3**，代回公式恰输出真实剩余，
	// 推送间隔内随 W 自然递减；叠加购票（剩余>单次时长）同样精确。
	// 到期条目不再下发：客户端徽章按条目存在性渲染，保留会让图标在到期
	// 后残留（2026-11-05 实测）；限购由服务端 3/38 拒绝并回业务失败文案。
	tripStates, _ := s.DB.TripStates(ctx, uid)
	vouchers := make([]*proto.Value, 0, len(tripStates))
	for _, st := range tripStates {
		if st.Purchases <= 0 || st.RemainingSec <= 0 {
			continue
		}
		remainGameMin := int64(st.RemainingSec * 24 / 60)
		if remainGameMin < 0 {
			remainGameMin = 0
		}
		var started uint32
		var start int64
		if st.LastConsumeTS > 0 {
			started = 1
			start = wNow + 3
		}
		vouchers = append(vouchers, wiregen.MustMakeObj("hajlpdpmnfe", map[int]*proto.Value{
			0: proto.StrVal(st.Place),
			1: proto.U32(uint32(st.Purchases)),
			2: proto.U32(uint32(remainGameMin)),
			3: proto.U32(started),
			4: proto.U32(uint32(start)),
		}))
	}

	// f16=kgpgemhhiik 是资料页奖杯鱼种数（trophy/trph），不是布尔值。客户端
	// 把 f16 拷进 ifbfdldbana.gebeadddmlp；RUIUserInfoWidget（等级旁星号）
	// 和 ddkpcmhdfdb.fgnijlofipp 都显示该值，24/19 的 f5 是同一字段。官服
	// 语义按鱼种去重：钓到过 grade>=3（重量稀有/超稀有）的鱼种各计一次，
	// 不等于统计页 3/2 按条数的稀有渔获计数。
	rareCatches, rareErr := s.DB.RareSpeciesCount(ctx, uid, 3)
	if rareErr != nil {
		s.log.Warn("奖杯鱼种数读取失败，按 0 下发", "uid", uid, "err", rareErr)
	}

	return wiregen.MakeObj("ghfbofgdflm", map[int]*proto.Value{
		0:  proto.U32(uint32(p.UID)),
		1:  proto.StrVal(p.Name),
		2:  proto.U32(uint32(p.Level)),
		3:  proto.U64(uint64(p.XP)),
		4:  proto.U64(inLevel),
		5:  proto.U64(uint64(nextLevelReq(p.Level))),
		6:  proto.U32(uint32(totalPts)),
		7:  proto.U64(uint64(p.Silver)),
		8:  proto.U64(uint64(p.Gold)),
		9:  proto.StrVal(""),
		10: proto.U32(membershipKind),
		11: proto.U64(membershipExp),
		12: s.buildPose308(ctx, uid),
		13: buildStats322W(wNow),
		14: proto.U32(0),
		15: proto.U32(profileFlags),
		16: proto.U32(uint32(rareCatches)),
		17: proto.StrVal(p.Email),
		18: proto.U32(uint32(p.Role)),
		19: proto.U64(uint64(p.Bank)),
		20: proto.ArrayOf(hexOf(houseItems)),
		21: proto.Str(nil),
		22: proto.ArrayOf(hexOf(placeItems)), // 已买断地点（f22 只承载买断，钓鱼游不进 f22）
		// 容器 code=181（by_type_id 实抓推导值，实机验证可用）：服务端始终
		// 发 181，购票→进图→抛竿→到期回收全链路回归通过——抛竿受
		// FishingSceneVoucherWathDog 门控，f23 解析作废会直接禁抛竿/黑屏，
		// 因此 181 可用是活体证据；rf4_hook decode=4 注入实测客户端解析的
		// knp/start/dur/bought 与服务端逐字段吻合。f4c375b 曾按反编译
		// "读取器注册表 1506 注册于 124"在注释里断言应改 124，但代码从未
		// 改过、实机也未复现解析失败，且其"125=bncmdhbgahe"邻位说法与
		// 在库表矛盾（bncmdhbgahe=600）——判定为当时误读，2026-11-06
		// 审查复核后定案保留 181（改动前需官方 307 实抓佐证）。
		23: proto.Args(proto.S("181"), vouchers...),
		24: build745(),
		25: season,
	})
}

// hexOf 把 bytes 树转 hex 列表（ArrayOf 用）。
func hexOf(vals []*proto.Value) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, v.Hex)
	}
	return out
}

// ---- 技能树 ----

// skillCatOrder 是技能树大类的显示顺序。
var skillCatOrder = []int64{1, 2, 3, 10, 11, 4, 5, 6, 7, 8, 9}

// profCap 永久熟练度上限。
const profCap = 100.0

// nodeUnlocked 计算技能节点在 313.f6 中的解锁状态。
// 客户端直接显示该值，因此服务端必须按当前熟练度判断；显式 enabled 的节点始终可用。
func nodeUnlocked(nd skillNodeDef, prof, gear float64) bool {
	if nd.Enabled {
		return true
	}
	total := gear + prof
	if total > effectiveSkillCap {
		total = effectiveSkillCap
	}
	return total+1e-6 >= float64(nd.UnlockPct)
}

// BuildSkillTree311 技能树（5/1 响应；2/6 推送复用，f0 由调用方覆写）。
func (s *Server) BuildSkillTree311(ctx context.Context, uid int64, availablePointsOverride uint64) (*proto.Value, error) {
	p, err := s.DB.EnsureProfile(ctx, uid)
	if err != nil {
		return nil, err
	}
	effects, err := s.effectiveFishingSkills(ctx, uid)
	if err != nil {
		return nil, err
	}
	total := skillPointsTotal(p.Level) + s.skillBonusPoints(ctx, uid)
	used := s.skillSpentPoints(ctx, uid)
	avail := uint64(0)
	if total > used {
		avail = uint64(total - used)
	}
	if availablePointsOverride != ^uint64(0) {
		avail = availablePointsOverride
	}

	cats := proto.Args(proto.S("121"))
	for _, catID := range skillCatOrder {
		cats.Items = append(cats.Items, s.skillCategory312(catID, effects))
	}
	return wiregen.MakeObj("djndmghegld", map[int]*proto.Value{
		0: proto.U32(uint32(avail)),
		1: cats,
		2: proto.U64(500),
		3: proto.Null(),
	})
}

// skillNodeDef 技能节点定义（PG skill_def）。
type skillNodeDef struct {
	NodeID    int64
	CatID     int64
	UnlockPct float32
	MaxLevel  int64
	EffectPct float32
	GroupID   int64
	Enabled   bool
}

// skillDefsOfCat 从 skill_def 读取大类节点并缓存。
func (s *Server) skillDefsOfCat(catID int64) []skillNodeDef {
	s.skillDefsOnce.Do(func() {
		rows, err := s.DB.SQL.QueryContext(context.Background(), `
			SELECT cat_id, node_id, unlock_pct, max_level, effect_pct, group_id, enabled
		FROM skill_def ORDER BY cat_id, ordinal, node_id`)
		if err != nil {
			return
		}
		defer rows.Close()
		m := map[int64][]skillNodeDef{}
		for rows.Next() {
			var d skillNodeDef
			var enabled int64
			if err := rows.Scan(&d.CatID, &d.NodeID, &d.UnlockPct, &d.MaxLevel, &d.EffectPct, &d.GroupID, &enabled); err != nil {
				continue
			}
			d.Enabled = enabled != 0
			m[d.CatID] = append(m[d.CatID], d)
		}
		s.skillDefs = m
	})
	return s.skillDefs[catID]
}

func f32bitsOf(v float32) uint32 { return proto.F32Bits(v) }

func boolU32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// ---- 快捷栏 136 ----

func build136(slot int64, guid string) *proto.Value {
	return wiregen.MustMakeObj("iiemhjlblcc", map[int]*proto.Value{
		0: proto.U32(uint32(slot)),
		1: proto.Guid(guid),
	})
}

// BuildShortcutList 11/1 快照：args("65")[136...]。
func (s *Server) BuildShortcutList(ctx context.Context, uid int64) *proto.Value {
	out := proto.Args(proto.S("65"))
	if slots, err := s.DB.ListShortcuts(ctx, uid); err == nil {
		for _, sc := range slots {
			out.Items = append(out.Items, build136(sc.Slot, sc.GUID))
		}
	}
	return out
}

// ---- 推送 ----

// Push2_1 推送当前档案，用于刷新货币与等级。
func (s *Session) Push2_1() {
	p, err := s.Srv.BuildProfile307W(s.pushCtx(), s.Userid, worldTotalGameMin(time.Now()))
	if err != nil {
		s.log.Warn("2/1 构造失败", "err", err)
		return
	}
	s.Push(2, 1, proto.Args524(p))
}

// Push2_6 推送升级后的技能树，外层必须是 args(524)[311]。
// f0 表示当前可用技能点；客户端用前后差值显示新增点数。
// 多级连升只发送一次，fromLevel 仅用于日志。
func (s *Session) Push2_6(fromLevel int64) {
	tree, err := s.Srv.BuildSkillTree311(s.pushCtx(), s.Userid, ^uint64(0)) // f0=自然可用点
	if err != nil {
		s.log.Warn("2/6 构造失败", "err", err)
		return
	}
	s.Push(2, 6, proto.Args524(tree))
}

// Push2_9 通知推送（306；type=100 放生额外经验）。
func (s *Session) Push2_9(notifyType byte, extraXP uint64) {
	obj := wiregen.MustMakeObj("nkoicoaemen", map[int]*proto.Value{
		0: proto.U32(uint32(notifyType)),
		1: proto.U32(0),
		2: proto.U64(extraXP),
		3: proto.U32(0),
		4: proto.Str(nil),
	})
	s.Push(2, 9, proto.Args524(obj))
}

// Push4_21 物品移除推送：args("524")[guidarr]。
func (s *Session) Push4_21(guids []string) {
	s.Push(4, 21, proto.Args524(proto.GuidArr(guids)))
}

// Push4_22Items 搏鱼全量同步并附上指定物品的树。
// 断竿会把配件摘回背包，而 11028 的移除清单会让客户端删掉这些本地物品，
// 必须靠这条推送把它们按背包状态重新装回来。
func (s *Session) Push4_22Items(extraGUIDs []string) {
	pctx := s.pushCtx()
	items, err := s.Srv.buildFightItems(pctx, s.Userid, true)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, item := range items {
		if idx := fieldIdxByCS(item.Class, csGUID); idx >= 0 && idx < len(item.Fields) {
			seen[item.Fields[idx].GuidHex()] = true
		}
	}
	for _, g := range extraGUIDs {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		row, gerr := s.Srv.DB.GetItemByGUID(pctx, g)
		if gerr != nil || row == nil || row.UID != s.Userid {
			continue
		}
		tree, terr := s.Srv.ItemToTree(pctx, row)
		if terr != nil || tree == nil {
			continue
		}
		items = append(items, tree)
	}
	if len(items) == 0 {
		return
	}
	s.Push(4, 22, proto.Args(proto.S("61"), items...))
}

// Push4_22ItemGUIDs 只推指定背包物品的最新树。
// 吃饵、剪线等背包数量变化走这条通道；不要把竿/线混进背包更新。
func (s *Session) Push4_22ItemGUIDs(guids []string) {
	pctx := s.pushCtx()
	var trees []*proto.Value
	seen := map[string]bool{}
	for _, g := range guids {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		row, err := s.Srv.DB.GetItemByGUID(pctx, g)
		if err != nil || row == nil || row.UID != s.Userid {
			continue
		}
		tree, err := s.Srv.ItemToTree(pctx, row)
		if err == nil && tree != nil {
			trees = append(trees, tree)
		}
	}
	if len(trees) > 0 {
		s.Push(4, 22, proto.Args(proto.S("61"), trees...))
	}
}

// Push4_22Full 搏鱼全量同步：args("61")[157竿, 140线]。
func (s *Session) Push4_22Full() {
	items, err := s.Srv.buildFightItems(s.pushCtx(), s.Userid, true)
	if err != nil || len(items) == 0 {
		return
	}
	s.Push(4, 22, proto.Args(proto.S("61"), items...))
}

// hookMountedFlag 中鱼挂竿运行位：官方真服抓包（2026-11-10/11）四钓法 9 次中鱼
// 钩逐帧验证，hgaijieklek 一律 = 定义基值（钩 20992）| 1024。缺位时客户端钩
// 实体不进搏鱼态，表现为浮漂无咬钩演出、左下角无搏鱼指示。
// 钩推送形状随钓组而变：单钩/单钩组=裸钩 args(61)[131]（沉底/浮钓/海钓单钩），
// 串钩组/拟饵 rig=ioombppcnlh 嵌套容器（海钓串钩、路亚）；flags 语义同构。
const hookMountedFlag = uint64(1024)

// markHookMounted 递归把树中所有钩对象（agedjipcnpe）的 flags 补挂竿位，
// 兼容裸钩（沉底/浮钓/海钓单钩）与容器嵌套钩（海钓串钩/路亚 ioombppcnlh）两种形状。
func markHookMounted(v *proto.Value) {
	if v == nil {
		return
	}
	if v.Class == "agedjipcnpe" {
		if idx := fieldIdxByCS("agedjipcnpe", csStateEnum); idx >= 0 && idx < len(v.Fields) &&
			v.Fields[idx].Num != nil {
			*v.Fields[idx].Num |= hookMountedFlag
		}
	}
	for _, f := range v.Fields {
		if f.Val != nil {
			markHookMounted(f.Val)
		}
	}
	for _, it := range v.Items {
		markHookMounted(it)
	}
}

// Push4_22Hook 刺中后单钩同步：args("61")[131钩]。
// mounted=true 时钩 flags 附挂竿位（官方口径：四钓法中鱼钩均带挂竿位）。
func (s *Session) Push4_22Hook(mounted bool, gears ...string) {
	items, err := s.Srv.buildFightItems(s.pushCtx(), s.Userid, false, gears...)
	if err != nil || len(items) == 0 {
		return
	}
	if mounted {
		for _, it := range items {
			markHookMounted(it)
		}
	}
	s.Push(4, 22, proto.Args(proto.S("61"), items...))
}

// pushCtx 推送上下文（超时保护）。
func (s *Session) pushCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ctx
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
