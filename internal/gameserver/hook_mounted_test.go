package gameserver

import (
	"testing"

	"rf4server/internal/proto"
	"rf4server/internal/wiregen"
)

// hookFlagsOut 读钩对象 hgaijieklek 当前值。
func hookFlagsOut(t *testing.T, hook *proto.Value) uint64 {
	t.Helper()
	idx := fieldIdxByCS("agedjipcnpe", csStateEnum)
	if idx < 0 || idx >= len(hook.Fields) || hook.Fields[idx].Num == nil {
		t.Fatalf("钩对象缺少 %s 字段", csStateEnum)
	}
	return *hook.Fields[idx].Num
}

// markHookMounted 补挂竿位：官方四钓法抓包口径（钩 20992 | 1024 = 22016），
// 缺位时客户端钩实体不进搏鱼态。裸钩与容器嵌套钩两种推送形状都必须补齐。
func TestMarkHookMountedBareHook(t *testing.T) {
	hook := wiregen.MustMakeObj("agedjipcnpe", map[int]*proto.Value{
		fieldIdxByCS("agedjipcnpe", csStateEnum): proto.U32(20992),
	})
	markHookMounted(hook)
	if got := hookFlagsOut(t, hook); got != 22016 {
		t.Fatalf("裸钩 flags=%d，期望 22016（20992|1024）", got)
	}
}

func TestMarkHookMountedNestedRig(t *testing.T) {
	// 路亚/海钓串钩形状：ioombppcnlh 容器 f0 挂 ARGS(60)[钩...]，钩在树深处。
	inner := wiregen.MustMakeObj("agedjipcnpe", map[int]*proto.Value{
		fieldIdxByCS("agedjipcnpe", csStateEnum): proto.U32(20992),
	})
	container := wiregen.MustMakeObj("ioombppcnlh", map[int]*proto.Value{
		0: proto.Args(proto.S("60"), inner),
	})
	markHookMounted(container)
	if got := hookFlagsOut(t, inner); got != 22016 {
		t.Fatalf("嵌套钩 flags=%d，期望 22016", got)
	}
}

func TestMarkHookMountedNilAndNonHook(t *testing.T) {
	markHookMounted(nil)
	rod := wiregen.MustMakeObj("kkmhfbeldpg", nil)
	markHookMounted(rod) // 非钩对象递归无副作用，仅要求不 panic
}
