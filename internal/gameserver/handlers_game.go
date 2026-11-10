package gameserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 登录时间线、容器、技能、快捷栏、地图和钓鱼处理器。

// requireDB 有 DB 才处理，否则交给兜底链。
func (s *Server) requireDB(ctx *Ctx) bool { return s.DB != nil }

// ---- 3/1 profile ----

func hProfile(ctx *Ctx) ([]byte, bool) {
	if !ctx.Sess.Srv.requireDB(ctx) {
		return nil, false
	}
	p, err := ctx.Sess.Srv.BuildProfile307W(ctx.Sess.pushCtx(), ctx.Sess.Userid,
		worldTotalGameMin(time.Now()))
	if err != nil {
		ctx.Log.Warn("3/1 构造失败", "err", err)
		return nil, false
	}
	body, err := proto.EncodeValue(p, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// 客户端库存枚举。
const (
	containerBackpack      = 0
	containerKeepnet       = 5
	containerTradingCards  = 18
	houseContainerCapacity = 100
)

// ---- 4/1 items/count ----

func hItemCount(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	container, _ := ctx.ArgInt(0)
	var used, cap, capMember int64
	cctx := ctx.Sess.pushCtx()
	switch container {
	case containerBackpack:
		used, _ = srv.DB.CountItemsByContainers(cctx, ctx.Sess.Userid, []string{"backpack", "rig"})
	case containerKeepnet:
		srv.retryCompletedCatchRecovery(ctx.Sess)
		n, err := srv.DB.KeepnetList(cctx, ctx.Sess.Userid)
		if err == nil {
			used = int64(len(n))
		}
		cap, capMember = 100, 150
	case 10, 11:
		// 房屋仓库：容量按 100 起步。回 0 时客户端认为仓库容量为零，
		// 存放交互在 UI 层就被拦下。
		used, _ = srv.DB.CountItemsByContainers(cctx, ctx.Sess.Userid, []string{"house"})
		cap = houseContainerCapacity
	default:
		if names := nativeInventoryContainers(container); names != nil {
			used, _ = srv.DB.CountItemsByContainers(cctx, ctx.Sess.Userid, names)
		}
	}
	cap, capMember = srv.importedContainerCapacity(cctx, ctx.Sess.Userid, container, cap, capMember)
	obj := wiregen.MustMakeObj("pbndmldbfai", map[int]*proto.Value{
		0: proto.U32(uint32(used)),
		1: proto.U32(uint32(cap)),
		2: proto.U32(uint32(capMember)),
	})
	body, err := proto.EncodeValue(obj, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// ---- 4/4 items/guids ----

func hItemGUIDs(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	container, _ := ctx.ArgInt(0)
	cctx := ctx.Sess.pushCtx()
	var guids []string
	switch container {
	case containerBackpack:
		guids, _ = srv.DB.ItemGUIDsByContainers(cctx, ctx.Sess.Userid, []string{"backpack", "rig"})
	case containerKeepnet:
		srv.retryCompletedCatchRecovery(ctx.Sess)
		list, err := srv.DB.KeepnetList(cctx, ctx.Sess.Userid)
		if err == nil {
			for _, e := range list {
				guids = append(guids, e.EchoGUID)
			}
		}
	default:
		if names := nativeInventoryContainers(container); names != nil {
			guids, _ = srv.DB.ItemGUIDsByContainers(cctx, ctx.Sess.Userid, names)
		}
	}
	if ctx.Tree.ArgCount() >= 3 {
		start, startOK := ctx.ArgInt(1)
		count, countOK := ctx.ArgInt(2)
		if !startOK || !countOK || start > 100000 || count > 500 {
			ctx.ReplyFailure("System.ArgumentException", "物品分页参数无效")
			return nil, true
		}
		if start >= uint64(len(guids)) {
			guids = nil
		} else {
			end := start + count
			if end > uint64(len(guids)) {
				end = uint64(len(guids))
			}
			guids = guids[start:end]
		}
	}
	body, err := proto.EncodeValue(proto.GuidArr(guids), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// ---- 4/5 items/details：args("61")[物品树...] ----

func hItemDetails(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	container, _ := ctx.ArgInt(0)
	guids := deepGUIDs(ctx.Tree)
	cctx := ctx.Sess.pushCtx()
	out := proto.Args(proto.S("61"))
	if container == containerKeepnet {
		srv.retryCompletedCatchRecovery(ctx.Sess)
		for _, g := range guids {
			e, err := srv.DB.KeepnetGet(cctx, ctx.Sess.Userid, g)
			if err != nil || e == nil {
				continue
			}
			out.Items = append(out.Items, srv.keepnetItemTree(cctx, e, srv.currentLevel(ctx.Sess.Userid)))
		}
	} else if containers := nativeInventoryContainers(container); containers != nil {
		for _, g := range guids {
			row, err := srv.DB.GetItemByGUID(cctx, g)
			if err != nil || row == nil || row.UID != ctx.Sess.Userid || !inventoryContainerMatches(row.Container, containers) {
				continue
			}
			tree, err := srv.ItemToTree(cctx, row)
			if err != nil {
				continue
			}
			out.Items = append(out.Items, tree)
		}
	}
	body, err := proto.EncodeValue(out, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// deepGUIDs 深扫 args 里全部 guid 值。
func deepGUIDs(v *proto.Value) []string {
	var out []string
	var walk func(x *proto.Value)
	walk = func(x *proto.Value) {
		if x == nil {
			return
		}
		switch x.Kind {
		case proto.KindGuid:
			out = append(out, x.Hex)
		case proto.KindArgs, proto.KindValList, proto.KindArray, proto.KindU32Arr, proto.KindGuidArr:
			for _, it := range x.Items {
				walk(it)
			}
		case proto.KindObj:
			for i := range x.Fields {
				if x.Fields[i].Op == "GUID16" && x.Fields[i].Hex != "" {
					out = append(out, x.Fields[i].Hex)
				}
				if x.Fields[i].Val != nil {
					walk(x.Fields[i].Val)
				}
			}
		}
	}
	walk(v)
	return out
}

// ---- 5/1 skills/get ----

func hSkillsGet(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	tree, err := srv.BuildSkillTree311(ctx.Sess.pushCtx(), ctx.Sess.Userid, ^uint64(0))
	if err != nil {
		ctx.Log.Error("技能树构建失败（回兜底空包会卡客户端登录）", "err", err)
		return nil, false
	}
	body, err := proto.EncodeValue(tree, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// ---- 11/1 快捷栏快照、11/2 绑定、11/3 解绑 ----

func hShortcutSnapshot(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	body, err := proto.EncodeValue(srv.BuildShortcutList(ctx.Sess.pushCtx(), ctx.Sess.Userid), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

func hShortcutBind(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	slot, validSlot := ctx.ArgInt(0)
	guids := deepGUIDs(ctx.Tree)
	if !validSlot || slot > 2147483647 || len(guids) != 1 {
		ctx.ReplyFailure("System.ArgumentException", "快捷栏绑定参数无效")
		return nil, true
	}
	g := guids[0]
	if err := srv.DB.BindShortcut(ctx.Sess.pushCtx(), ctx.Sess.Userid, int64(slot), g); err != nil {
		ctx.ReplyFailure("System.ArgumentException", "快捷栏物品不可用")
		return nil, true
	}
	body, err := proto.EncodeValue(build136(int64(slot), g), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

func hShortcutUnbind(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	slot, validSlot := ctx.ArgInt(0)
	if !validSlot || slot > 2147483647 {
		ctx.ReplyFailure("System.ArgumentException", "快捷栏解绑参数无效")
		return nil, true
	}
	if err := srv.DB.BindShortcut(ctx.Sess.pushCtx(), ctx.Sess.Userid, int64(slot), ""); err != nil {
		ctx.ReplyFailure("System.InvalidOperationException", "快捷栏解绑未保存")
		return nil, true
	}
	body, err := proto.EncodeValue(build136(int64(slot), ""), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// ---- 3/30 / 3/31 钓组物品同步 ----

func build800(f0, f1, f2 *proto.Value) *proto.Value {
	return wiregen.MustMakeObj("dlbahppbdad", map[int]*proto.Value{
		0: f0,
		1: f1,
		2: f2,
	})
}

func hRigSyncBackpack(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	cctx := ctx.Sess.pushCtx()
	bp := proto.Args(proto.S("61"))
	rg := proto.Args(proto.S("61"))
	guids, _ := srv.DB.ItemGUIDsByContainers(cctx, ctx.Sess.Userid, []string{"backpack"})
	for _, g := range guids {
		if row, err := srv.DB.GetItemByGUID(cctx, g); err == nil {
			if tree, terr := srv.ItemToTree(cctx, row); terr == nil {
				bp.Items = append(bp.Items, tree)
			}
		}
	}
	rguids, _ := srv.DB.ItemGUIDsByContainers(cctx, ctx.Sess.Userid, []string{"rig"})
	for _, g := range rguids {
		if row, err := srv.DB.GetItemByGUID(cctx, g); err == nil {
			if tree, terr := srv.ItemToTree(cctx, row); terr == nil {
				rg.Items = append(rg.Items, tree)
			}
		}
	}
	body, err := proto.EncodeValue(build800(bp, rg, proto.Null()), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

func hRigSyncRig(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	cctx := ctx.Sess.pushCtx()
	rg := proto.Args(proto.S("61"))
	rguids, _ := srv.DB.ItemGUIDsByContainers(cctx, ctx.Sess.Userid, []string{"rig"})
	for _, g := range rguids {
		if row, err := srv.DB.GetItemByGUID(cctx, g); err == nil {
			if tree, terr := srv.ItemToTree(cctx, row); terr == nil {
				rg.Items = append(rg.Items, tree)
			}
		}
	}
	ga := proto.GuidArr(rguids)
	body, err := proto.EncodeValue(build800(proto.Null(), rg, ga), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// ---- 3/37 金币买断地图 ----

// hPlaceBuyout 处理 3/37 地图买断。
// 请求参数为 [place, packetId, price]，响应为 307 玩家档案。
//
// 价格必须等于静态买断价才扣金币，防止客户端改价。成功后写入 place_access
// 和交易流水；档案 307 的字段 22 用于向客户端表示已买断。
func hPlaceBuyout(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	place, packet, price := parsePlaceBuyoutArgs(ctx.Tree)
	cctx := ctx.Sess.pushCtx()
	uid := ctx.Sess.Userid
	if tier := placeBuyoutPrice(place, packet); tier > 0 && price == tier {
		// 金币买断（百分位）：扣款 → 授予 → 流水
		if p, err := srv.DB.EnsureProfile(cctx, uid); err == nil && p != nil {
			_ = srv.DB.UpdateBalance(cctx, uid, p.Silver, p.Gold-tier, p.Bank)
		}
		if err := srv.DB.PlaceAccessInsert(cctx, uid, place, "buyout"); err != nil {
			ctx.Log.Warn("3/37 授予地点失败", "place", place, "err", err)
		}
		_ = srv.DB.AddTxn(cctx, uid, "buy", place, "", 0, -tier, "place_buyout")
		ctx.Log.Info("3/37 金币买断地图", "place", place, "tier", packet,
			"gold", -tier, "gold_yuan", float64(tier)/100.0)
	} else {
		tiers, _ := placeBuyoutTiers[place]
		ctx.Log.Info("3/37 价格与静态表不符，不扣款", "place", place,
			"packet", packet, "price", price, "tiers", tiers)
	}
	p, err := srv.BuildProfile307W(cctx, uid, worldTotalGameMin(time.Now()))
	if err != nil {
		ctx.Log.Warn("3/37 档案构造失败", "err", err)
		return nil, false
	}
	body, err := proto.EncodeValue(p, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// parsePlaceBuyoutArgs 顶层 args 位置取参：[0]=place 字符串、[1]=packetId、
// [2]=price（long）。缺参按零值处理（调用方走"价格不符"分支）。
func parsePlaceBuyoutArgs(tree *proto.Value) (place string, packet, price int64) {
	if tree == nil || tree.Kind != proto.KindArgs {
		return "", 0, 0
	}
	ints := 0
	for _, it := range tree.Items {
		if it == nil {
			continue
		}
		switch it.Kind {
		case proto.KindStr:
			if it.Str != nil && *it.Str != "" && place == "" {
				place = *it.Str
			}
		case proto.KindU32, proto.KindU64:
			v := int64(it.Num)
			switch ints {
			case 0:
				packet = v
			case 1:
				price = v
			}
			ints++
		}
	}
	return place, packet, price
}

// ---- 1/12 place / 1/14 travel（切图）----

// hPlace 处理 1/12 地图读取，返回 205 dob。
// 请求为 [地图 key]；未知地图回退出生地图，避免客户端读取失败。
func hPlace(ctx *Ctx) ([]byte, bool) {
	key := spawnLevelKey
	if ctx.Tree != nil {
		if v := firstNonEmptyStr(ctx.Tree); v != "" {
			key = v
		}
	}
	key = travelLevelKey(key)
	nowUnix := time.Now().Unix()
	body, err := ctx.Sess.Srv.buildMapDobBody(ctx.Sess.Userid, key, nowUnix)
	if err != nil {
		ctx.Log.Warn("1/12 dob 构造失败", "level", key, "err", err)
		return nil, false
	}
	ctx.Sess.rememberWeather(key, nowUnix)
	return body, true
}

// settleTravel 在响应体构造成功后，把「同图判定、免费资格、银币扣款、流水、
// 位置写回」放进同一个数据库事务：任一步失败整体回滚，不会出现已扣旅费却
// 仍在原地图的状态。charge=false 用于 1/13 空请求的登录恢复（不收费）。
//
// 返回 charged（本次是否实际扣款，决定是否需要 2/1 刷新档案）和 ok；ok=false
// 时已回业务错误，调用方必须中止切图。事务内先取得 profile 写锁再读当前地图，
// 并发的重复切图因此串行执行，后一个请求能看到前一个已提交的位置而不重复扣款。
func settleTravel(ctx *Ctx, level, scene string, spot int64, charge bool) (charged, ok bool) {
	srv := ctx.Sess.Srv
	uid := ctx.Sess.Userid
	cctx := ctx.Sess.pushCtx()
	cost := int64(0)
	if charge {
		cost = placeTravelCost(level)
	}

	tx, err := beginEconomyTx(cctx, srv.DB)
	if err != nil {
		ctx.Log.Warn("切图开启事务失败", "level", level, "err", err)
		ctx.ReplyFailure("System.InvalidOperationException", "切图结算失败，请重试")
		return false, false
	}
	defer func() { _ = tx.Rollback() }()

	// 先取写锁：DEFERRED 事务不能在读完旧位置后再升级，也不能让两个并发
	// 切图都基于同一份旧位置各自扣款。
	if _, err := tx.ExecContext(cctx, `UPDATE profile SET uid=uid WHERE uid=?`, uid); err != nil {
		ctx.Log.Warn("切图锁定玩家档案失败", "level", level, "err", err)
		ctx.ReplyFailure("System.InvalidOperationException", "切图结算失败，请重试")
		return false, false
	}
	if err := ensureProfileTx(cctx, tx, uid); err != nil {
		ctx.Log.Warn("切图建立玩家档案失败", "level", level, "err", err)
		ctx.ReplyFailure("System.InvalidOperationException", "切图结算失败，请重试")
		return false, false
	}
	var current string
	if err := tx.QueryRowContext(cctx,
		`SELECT COALESCE(last_level_key,'') FROM profile WHERE uid=?`, uid).Scan(&current); err != nil {
		ctx.Log.Warn("切图读取当前地图失败", "level", level, "err", err)
		ctx.ReplyFailure("System.InvalidOperationException", "切图结算失败，请重试")
		return false, false
	}
	sameLevel := travelLevelKey(current) == level

	if cost > 0 && !sameLevel {
		free, err := hasFreeTravelAccessTx(cctx, tx, uid, level)
		if err != nil {
			ctx.Log.Warn("切图免费通行判定失败", "level", level, "err", err)
			ctx.ReplyFailure("System.InvalidOperationException", "切图结算失败，请重试")
			return false, false
		}
		if !free {
			if err := applyWalletTx(cctx, tx, uid, -cost, 0,
				"buy", level, "", "place_travel"); err != nil {
				if errors.Is(err, errInsufficientFunds) {
					ctx.Log.Info("切图银币不足，不切图", "level", level, "cost", cost)
					ctx.ReplyFailure("System.InvalidOperationException", "银币不足，无法前往该水域")
				} else {
					ctx.Log.Warn("切图银币扣款失败", "level", level, "err", err)
					ctx.ReplyFailure("System.InvalidOperationException", "切图结算失败，请重试")
				}
				return false, false
			}
			charged = true
		}
	}
	if err := srv.DB.SetPlayerLocationTx(cctx, tx, uid, level, scene, spot); err != nil {
		ctx.Log.Warn("切图位置落库失败，扣款已回滚", "level", level, "err", err)
		ctx.ReplyFailure("System.InvalidOperationException", "切图结算失败，请重试")
		return false, false
	}
	if err := tx.Commit(); err != nil {
		ctx.Log.Warn("切图事务提交失败，扣款已回滚", "level", level, "err", err)
		ctx.ReplyFailure("System.InvalidOperationException", "切图结算失败，请重试")
		return false, false
	}
	if charged {
		ctx.Log.Info("切图扣银币", "level", level, "silver", -cost)
	}
	return charged, true
}

// hTravel 处理 1/14 切图，返回 [dob(205), 护网快照(800), 玩家档案(307), null]。
// 先预构造响应体（构造失败不结算），再把扣款、流水和位置写回放进同一事务；
// 提交成功后重新构造 307，使响应中的余额与实扣一致。
func hTravel(ctx *Ctx) ([]byte, bool) {
	if !ctx.Sess.Srv.requireDB(ctx) {
		return nil, false
	}
	key := spawnLevelKey
	if ctx.Tree != nil {
		if v := firstNonEmptyStr(ctx.Tree); v != "" {
			key = v
		}
	}
	key = travelLevelKey(key)
	sess := ctx.Sess
	// 凭证地图不再在入口一律拒绝：settleTravel 在同一事务内判断有效
	// trip/buyout/house 是否免票；无有效凭证时按普通切图费收费，余额不足
	// 则整体回滚。这样票到期后可以重新付费进入，也不会无扣款绕过切图结算。
	nowUnix := time.Now().Unix()
	// 预构造只用于确认响应可用，避免扣款后才暴露构造失败。
	body, err := sess.Srv.buildTravelBodyAt(sess, key, nowUnix)
	if err != nil {
		ctx.Log.Warn("1/14 travel 构造失败", "level", key, "err", err)
		return nil, false
	}
	scene := sceneForLevel(key)
	var spot int64
	if ints := treeIntsDeep(ctx.Tree); len(ints) > 0 {
		spot = int64(ints[0])
	}
	charged, ok := settleTravel(ctx, key, scene, spot, true)
	if !ok {
		return nil, true
	}
	if charged {
		// 307 必须反映实扣后的余额；重新构造是只读的，不会再改状态。极端
		// 情况下构造失败时仍返回预构造响应，且切图已成功，才用 2/1 刷新档案。
		if fresh, err := sess.Srv.buildTravelBodyAt(sess, key, nowUnix); err == nil {
			body = fresh
		} else {
			ctx.Log.Warn("1/14 扣款后档案构造失败，改用 2/1 刷新", "level", key, "err", err)
			sess.Push2_1()
		}
	}
	sess.Srv.setLevel(sess.Userid, key)
	sess.rememberWeather(key, nowUnix)
	// 兼容旧存档：只对 last_consume_ts=0 的历史已购行补启表；新购票已在
	// 3/38 购买事务中启表。
	if sess.Srv.startTripTimers(sess.pushCtx(), sess.Userid, key) {
		sess.Push2_1()
	}
	// 装备包与地图同步：进入有效钓鱼游地图发放，切离的地图回收
	// （官方生命周期"进图发放/离图回收"，见 trip_equip.go）。
	sess.Srv.syncTripEquipment(sess.pushCtx(), sess.Userid, key)
	ctx.Log.Info("1/14 travel", "level", key, "scene", scene, "spot", spot)
	return body, true
}

// ---- 1/13 enter map（登录定位：优先恢复上次所在地图）----

func hEnterMap(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	sess := ctx.Sess
	uid := sess.Userid
	requested := ""
	if ctx.Tree != nil {
		requested = firstNonEmptyStr(ctx.Tree)
	}

	level, scene, spot := spawnLevelKey, sceneForLevel(spawnLevelKey), int64(0)
	saved, err := srv.DB.GetPlayerLocation(sess.pushCtx(), uid)
	if err != nil {
		ctx.Log.Warn("1/13 读取上次位置失败，回出生地图", "err", err)
	} else if saved != nil && validLevelKey(saved.LevelKey) {
		level, scene, spot = saved.LevelKey, saved.Scene, saved.Spot
	}

	// 1/13 若带地图 key，则以本次进入作为一次成功切图；空请求是登录恢复，
	// 只写回位置、不收旅费。
	charge := requested != ""
	if charge {
		level = travelLevelKey(requested)
		scene = sceneForLevel(level)
	}
	level = travelLevelKey(level)
	if scene == "" {
		scene = sceneForLevel(level)
	}
	// 空请求只恢复上次位置；银币票停留没有 access 记录，不能直接用
	// tripLevelLocked 判断。只有上次地图明确依赖已到期钓鱼游且之后没有
	// 新银币票流水时才回出生图。带 key 的真正进入交给 settleTravel：
	// 买断/有效钓鱼游/房屋免票，否则扣银币门票。
	if !charge && srv.loginRestoreTripExpired(sess.pushCtx(), uid, level) {
		ctx.Log.Info("1/13 上次地图钓鱼游已到期，回出生地图", "level", level)
		level, scene, spot = spawnLevelKey, sceneForLevel(spawnLevelKey), 0
	}

	nowUnix := time.Now().Unix()
	body, err := srv.buildSpawnLocatorBody(uid, level, scene, nowUnix)
	if err != nil {
		ctx.Log.Warn("1/13 出生点定位构造失败", "level", level, "err", err)
		return nil, false
	}
	charged, ok := settleTravel(ctx, level, scene, spot, charge)
	if !ok {
		return nil, true
	}
	if charged {
		// 1/13 的 206 响应不含档案；切图事务提交成功后才推送 2/1 刷新余额。
		sess.Push2_1()
	}
	srv.setLevel(uid, level)
	sess.rememberWeather(level, nowUnix)
	// 兼容旧存档：重登时补启 last_consume_ts=0 的历史已购行。
	if srv.startTripTimers(sess.pushCtx(), uid, level) {
		sess.Push2_1()
	}
	// 装备包与地图同步：落点=有效钓鱼游地图 → 发放；其他地图残留 → 回收。
	srv.syncTripEquipment(sess.pushCtx(), uid, level)
	ctx.Log.Info("1/13 下发出生地图", "level", level, "scene", scene, "spot", spot, "bytes", len(body))
	return body, true
}

func firstNonEmptyStr(v *proto.Value) string {
	if v == nil {
		return ""
	}
	switch v.Kind {
	case proto.KindArgs, proto.KindValList:
		for _, it := range v.Items {
			if it != nil && it.Kind == proto.KindStr && it.Str != nil && *it.Str != "" {
				return *it.Str
			}
			if s := firstNonEmptyStr(it); s != "" {
				return s
			}
		}
	case proto.KindObj:
		for i := range v.Fields {
			if v.Fields[i].Str != nil && *v.Fields[i].Str != "" {
				return *v.Fields[i].Str
			}
		}
	}
	return ""
}

// hSetQuivertip 处理 17/5 竿稍设置。
// 请求为 [容器, 竿 GUID, 型号键]。竿实例的 f8 是抛竿校验读取的权威值，
// 因此同时更新 rod_setting 和实例字段；响应 800.f1 返回更新后的竿树。
func hSetQuivertip(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	gs := deepGUIDs(ctx.Tree)
	strs := treeStrsDeep(ctx.Tree)
	rodGUID, tip := "", ""
	if len(gs) > 0 {
		rodGUID = gs[0]
	}
	if len(strs) > 0 {
		tip = strs[0]
	}
	if rodGUID == "" || tip == "" {
		return nil, false // 交给兜底
	}
	cctx := ctx.Sess.pushCtx()
	row := srv.ownedRodRow(cctx, ctx.Sess.Userid, rodGUID)
	if row == nil {
		ctx.Log.Warn("17/5 竿不属于当前玩家，已忽略", "竿", shortGUID(rodGUID))
		return nil, true
	}
	_ = srv.DB.UpsertRodSetting(cctx, rodGUID, "quivertip", tip)
	if fields, ferr := FieldsFromJSON(row.FieldsJSON); ferr == nil {
		tidx := fieldIdxByCS(row.WireClass, "kjphlnnlflh")
		if tidx >= 0 {
			for len(fields) <= tidx {
				zf, zerr := proto.ZeroField(fieldOpAt(row.WireClass, len(fields)))
				if zerr != nil {
					break
				}
				fields = append(fields, zf)
			}
			fields[tidx].Str = &tip
			// 兼容旧版把 U32N 的字符串值写进 STR 标签；
			// fields_json 读取会丢失 u32n 标签下的字符串。
			fields[tidx].Op = "STR"
			if fj, jerr := FieldsToJSON(fields); jerr == nil {
				_ = srv.DB.UpdateItemFields(cctx, row.GUID, fj, row.Qty, row.Durability, row.Workmanship, row.RigGUID)
			}
		}
	}
	// 响应 800，Field 1（更新段）= 刚更新的竿实例。
	if tree, terr := srv.ItemToTree(cctx, row); terr == nil {
		body, encErr := proto.EncodeValue(build800(proto.Null(), proto.Args(proto.S("61"), tree), proto.Null()), ctx.Sess.Table())
		if encErr == nil {
			return body, true
		}
	}
	return nil, false
}

// treeStrsDeep 深扫字符串。
func treeStrsDeep(v *proto.Value) []string {
	var out []string
	var walk func(x *proto.Value)
	walk = func(x *proto.Value) {
		if x == nil {
			return
		}
		switch x.Kind {
		case proto.KindStr:
			if x.Str != nil {
				out = append(out, *x.Str)
			}
		case proto.KindArgs, proto.KindValList, proto.KindArray, proto.KindGuidArr, proto.KindU32Arr:
			for _, it := range x.Items {
				walk(it)
			}
		case proto.KindObj:
			for i := range x.Fields {
				if x.Fields[i].Str != nil {
					out = append(out, *x.Fields[i].Str)
				}
			}
		}
	}
	walk(v)
	return out
}

// ---- 14/x 钓鱼 ----

func hCastPrepare(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	guids := deepGUIDs(ctx.Tree)
	gear := ""
	if len(guids) > 0 {
		gear = guids[0]
	}
	st := srv.stateFor(ctx.Sess.Userid)
	if !srv.pendingCatchAllowsCast(ctx, st) {
		return nil, true
	}
	st = srv.selectFishingRod(ctx.Sess.Userid, gear)
	// 未保存过 17/15 时按新竿 f5.f7 的默认值处理，避免客户端显示 15、
	// 服务端判鱼却按 0（无 trolling 加成）导致状态不一致。
	reelSpeed := defaultReelSpeed
	if saved, ok := srv.rodReelSpeedSetting(ctx.Sess.pushCtx(), gear); ok {
		reelSpeed = saved
	}
	st.mu.Lock()
	st.GearGuid = gear
	st.ReelSetting = reelSpeed
	if rigTypeLureFamily[st.RigType] {
		st.LureMotionMask = 0
		st.LureAction = ""
		st.LureLayer = .5
		st.LurePulseCount = 0
		st.LurePulseAt = time.Time{}
		st.LureToggleCount = 0
		st.LureToggleAt = time.Time{}
		st.LureStateAt = time.Time{}
		st.LureLayerAt = time.Time{}
	}
	depthCM := st.DepthCM
	rigType := st.RigType
	st.mu.Unlock()
	if err := ctx.Sess.prepareBaitForCast(st, gear); err != nil {
		ctx.Log.Warn("抛竿前补饵失败", "err", err)
		return nil, false
	}
	// 收敛钓组数据：补齐 f5、rig 行、组件和尖梢。
	// 尖梢有更新时推送 4/22 全量竿树，因为客户端抛竿前会校验实例 f8。
	if srv.ensureRig(ctx.Sess.pushCtx(), ctx.Sess.Userid, gear) {
		ctx.Sess.Push4_22Full()
	}
	if row, err := srv.DB.GetItemByGUID(ctx.Sess.pushCtx(), gear); err == nil && row != nil {
		if row.Durability <= 0 {
			// 断竿/耐久归零的竿不可抛投（客户端可能不理会 17/7 的拒取
			// 响应继续持竿，14/1 必须再拦一道）。
			st.mu.Lock()
			st.GearGuid = ""
			st.mu.Unlock()
			ctx.Log.Info("14/1 被拒：鱼竿已损坏", "rod", shortGUID(gear))
			if body := castReady158(ctx, "", depthCM, 0); body != nil {
				return body, true
			}
			return nil, false
		}
		rigType = srv.rigTypeOfItem(ctx.Sess.pushCtx(), row)
		st.mu.Lock()
		st.RigType = rigType
		st.mu.Unlock()
	}
	// 水域装备限制：当前地图禁用的竿不可抛投准备（与 17/7 同一套拦截，
	// 客户端直发 14/1 时兜底）。
	if reason := srv.placeRodViolation(ctx.Sess.pushCtx(), ctx.Sess.Userid, gear); reason != "" {
		st.mu.Lock()
		st.GearGuid = ""
		st.mu.Unlock()
		ctx.Log.Info("14/1 被拒：装备限制", "rod", shortGUID(gear), "reason", reason)
		if body := castReady158(ctx, "", depthCM, 0); body != nil {
			return body, true
		}
		return nil, false
	}
	// 轮子耐久归零即损坏，但它不像断竿那样从手上消失，客户端仍会发 14/1。
	// 这里必须拦住，否则损坏的轮子还能继续抛投上鱼。
	// 不清 GearGuid：竿确实还在手上，留着它 14/2 才能回出"请先修理"的提示。
	if reelGUID := srv.rigBrokenReelGUID(ctx.Sess.pushCtx(), ctx.Sess.Userid, gear); reelGUID != "" {
		ctx.Log.Info("14/1 被拒：渔线轮已损坏", "rod", shortGUID(gear), "reel", shortGUID(reelGUID))
		if body := castReady158(ctx, "", depthCM, 0); body != nil {
			return body, true
		}
		return nil, false
	}

	// 海杆的 Rod 实例由 4/22 竿线全量树构建；普通纺车杆通常已从库存树
	// 实例化。海杆没有该对象时，客户端的 RUILoadIndicator 会一直显示 0。
	if srv.tunableF("marine_full_sync", 0) != 0 && marineRigType(rigType) {
		ctx.Sess.Push4_22Full()
	}

	body := castReady158(ctx, gear, depthCM, rigType)
	if body == nil {
		return nil, false
	}
	return body, true
}

func hCast(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	// loc/spot：顶层 obj olieodeoaof 第一个非空 str = loc，第二个 u32 存 spot。
	// 协议注释里该槽典型取值是 0..16 的 float（按 U32 承载位型），
	// 不能确认是钓点编号，判鱼不再拿它去对攻略点位；海图区号改用抛竿落点定位。
	guids := deepGUIDs(ctx.Tree)
	gear := ""
	if len(guids) > 0 {
		gear = guids[len(guids)-1]
	}
	st := srv.stateFor(ctx.Sess.Userid)
	if !srv.pendingCatchAllowsCast(ctx, st) {
		return nil, true
	}
	st = srv.selectFishingRod(ctx.Sess.Userid, gear)
	st.mu.Lock()
	if ctx.Tree != nil {
		if loc := findObjStr(ctx.Tree, "olieodeoaof"); loc != "" {
			st.Loc = loc
		}
		if spot := findObjNthU32(ctx.Tree, "olieodeoaof", 1); spot != 0 {
			st.Spot = spot
		}
	}
	castX, castZ, hasCastPos := castLandingPosition(ctx.Tree)
	st.GearGuid = gear
	st.CastPosX, st.CastPosZ, st.HasCastPos = castX, castZ, hasCastPos
	st.mu.Unlock()

	// 记录钓组类型并读取当前钩上饵；路亚使用独立的咬钩节奏。
	rigType := int64(0)
	if row, err := srv.DB.GetItemByGUID(ctx.Sess.pushCtx(), gear); err == nil && row != nil {
		if row.Durability <= 0 {
			// 断竿不可抛投：耐久归零只能修理，17/7 拒取后客户端仍可能直发 14/2。
			st.mu.Lock()
			st.GearGuid = ""
			st.mu.Unlock()
			ctx.Log.Info("14/2 被拒：鱼竿已损坏", "rod", shortGUID(gear))
			ctx.ReplyFailure("System.InvalidOperationException", "鱼竿已损坏，请先修理")
			return nil, true
		}
		rt := srv.rigTypeOfItem(ctx.Sess.pushCtx(), row)
		rigType = rt
		st.mu.Lock()
		st.RigType = rt
		st.mu.Unlock()
	}
	// 水域装备限制：竿或已装渔轮被当前地图禁用时不可抛投
	// （17/7 已拦正常路径，这里兜底已装配好才切图的情况）。
	// 官服语义：装配放行、抛竿拦；用 136 (kOperationResult_WrongRegion)
	// 弹正规文案，避免普通异常名落到「内部错误」。
	if reason := srv.placeRigViolation(ctx.Sess.pushCtx(), ctx.Sess.Userid, gear); reason != "" {
		st.mu.Lock()
		st.GearGuid = ""
		st.mu.Unlock()
		ctx.Log.Info("14/2 被拒：装备限制", "rod", shortGUID(gear), "reason", reason)
		ctx.ReplyOperationResult(136) // kOperationResult_WrongRegion
		return nil, true
	}
	// 14/1 给的是力度上限，客户端被拒后仍可能直发 14/2，这里再拦一道。
	if reelGUID := srv.rigBrokenReelGUID(ctx.Sess.pushCtx(), ctx.Sess.Userid, gear); reelGUID != "" {
		ctx.Log.Info("14/2 被拒：渔线轮已损坏", "rod", shortGUID(gear), "reel", shortGUID(reelGUID))
		ctx.ReplyFailure("System.InvalidOperationException", "渔线轮已损坏，请先修理")
		return nil, true
	}
	// 精力闸门：低于阈值拒绝抛投。官服结果码 84 = kOperationResult_LowEnergyLevel，
	// 形状与上面 136 WrongRegion 拒绝一致，客户端弹 kOperationResult_* 文案。
	if !srv.conditionAllowsCast(ctx.Sess.pushCtx(), ctx.Sess.Userid) {
		ctx.Log.Info("14/2 被拒：精力不足")
		ctx.ReplyOperationResult(operationResultLowEnergyLevel)
		return nil, true
	}
	// 补饵/ensureRig 会写库并推送背包状态，必须在精力闸门放行后执行；
	// 否则一次低精力拒绝也会产生可见的钓组/背包副作用。
	if err := ctx.Sess.prepareBaitForCast(st, gear); err != nil {
		ctx.Log.Warn("抛竿前补饵失败", "err", err)
		return nil, false
	}
	srv.ensureRig(ctx.Sess.pushCtx(), ctx.Sess.Userid, gear)
	// 14/1 给出的是满力度上限；14/2 才携带本次实际落点、动作和出手力度。
	// 这里只做真实请求诊断，不能把本杆力度写回下一杆的上限。
	requestedM := throwDistance(ctx.Tree)
	action := uint32(findObjNthByte(ctx.Tree, "olieodeoaof", 0))
	profile := srv.castDistanceForRig(ctx.Sess.pushCtx(), ctx.Sess.Userid, gear, rigType)
	actionMaxM := profile.DistanceM * castActionFactor(action)
	powerRatio := 0.0
	if actionMaxM > 0 {
		powerRatio = requestedM / actionMaxM
	}
	ctx.Log.Info("实际抛投请求", "rod", shortGUID(gear), "requested_m", math.Round(requestedM*100)/100,
		"max_m", math.Round(profile.DistanceM*100)/100, "action", action,
		"action_factor", math.Round(castActionFactor(action)*1000)/1000,
		"power_ratio", math.Round(powerRatio*1000)/1000)
	// 主线版本会无条件记录当前饵，供路亚咬钩规则和持久化状态使用。
	bait := srv.currentBait(ctx.Sess.pushCtx(), gear)
	st.mu.Lock()
	st.Bait = bait
	// 实际落点距离供后续咬钩快照给资格判定；不能用 castProf.DistanceM
	//（钓具理论最大距离），否则 5m 短抛也会满足「抛投 ≥30m」。
	st.CastM = requestedM
	st.mu.Unlock()
	// 水体距离闸门：玩家离水体太远（抛投够不到）时不派鱼。
	// 只有该地点有水体矩形数据时才判定；没有数据一律放行，避免误伤。
	st.mu.Lock()
	hasPos, px, pz, locNow := st.HasPos, st.PosX, st.PosZ, st.Loc
	st.mu.Unlock()
	if hasPos {
		const waterReachM = 15.0
		nearWater, hasData := waterWithin(locNow, px, pz, waterReachM)
		ctx.Log.Info("抛竿水体诊断", "loc", locNow, "x", px, "z", pz, "hasData", hasData, "near", nearWater)
		if hasData && !nearWater {
			ctx.Log.Info("距水体过远，本杆不派鱼", "loc", locNow, "x", px, "z", pz, "reach", waterReachM)
		} else {
			if !srv.scheduleBite(ctx.Sess, st) {
				ctx.ReplyFailure("System.InvalidOperationException", "请先处理当前鱼获")
				return nil, true
			}
		}
	} else {
		if !srv.scheduleBite(ctx.Sess, st) {
			ctx.ReplyFailure("System.InvalidOperationException", "请先处理当前鱼获")
			return nil, true
		}
	}
	// 抛投已受理（含够不到水面的空杆）：扣本次抛投精力，动作写入总是落库。
	srv.spendEnergy(ctx.Sess.pushCtx(), ctx.Sess.Userid, srv.conditionParams().CastEnergyCost)

	// 响应 800.f0 返回当前钓组 157 对象，包含完整竿树和重建后的 f5。
	cctx := ctx.Sess.pushCtx()
	out := proto.Args(proto.S("61"))
	if tree := srv.buildRigObject(cctx, ctx.Sess.Userid, gear); tree != nil {
		out.Items = append(out.Items, tree)
	}
	body, err := proto.EncodeValue(build800(out, proto.Null(), proto.Null()), ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	// 抛投事件交给成就规则判断一次；2/8 弹窗排在 800 应答之后。
	srv.evaluateAchievements(ctx, achievementEvent{Kind: evCast, DistanceM: requestedM})
	return body, true
}

func findObjStr(root *proto.Value, class string) string {
	if o := findObj(root, class); o != nil {
		for i := range o.Fields {
			if o.Fields[i].Str != nil && *o.Fields[i].Str != "" {
				return *o.Fields[i].Str
			}
		}
	}
	return ""
}

func findObjU32(root *proto.Value, class string) uint64 {
	return findObjNthU32(root, class, 0)
}

// castLandingPosition 读取 14/2 olieodeoaof.f6/f7 的实际抛竿落点。
// 这两列在 wire 上以 U32 承载 F32 位型。
func castLandingPosition(root *proto.Value) (float64, float64, bool) {
	obj := findObj(root, "olieodeoaof")
	if obj == nil {
		return 0, 0, false
	}
	xi := fieldIdxByCS("olieodeoaof", "bopigefkkom")
	zi := fieldIdxByCS("olieodeoaof", "ceicchfhpld")
	if xi < 0 || zi < 0 || xi >= obj.FieldCount() || zi >= obj.FieldCount() {
		return 0, 0, false
	}
	xf, zf := obj.Field(xi), obj.Field(zi)
	if xf.Num == nil || zf.Num == nil {
		return 0, 0, false
	}
	x := float64(math.Float32frombits(uint32(u64Of(xf.Num))))
	z := float64(math.Float32frombits(uint32(u64Of(zf.Num))))
	if math.IsNaN(x) || math.IsNaN(z) || math.IsInf(x, 0) || math.IsInf(z, 0) {
		return 0, 0, false
	}
	return x, z, true
}

// findObjNthByte 取对象第 n 个 BYTE 字段。14/2 的 olieodeoaof.f8 是本次
// 抛投动作枚举，不能和距离、钓点这些 U32 混读。
func findObjNthByte(root *proto.Value, class string, n int) uint64 {
	if o := findObj(root, class); o != nil {
		seen := 0
		for i := range o.Fields {
			if o.Fields[i].Op != "BYTE" || o.Fields[i].Num == nil {
				continue
			}
			if seen == n {
				return u64Of(o.Fields[i].Num)
			}
			seen++
		}
	}
	return 0
}

// findObjNthU32 取对象第 n 个 U32 字段。14/2 中 loc 是首个非空字符串，
// spot 是第二个 U32，不能改用第一个数字字段。
func findObjNthU32(root *proto.Value, class string, n int) uint64 {
	if o := findObj(root, class); o != nil {
		seen := 0
		for i := range o.Fields {
			if o.Fields[i].Op != "U32" || o.Fields[i].Num == nil {
				continue
			}
			if seen == n {
				return u64Of(o.Fields[i].Num)
			}
			seen++
		}
	}
	return 0
}

func findObj(root *proto.Value, class string) *proto.Value {
	if root == nil {
		return nil
	}
	switch root.Kind {
	case proto.KindObj:
		if root.Class == class {
			return root
		}
		for i := range root.Fields {
			if root.Fields[i].Val != nil {
				if o := findObj(root.Fields[i].Val, class); o != nil {
					return o
				}
			}
		}
	case proto.KindArgs, proto.KindValList:
		for _, it := range root.Items {
			if o := findObj(it, class); o != nil {
				return o
			}
		}
	}
	return nil
}

// fireAndForget14 客户端上报族：只记录不回包。
func hFishingStep(ctx *Ctx) ([]byte, bool) {
	// 14/7：obj lobemkloehp f5=anim 位掩码；0x200=MotionlessInWaterColumn（水中静止）
	if ctx.Tree != nil {
		if o := findObj(ctx.Tree, "lobemkloehp"); o != nil && o.FieldCount() > 5 {
			anim := u32Of(o.Field(5).Num)
			settled := anim&rigAnimSettledMask != 0
			var seq uint64
			if o.FieldCount() > 14 && o.Field(14).Num != nil {
				seq = u64Of(o.Field(14).Num)
			}
			strs := treeStrsDeep(o)
			st := ctx.Sess.Srv.fishingStateForRequest(ctx)
			st.mu.Lock()
			st.TickSeen = true
			st.TickSeq = seq
			// 每帧按当前动画位重算，不保留上一帧的就位状态。
			st.RigSettled = settled
			// 从 lobemkloehp 的第一个非空 STR 更新当前水域，避免跨图后沿用上一地点。
			for _, s := range strs {
				if s != "" {
					st.Loc = s
					break
				}
			}
			changed := anim != st.LastAnim
			st.LastAnim = anim
			previousMask := st.LureMotionMask
			st.LureMotionMask = anim
			if rigTypeLureFamily[st.RigType] {
				now := time.Now()
				if anim != previousMask || st.LureLayerAt.IsZero() || now.Sub(st.LureLayerAt) >= 1500*time.Millisecond {
					st.LureLayer = lureLayerFromMotion(anim, st.LureLayer)
					st.LureLayerAt = now
				}
				st.updateLureActionLocked(now, previousMask, anim)
			}
			// 0x200/0x400/0x800/0x1000 是不同水层与姿态的“在水中”位族；岸上不会出现。
			enterWater := false
			needBite := false
			if st.CastActive && !st.InWater && anim&rigAnimInWaterMask != 0 {
				st.InWater = true
				enterWater = true
				if st.NeedBite {
					st.NeedBite = false
					needBite = true
				}
			}
			st.mu.Unlock()
			if enterWater {
				if err := ctx.Sess.Srv.consumeCastBait(ctx.Sess, st); err != nil {
					ctx.Log.Warn("入水窝料扣减失败", "err", err)
					st.mu.Lock()
					st.InWater = false
					st.mu.Unlock()
					return nil, true
				}
			}
			if needBite {
				ctx.Log.Info("钓组在水中，补排咬钩", "rod", shortGUID(st.GearGuid), "f5", fmt.Sprintf("0x%X", anim))
				ctx.Sess.Srv.scheduleBiteForHooked(ctx.Sess, st)
			} else if enterWater {
				ctx.Log.Info("钓组在水中", "rod", shortGUID(st.GearGuid), "f5", fmt.Sprintf("0x%X", anim))
			}
			// 14/7 每帧都会发送，仅在动画位变化时记录日志。
			if changed {
				stateAgeMS := time.Since(st.LureStateAt).Milliseconds()
				ctx.Log.Info("14/7 钓组动画位", "rod", shortGUID(st.GearGuid), "f5", fmt.Sprintf("0x%X", anim),
					"mask", fmt.Sprintf("0x%X", anim&lureMotionStatusMask),
					"action", st.LureAction, "layer", st.LureLayer,
					"layer_cm", st.effectiveBaitDepthCMLocked(time.Now()),
					"speed", st.ReelSetting, "state_age_ms", stateAgeMS)
			}
		}
	}
	return nil, true // handled + nil body = 不回包
}

func hFishingFall(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	st := srv.fishingStateForRequest(ctx)
	st.mu.Lock()
	st.FallSeen = true
	st.FallTs = time.Now()
	rigType, gear := st.RigType, st.GearGuid
	st.mu.Unlock()
	// 打窝器入水就把料倒进落点：它不发 14/7，14/3 就是它的入水信号。
	// 幂等键沿用本次抛竿，客户端重复上报不会重复扣份。
	if spodRigTypes[rigType] {
		if err := srv.consumeCastBait(ctx.Sess, st); err != nil {
			ctx.Log.Warn("打窝器入窝料扣减失败", "rod", shortGUID(gear), "err", err)
		}
	}
	return nil, true
}

func hFishingPull(ctx *Ctx) ([]byte, bool) {
	// 14/9 fishing/pull：纺车杆的"抬杆"（右键）与"小刺"（Ctrl+右键）。
	// 客户端在本地播放杆梢动作，这条命令是 fire-and-forget 通知，不需要回包。
	// 海钓使用 Pull 作为实际刺鱼入口，不一定另发普通钓法的 14/11。
	// ⚠️ 下面的 marineHookConfirm 分支推送的 14/15 实为"鱼离场"（见 fishing.go
	// Push14_15 注释），开启 marine_hook_confirm 会把刚上钩的海钓鱼直接放走，
	// 保持默认关闭。
	srv := ctx.Sess.Srv
	// 搏鱼抬杆/小刺扣精力：14/9 无官服拒绝样本，只扣不拒（搏鱼中拒绝会破坏
	// 鱼战状态一致性）；精力闸门只在 14/2 抛投入口生效。
	srv.spendEnergy(ctx.Sess.pushCtx(), ctx.Sess.Userid, srv.conditionParams().PullEnergyCost)
	st := srv.fishingStateForRequest(ctx)
	st.mu.Lock()
	wasHooked := st.Hooked
	pending, gear, rigType := st.Pending, st.GearGuid, st.RigType
	castActive, resolved := st.CastActive, st.Resolved
	if rigTypeLureFamily[rigType] {
		st.recordLurePulseLocked(time.Now())
	}
	needsConfirmation := marineRigType(rigType) && pending != nil && castActive && !resolved && !pending.HookConfirmed
	reconfirm := marineRigType(rigType) && pending != nil && pending.HookConfirmed && !wasHooked && castActive && !resolved
	nibble := false
	if needsConfirmation {
		nibble = srv.hookNibbleEnabled() && !pending.LuckProtected && pending.BiteQuality == "nibble"
		if !wasHooked && !nibble {
			nibble = hookRetentionFails(pending)
		}
		if !nibble {
			st.Hooked = true
			pending.HookConfirmed = true
		}
	} else if reconfirm {
		st.Hooked = true
	}
	var hookedFish RolledFish
	var announce bool
	if needsConfirmation && !nibble {
		hookedFish, announce = st.takeHookedChatFishLocked()
	}
	st.mu.Unlock()
	if needsConfirmation && nibble {
		finishNibbleHook(ctx, st, pending, gear, castActive, resolved)
		ctx.Log.Info("14/9 海钓嘬饵未中钩，重新排鱼", "rod", shortGUID(gear),
			"fish", shortGUID(pending.FishGUID), "species", pending.Species)
		return nil, true
	}
	if needsConfirmation {
		if err := srv.consumeHookedBait(ctx.Sess, pending, gear); err != nil {
			ctx.Log.Warn("海钓抬竿后鱼饵扣减失败", "fish", pending.FishGUID, "err", err)
		}
		bound := false
		if ctx.Sess.marineHookConfirm() {
			bound = ctx.Sess.Push14_15(gear, pending.FishGUID)
		} else if !ctx.Sess.marineHookItemSync() {
			ctx.Log.Info("14/9 海钓抬竿只走真服 14/11 路径，跳过 14/15/4/22")
		}
		if ctx.Sess.marineHookItemSync() {
			ctx.Sess.Push4_22Hook(false, gear)
		}
		ctx.Log.Info("14/9 海钓抬竿确认", "rod", shortGUID(gear), "fish", shortGUID(pending.FishGUID),
			"rig_type", rigType, "14/15", bound)
	} else if reconfirm {
		ctx.Log.Info("14/9 重复确认已中钩鱼，幂等恢复 Hooked", "rod", shortGUID(gear),
			"fish", shortGUID(pending.FishGUID))
	} else {
		ctx.Log.Info("14/9 抬杆/小刺", "len", len(ctx.Body), "hex", traceHexHead(ctx.Body, 96), "rig_type", rigType)
	}
	if announce {
		srv.notifyFishHooked(ctx.Sess, hookedFish, gear)
	}
	return nil, true
}

func hFishingHooked(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	st := srv.fishingStateForRequest(ctx)
	st.mu.Lock()
	wasHooked := st.Hooked
	pending := st.Pending
	gear := st.GearGuid
	rigType := st.RigType
	castActive := st.CastActive
	resolved := st.Resolved
	if pending != nil && pending.HookConfirmed {
		if !wasHooked {
			st.Hooked = true
		}
		hookedFish, announce := st.takeHookedChatFishLocked()
		st.mu.Unlock()
		if announce {
			srv.notifyFishHooked(ctx.Sess, hookedFish, gear)
		}
		ctx.Log.Info("14/11 重复中钩确认，幂等恢复", "rod", shortGUID(gear),
			"fish", shortGUID(pending.FishGUID), "species", pending.Species)
		return nil, true
	}
	// 钩型保持率只在首次中钩确认时抽样一次；已经 Hooked 的早期确认
	// 或登录恢复不再重抽，但仍需为这尾鱼记录一次确认完成。
	nibble := pending != nil && srv.hookNibbleEnabled() && !pending.LuckProtected && pending.BiteQuality == "nibble"
	if pending != nil && !wasHooked && !nibble && hookRetentionFails(pending) {
		pending.BiteQuality = "nibble"
		nibble = true
	}
	if !nibble {
		st.Hooked = true
		if pending != nil {
			pending.HookConfirmed = true
		}
	}
	hookedFish, announce := st.takeHookedChatFishLocked()
	st.mu.Unlock()
	fishGUID, species := "", ""
	if pending != nil {
		fishGUID, species = pending.FishGUID, pending.Species
	}
	ctx.Log.Info("14/11 中鱼", "rod", shortGUID(gear), "fish", shortGUID(fishGUID), "species", species, "was_hooked", wasHooked)
	if nibble {
		finishNibbleHook(ctx, st, pending, gear, castActive, resolved)
		ctx.Log.Info("14/11 嘬饵未中钩，重新排鱼", "fish", shortGUID(fishGUID), "species", species)
		return nil, true
	}
	if pending != nil {
		if err := srv.consumeHookedBait(ctx.Sess, pending, gear); err != nil {
			ctx.Log.Warn("中鱼后鱼饵扣减失败", "fish", pending.FishGUID, "err", err)
		}
	}

	// 登录恢复/提前起竿：复用完整判鱼链（概率、GM、装备限重、mounted hook），
	// 保留本次 Hooked=true；不能直接 roll 后把竿 guid 当 f23。
	if pending == nil && !resolved && castActive {
		srv.scheduleBiteForHooked(ctx.Sess, st)
	}
	if pending != nil && marineRigType(rigType) && !wasHooked {
		// 官方抓包（2026-11-11）海钓 14/11 后有钩 4/22（串钩=嵌套容器、
		// 单钩=裸钩，挂竿位同四钓法口径）；14/15 仍是"鱼离场"分支，继续默认不推。
		if ctx.Sess.marineHookConfirm() {
			ctx.Sess.Push14_15(gear, pending.FishGUID)
		}
		ctx.Sess.Push4_22Hook(true, gear)
	} else {
		// 挂竿位四钓法通用（官方抓包沉底/浮钓/海钓/路亚中鱼钩 flags 均含 1024）。
		ctx.Sess.Push4_22Hook(true, gear)
	}
	if announce {
		srv.notifyFishHooked(ctx.Sess, hookedFish, gear)
	}
	return nil, true
}

// finishNibbleHook 统一普通与海钓的嘬饵失败收尾：轻耗一份饵，
// 清掉本轮鱼实体并重新跑一次咬口调度。
func finishNibbleHook(ctx *Ctx, st *FishState, pending *RolledFish, gear string, castActive, resolved bool) {
	srv := ctx.Sess.Srv
	if pending != nil {
		pending.HookConfirmed = false
		if err := srv.consumeNibbleBait(ctx.Sess, pending, gear); err != nil {
			ctx.Log.Warn("嘬饵扣饵失败", "fish", pending.FishGUID, "err", err)
		}
		// 14/11 的客户端语义是"鱼实体已进搏鱼态"，服务端单方面否决必须让客户端
		// 知情：推送鱼离场（14/15 = 鱼离场分支，args[1]=鱼 GUID），
		// 客户端走与自然脱钩相同的离场状态机放鱼游走，否则它会一直搏到岸边凭空消失。
		// 客户端尚未接受鱼实体（此前 f0=3/5 拒收）时这是一次无效查找，无副作用。
		if bound := ctx.Sess.Push14_15(gear, pending.FishGUID); bound {
			ctx.Log.Info("嘬饵否决推送鱼离场 14/15", "fish", shortGUID(pending.FishGUID),
				"species", pending.Species)
		}
	}
	st.mu.Lock()
	if st.Pending == pending {
		st.Pending = nil
		st.PushSent = false
		st.Hooked = false
		st.Resolved = false
		st.PendingRepush = 0
		st.LuckRepush = 0
	}
	st.mu.Unlock()
	if castActive && !resolved {
		srv.scheduleBiteState(ctx.Sess, st, false, false)
	}
}

func hFishingLeft(ctx *Ctx) ([]byte, bool) {
	// 14/12 的 f0 是客户端鱼实体状态变化：
	//   f0=3/5：客户端因钓组状态拒收鱼实体，保留当前鱼并延迟重推；
	//   f0=6：鱼实体已经进入当前钓组，等待玩家刺鱼；不能重推或清 Hooked，
	//          否则海钓会每 6 秒重复生成同一尾鱼并锁住右键抬竿。
	//   f0=1/2/8：当前鱼未中钩离场；钓组仍在水中时重新等待下一尾；
	//   其他：保守按本轮没有鱼处理。
	srv := ctx.Sess.Srv
	st := srv.fishingStateForRequest(ctx)
	f0 := uint32(0)
	if ctx.Tree != nil {
		if o := findObj(ctx.Tree, "lhicjpmglbo"); o != nil && o.FieldCount() > 0 {
			f0 = u32Of(o.Field(0).Num)
		}
	}
	st.mu.Lock()
	ctx.Log.Info("14/12 鱼状态", "rod", shortGUID(st.GearGuid), "f0", f0, "hooked", st.Hooked)
	// 14/12 顶层 args 为 [竿 GUID, 鱼 GUID, 状态对象]（实机 TRACE 与 14/14
	// 推送核对）。鱼 GUID 与当前 Pending 不一致的是陈旧/异常帧：它描述的是
	// 别的鱼，不得影响正在搏斗或待入护的这条。
	reported := ""
	if guids := deepGUIDs(ctx.Tree); len(guids) >= 2 {
		reported = guids[1]
	}
	if st.Pending != nil && reported != "" && !strings.EqualFold(reported, st.Pending.FishGUID) {
		ctx.Log.Info("14/12 忽略不一致的鱼 GUID", "f0", f0,
			"reported", shortGUID(reported), "pending", shortGUID(st.Pending.FishGUID))
		st.mu.Unlock()
		return nil, true
	}
	if completedCatchPending(st) {
		// 14/4 已结算、等待 14/5 入护 / 14/6 放生：经验已落账，任何迟到的
		// 状态上报不得清掉这尾鱼，否则结算无处安放（幽灵鱼）。
		ctx.Log.Info("14/12 已结算渔获等待处理，忽略状态上报", "f0", f0)
		st.mu.Unlock()
		return nil, true
	}
	if f0 == 6 {
		st.cancelPendingTimerLocked()
		st.PushSent = true
		st.mu.Unlock()
		return nil, true
	}
	if f0 == 3 || f0 == 5 {
		// 客户端尚未接收鱼实体：保留当前鱼，延迟重推同一尾，
		// 不能把不兼容的钓组状态误当成鱼已离场。
		//
		// 例外：Pending 为空时这是一条离场回执（典型：嘬饵否决推送 14/15 后
		// 客户端放鱼游走的 f0=5）。此时本竿刚重排完，绝不能走下面的拒收路径——
		// 它会 cancel 刚 armed 的咬钩 timer 并把 Resolved 置真，整竿就此哑火。
		if st.Pending == nil {
			st.mu.Unlock()
			ctx.Log.Info("14/12 无在途鱼，按离场回执忽略", "f0", f0, "reported", shortGUID(reported))
			return nil, true
		}
		st.cancelBiteTimerLocked()
		st.PushSent = false
		st.Hooked = false
		st.PendingRepush++
		if st.PendingRepush <= maxPendingRepush && st.CastActive && st.Pending != nil {
			srv.armPendingTimerLocked(ctx.Sess.Userid, st, 6*time.Second)
		} else {
			// 连续拒收说明当前钓组状态与鱼实体不兼容；继续重推只会每 6 秒
			// 重新刷一次鱼。放弃这尾鱼，等玩家重新抛竿。
			ctx.Log.Info("14/12 连续拒收鱼实体，停止重推", "f0", f0, "retries", maxPendingRepush)
			st.Pending = nil
			st.PendingRepush = 0
			st.Resolved = true
		}
		st.mu.Unlock()
		return nil, true
	}
	if (f0 == 1 || f0 == 2 || f0 == 8) && st.Hooked &&
		st.Pending != nil && st.Pending.LuckProtected && st.LuckRepush < 2 {
		st.LuckRepush++
		st.Hooked = false
		st.PushSent = false
		st.PendingRepush = 0
		srv.armPendingTimerLocked(ctx.Sess.Userid, st, 2*time.Second)
		ctx.Log.Info("蓝冠天命保护：忽略钩型脱钩并重推", "f0", f0,
			"fish", shortGUID(st.Pending.FishGUID), "retry", st.LuckRepush)
		st.mu.Unlock()
		return nil, true
	}
	if f0 == 1 || f0 == 2 || f0 == 8 {
		// 实机日志确认：1/2/8 会在鱼未中钩、钓组仍在水中时上报。
		// 它们不代表收竿、断线或钓组离水；
		// 旧逻辑把 CastActive 一并清掉，导致饵仍在水中却永远不再派鱼。
		// 只清理当前鱼，并在同一水中钓组上重新安排下一次判鱼；
		// 路亚脱钩同样重新排鱼，收线途中仍可能再次咬钩。
		//
		// 官方语义（客户端状态机 + 2026-11-06 线上日志实证）：
		// f0=1/2/8 只是客户端鱼 AI 不同的内部计时器/状态机路径
		// （咬钩窗口、试探窗口、钩概率掷骰），**f0 本身不区分鱼
		// 是否上过钩**——搏鱼中跑鱼同样走计时器路径报 f0=2
		// （riv_rb_trout 一轮 14/14 → 14/11 → f0=2，全程无 8/7）。
		// 是否真脱钩以服务端记的 14/11 中鱼确认（st.Hooked）为准：
		//   已中鱼 → 跑鱼等同 f0=7 真脱钩：播报模板 7 并计 escapes；
		//   未中鱼 → 吃饵/试探后游走，官方静默（模板表无此类）。
		st.cancelBiteTimerLocked()
		pending := st.Pending
		alreadyResolved := st.Resolved
		hooked := st.Hooked
		st.Pending = nil
		st.PushSent = false
		st.Hooked = false
		st.Resolved = false
		castActive := st.CastActive
		inWater := st.InWater
		loc, spot := st.Loc, st.Spot
		st.mu.Unlock()
		if hooked && !alreadyResolved {
			srv.bumpFishingCounter(ctx.Sess.Userid, "escapes")
			srv.recordHuntEscape(ctx.Sess.pushCtx(), ctx.Sess.Userid, loc, spot, pending)
			srv.notifyFishEscape(ctx.Sess, pending, "脱钩离开")
			ctx.Log.Info("14/12 已中鱼跑鱼，按真脱钩播报", "f0", f0)
		} else {
			ctx.Log.Info("14/12 鱼未上钩离场，官方静默", "f0", f0, "hooked", hooked)
		}
		// 这一尾鱼的渔轮账到此为止，先落库再排下一尾。
		srv.settleFightReelWearState(srv.dbCtx(), st, ctx.Sess.Userid, "14/12")
		if castActive && inWater {
			srv.scheduleBiteState(ctx.Sess, st, false, false)
		}
		return nil, true
	}
	st.cancelBiteTimerLocked()
	st.cancelPendingTimerLocked()
	pending := st.Pending
	alreadyResolved := st.Resolved
	hooked := st.Hooked
	loc, spot := st.Loc, st.Spot
	st.Resolved = true
	st.Pending = nil
	st.Hooked = false
	st.PushSent = false
	st.CastActive = false
	st.mu.Unlock()
	// 脱钩同样是这一竿的终局：渔轮发热账当场落库。
	srv.settleFightReelWearState(srv.dbCtx(), st, ctx.Sess.Userid, "14/12")
	if hooked && pending != nil && !alreadyResolved {
		srv.bumpFishingCounter(ctx.Sess.Userid, "escapes")
		srv.recordHuntEscape(ctx.Sess.pushCtx(), ctx.Sess.Userid, loc, spot, pending)
		srv.notifyFishEscape(ctx.Sess, pending, "脱钩离开")
		ctx.Log.Info("14/12 未知帧已中鱼跑鱼，按真脱钩播报", "f0", f0)
	} else {
		ctx.Log.Info("14/12 未知帧鱼未上钩，不记脱钩补偿", "f0", f0, "hooked", hooked)
	}
	return nil, true
}

func hFishingGen(ctx *Ctx) ([]byte, bool) { return nil, true } // fire-and-forget
func hFishingLoad(ctx *Ctx) ([]byte, bool) {
	// 14/8 是持续发送的钓组负载/姿态遥测，不是中钩确认。
	// 全量 4/22 会重建鱼实体并打断冲刺、体力阶段；真实报文后没有这条推送。
	if ctx.Sess.Srv.tunableF("fight_load_full_sync", 0) != 0 {
		ctx.Sess.Push4_22Full()
	}
	// 一条负载上报折算一次判定：未锁轮超泄力算打滑磨摩擦片，锁轮超承力按发热曲线烧。
	ctx.Sess.Srv.recordFightReelLoad(ctx)
	return nil, true
}

// hFishingEnd 处理 14/4 收竿结算。
// 结算由 progress_ops.go 的 fishingEndSettle 完成，包括经验、Premium 加成和等级更新。
func hFishingEnd(ctx *Ctx) ([]byte, bool) {
	// 收竿：钓组离水，清掉“在水中/挂起”状态。
	st := ctx.Sess.Srv.fishingStateForRequest(ctx)
	st.mu.Lock()
	st.InWater = false
	st.NeedBite = false
	st.mu.Unlock()
	body, ok := ctx.Sess.Srv.fishingEndSettle(ctx)
	ctx.Log.Info("14/4 收竿", "handled", ok, "bytes", len(body))
	return body, ok
}

// hFishingKeep 14/5 入护。
func hFishingKeep(ctx *Ctx) ([]byte, bool) {
	return ctx.Sess.Srv.fishingKeep(ctx)
}

// hFishingRelease 14/6 放生。
func hFishingRelease(ctx *Ctx) ([]byte, bool) {
	return ctx.Sess.Srv.fishingRelease(ctx)
}

// hFishStats 处理 3/2 钓鱼统计，321 根据 catch_log 实时构造。
func hFishStats(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	tree, err := srv.buildStats321(ctx.Sess.pushCtx(), ctx.Sess.Userid)
	if err != nil {
		ctx.Log.Warn("3/2 统计构造失败", "err", err)
		return nil, false
	}
	f0, f1, f4 := tree.Field(0), tree.Field(1), tree.Field(4)
	ctx.Log.Info("3/2 统计快照", "total", f0.U32(), "rare", f1.U32(), "daily_count", f4.U32())
	body, err := proto.EncodeValue(tree, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// registerSessionHandlers 注册登录时间线 + 钓鱼族。
func registerSessionHandlers(d *Dispatch) {
	d.Register(3, 1, hProfile)
	d.Register(3, 14, hTackleAid)
	d.Register(3, 15, hTackleAid)
	d.Register(3, 2, hFishStats)
	d.Register(4, 1, hItemCount)
	d.Register(4, 4, hItemGUIDs)
	d.Register(4, 5, hItemDetails)
	d.Register(5, 1, hSkillsGet)
	d.Register(11, 1, hShortcutSnapshot)
	d.Register(11, 2, hShortcutBind)
	d.Register(11, 3, hShortcutUnbind)
	d.Register(3, 9, hPositionReport) // 位置同步：记录玩家世界坐标
	d.Register(3, 13, hKitchenEat)    // 3/13 player/kitchen_eat：野外厨房进食，恢复饱食度
	d.Register(3, 30, hRigSyncBackpack)
	d.Register(3, 31, hRigSyncRig)
	d.Register(3, 37, hPlaceBuyout)
	d.Register(1, 12, hPlace)
	d.Register(1, 13, hEnterMap)
	d.Register(1, 14, hTravel)
	d.Register(17, 5, hSetQuivertip)
	// 14/x
	d.Register(14, 1, hCastPrepare)
	d.Register(14, 2, hCast)
	d.Register(14, 3, hFishingFall)
	d.Register(14, 4, hFishingEnd)
	d.Register(14, 5, hFishingKeep)
	d.Register(14, 6, hFishingRelease)
	d.Register(14, 7, hFishingStep)
	d.Register(14, 8, hFishingLoad)
	d.Register(14, 9, hFishingPull)
	d.Register(14, 10, hFishingGen)
	d.Register(14, 11, hFishingHooked)
	d.Register(14, 12, hFishingLeft)
}

// ---- 25 护网条目 ----

// rarityOf25 取鱼种稀有度（ecknbkpeeji.f4，jmpghlofkoc：0 普通 1 稀有 2 超稀有）。
// 结算卡与图鉴按它渲染紫/粉标（icn_rare_* / icn_very_rare_*），缺省为普通。
func (s *Server) rarityOf25(fishKey string) int64 {
	return s.DB.SpeciesRarity(s.dbCtx(), fishKey)
}

func buildKeepnet25(e *store.KeepnetEntry, levelKey string, rarity int64) *proto.Value {
	if e.KeepLevel != "" {
		levelKey = e.KeepLevel
	}
	lengthBits := proto.F32Bits(float32(e.LengthM))
	grade := int(e.Cat)
	var opt byte
	if grade >= 2 {
		opt = 1
	}
	// 游戏时刻 600：gameSec=(u-1438387200)*24
	day, hour, minute := gameClockOf(e.TS)
	var state uint32
	if grade >= 4 && e.NameZh == "" {
		state |= 4194304 // 蓝冠未命名可命名位
	}
	var name *string
	if e.NameZh != "" {
		name = &e.NameZh
	}
	var bait *string
	if e.KeepBait != "" {
		bait = &e.KeepBait
	}
	var species *string
	if e.Species != "" {
		species = &e.Species
	}
	var loc *string
	if levelKey != "" {
		loc = &levelKey
	}
	created := uint64(netTicksNow()) // f22 是版本戳，每次序列化必须严格变大
	updated := uint64(filetimeFromUnixFloat(e.TS))
	displayXP := uint32(float64(e.WeightG) * map[bool]float64{true: 1.3, false: 0.85}[grade == 1])

	return wiregen.MustMakeObj("ecknbkpeeji", map[int]*proto.Value{
		0: proto.Str(species),
		1: proto.U32(uint32(e.WeightG)),
		2: proto.U32(lengthBits),
		3: proto.U32(uint32(grade)),
		4: proto.U32(uint32(rarity)),
		5: proto.U64(updated),
		6: wiregen.MustMakeObj("bncmdhbgahe", map[int]*proto.Value{
			0: proto.U32(uint32(day)),
			1: proto.U32(uint32(hour)),
			2: proto.U32(uint32(minute)),
		}),
		7:  proto.U32(uint32(opt)),
		8:  proto.Str(bait),
		9:  proto.U32(27), // 钩位：浮钓
		10: proto.Str(loc),
		11: proto.Str(name),
		14: proto.U32(displayXP),
		16: proto.Guid(e.EchoGUID),
		18: proto.U32(5), // 鱼护容器 id
		22: proto.U64(created),
		23: proto.U64(updated),
		24: proto.U32(state),
	})
}

// gameClockOf 游戏时刻 (day,hour,min)：现实 1h = 游戏 24h，基线 2015-08-01。
func gameClockOf(unixSec float64) (uint32, uint32, uint32) {
	gameSec := (unixSec - 1438387200) * 24
	totalMin := int64(gameSec) / 60
	if totalMin < 0 {
		totalMin = 0
	}
	day := totalMin / 1440
	hour := (totalMin / 60) % 24
	min := totalMin % 60
	return uint32(day), uint32(hour), uint32(min)
}

// filetimeFromUnixFloat 将 Unix 时间转换为 FILETIME，并保留亚秒精度。
func filetimeFromUnixFloat(ts float64) int64 {
	return int64(ts*10_000_000) + 116444736000000000
}

// netTicksFromUnix .NET Ticks。
func netTicksFromUnix(unixSec float64) int64 {
	return int64(unixSec)*10_000_000 + 621_355_968_000_000_000
}

// premiumBundleItem 捆绑包里的单件物品与数量。
type premiumBundleItem struct {
	ItemID string `json:"item_id"`
	Qty    int64  `json:"qty"`
}

// premiumBundleDef 一个地图的捆绑包档位：客户端按（地图 + 价位）区分，不同地图内容不同。
type premiumBundleDef struct {
	Place string              `json:"place"`
	Price int64               `json:"price"`
	Name  string              `json:"name"`
	Items []premiumBundleItem `json:"items"`
}

// bundleDefsKVKey 保存捆绑包配置的 kv 键；改这个 JSON 即可更新配置，不必重启进程。
const bundleDefsKVKey = "bundle_defs"

// defaultPremiumBundleDefs 内置兜底配置：北海三档。
var defaultPremiumBundleDefs = []premiumBundleDef{
	{
		Place: "level_012_north_sea",
		Price: 17990,
		Name:  "北海-全捆绑包",
		Items: northSeaBundleItems,
	},
	{
		Place: "level_012_north_sea",
		Price: 15990,
		Name:  "北海-扩展捆绑包",
		Items: northSeaBundleItems,
	},
	{
		Place: "level_012_north_sea",
		Price: 3290,
		Name:  "北海-基本捆绑包",
		Items: northSeaBundleItems,
	},
}

// northSeaBundleItems 北海捆绑包附赠物品。
//
// 海鱼饵按 box_sea_baits 的盒内清单逐件发放：该套盒物品在 4.0.25034 客户端里加载不出来，
// 整盒发放会让背包显示"正在下载"。
var northSeaBundleItems = []premiumBundleItem{
	{ItemID: "pilkr_10208_fu88_sbtn", Qty: 1},
	{ItemID: "spin_5140_sbtn", Qty: 1},
	{ItemID: "seafish_book", Qty: 1},
	{ItemID: "fb_lf_makrel", Qty: 1},
	{ItemID: "fb_lf_saida", Qty: 1},
	{ItemID: "fb_lf_seld", Qty: 1},
	{ItemID: "fb_bf_makrel", Qty: 1},
	{ItemID: "fb_bf_saida", Qty: 1},
	{ItemID: "fb_bf_seld", Qty: 1},
	{ItemID: "fb_shrimp", Qty: 1},
	{ItemID: "fb_nereis", Qty: 1},
	{ItemID: "ticket_boat_norw_sea_a_120", Qty: 10},
}

// bundleDefs 读当前生效的捆绑包表：优先 kv.bundle_defs，缺失或解析失败时用内置兜底。
func (s *Server) bundleDefs(ctx context.Context) []premiumBundleDef {
	if s.DB != nil {
		var raw string
		if err := s.DB.SQL.QueryRowContext(ctx,
			`SELECT v FROM kv WHERE k=?`, bundleDefsKVKey).Scan(&raw); err == nil && raw != "" {
			var defs []premiumBundleDef
			if json.Unmarshal([]byte(raw), &defs) == nil && len(defs) > 0 {
				return defs
			}
		}
	}
	return defaultPremiumBundleDefs
}

// grantPremiumBundle 在 3/37 买断成功后，按（地图 + 价位）发放捆绑包附赠物品。
// 物品是直接落库的，客户端不会自动刷新，因此逐件通过 4/22 推送。
func (s *Server) grantPremiumBundle(ctx context.Context, c *Ctx, uid int64, place string, price int64) {
	var hit *premiumBundleDef
	for _, def := range s.bundleDefs(ctx) {
		if def.Price != price {
			continue
		}
		if def.Place != "" && def.Place != place {
			continue
		}
		d := def
		hit = &d
		break
	}
	if hit == nil {
		c.Log.Info("捆绑包未配置，跳过发放", "place", place, "price", price)
		return
	}
	name := hit.Name
	if name == "" {
		name = place
	}
	slot, err := s.DB.NextFreeSlot(ctx, uid, "backpack")
	if err != nil {
		c.Log.Warn("捆绑包发放取空槽失败", "bundle", name, "err", err)
		return
	}
	granted := 0
	for _, it := range hit.Items {
		row, gerr := s.CreateItem(ctx, uid, it.ItemID, "backpack", it.Qty)
		if gerr != nil || row == nil {
			c.Log.Warn("捆绑包物品发放失败", "bundle", name, "item", it.ItemID, "err", gerr)
			continue
		}
		if serr := s.DB.SetItemSlot(ctx, row.GUID, slot); serr != nil {
			c.Log.Warn("捆绑包物品槽位设置失败", "item", it.ItemID, "slot", slot, "err", serr)
		}
		slot++
		granted++
		if c.Sess != nil {
			s.pushItemUpdate(c.Sess.Userid, row)
		}
	}
	c.Log.Info("捆绑包附赠物品已发放", "bundle", name, "place", place, "price", price,
		"granted", granted, "total", len(hit.Items))
}

var _ = context.Background
