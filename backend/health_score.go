package main

// =============================================================================
// 二次开发新增功能：磁盘健康评分 (Disk Health Score)
// -----------------------------------------------------------------------------
// 基于磁盘的 SMART 关键属性，计算一个 0~100 的健康评分与等级 (A/B/C/D/F)，
// 并给出可读的扣分依据。评分只用于辅助参考，不预测剩余寿命，也不替代备份。
//
// 设计原则：
//   - 未取得的 SMART 信息不按“正常/0”处理，而是明确标记为“信息不足”。
//   - 每个扣分维度都有上限，避免单一指标把分数直接打到 0 造成误解。
//   - 评分纯函数化（输入 Disk，输出 HealthScore），便于单元测试与前端契约测试。
// =============================================================================

import "sort"

// HealthScore 是单块磁盘的健康评分结果。
// 该结构会作为 healthScore 字段附加到 GET /api/disks 的每个 Disk 上。
type HealthScore struct {
	Score      int      `json:"score"`      // 0~100，-1 表示信息不足无法评分
	Grade      string   `json:"grade"`      // A / B / C / D / F / U(未知)
	Label      string   `json:"label"`      // 中文等级说明
	Reasons    []string `json:"reasons"`    // 扣分/加分依据（人类可读）
	Available  bool     `json:"available"`  // 是否具备足够信息完成评分
	SMARTValid bool     `json:"smartValid"` // SMART 整体状态是否通过
}

const healthScoreUnknown = -1

// scoreDiskHealth 根据磁盘 SMART 数据计算健康评分。
// 该函数是纯函数，不修改传入的 Disk，不访问系统，可安全并发调用。
func scoreDiskHealth(disk Disk) HealthScore {
	hs := HealthScore{
		Score:     100,
		Available: false,
		SMARTValid: disk.SmartAvailable && (disk.SmartStatus == "" || equalFold(disk.SmartStatus, "passed")),
	}
	reasons := make([]string, 0, 6)
	deduct := func(points int, reason string) {
		if points <= 0 {
			return
		}
		hs.Score -= points
		reasons = append(reasons, reason)
	}

	// 若没有拿到任何 SMART 信息，明确返回“信息不足”，而不是给出虚假的满分。
	hasAnySmart := disk.SmartAvailable
	hasAnySmart = hasAnySmart || disk.TemperatureC != nil || disk.PowerOnHours != nil
	hasAnySmart = hasAnySmart || disk.ReallocatedSectors != nil || disk.PendingSectors != nil
	hasAnySmart = hasAnySmart || disk.UncorrectableSectors != nil || disk.ReportedUncorrectable != nil
	hasAnySmart = hasAnySmart || disk.NVMeMediaErrors != nil || disk.CriticalWarning != nil
	hasAnySmart = hasAnySmart || disk.PercentageUsed != nil
	if !hasAnySmart {
		hs.Score = healthScoreUnknown
		hs.Grade = "U"
		hs.Label = "信息不足"
		hs.Reasons = []string{"未取得 SMART 信息，无法评分"}
		return hs
	}
	hs.Available = true

	// 1) SMART 整体状态：失败是最严重的信号。
	if disk.SmartAvailable && !equalFold(disk.SmartStatus, "passed") && disk.SmartStatus != "" {
		deduct(45, "SMART 整体状态异常（"+disk.SmartStatus+"）")
	}

	// 2) ATA 关键属性。阶梯扣分，单项封顶。
	//    重映射/待处理扇区出现即代表介质已出问题，权重高于普通磨损指标。
	hs.Score -= deductThreshold(disk.ReallocatedSectors, "重映射扇区(ATA 5)", 8, 32, &reasons)
	hs.Score -= deductThreshold(disk.PendingSectors, "待处理扇区(ATA 197)", 9, 36, &reasons)
	hs.Score -= deductThreshold(disk.UncorrectableSectors, "不可纠正扇区(ATA 198)", 10, 40, &reasons)
	hs.Score -= deductThreshold(disk.ReportedUncorrectable, "报告不可纠正(ATA 187)", 10, 40, &reasons)

	// 3) NVMe 关键属性。
	hs.Score -= deductThreshold(disk.NVMeMediaErrors, "NVMe 媒体错误", 10, 35, &reasons)
	if disk.CriticalWarning != nil && *disk.CriticalWarning != 0 {
		deduct(30, "NVMe critical_warning 告警")
	}

	// 4) 寿命消耗（NVMe percentage used）。
	if disk.PercentageUsed != nil {
		p := *disk.PercentageUsed
		switch {
		case p >= 90:
			deduct(25, "寿命已用 "+formatPercent(p)+"，接近耗尽")
		case p >= 70:
			deduct(15, "寿命已用 "+formatPercent(p))
		case p >= 40:
			deduct(8, "寿命已用 "+formatPercent(p))
		}
	}

	// 5) 高温：仅作为轻微参考扣分（温度属于可变的运行态）。
	if disk.TemperatureC != nil {
		t := *disk.TemperatureC
		switch {
		case t >= 60:
			deduct(8, "当前温度 "+formatInt64(int64(t))+"°C 偏高")
		case t >= 50:
			deduct(4, "当前温度 "+formatInt64(int64(t))+"°C 偏高")
		}
	}

	if hs.Score < 0 {
		hs.Score = 0
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "关键 SMART 属性未见异常")
	}
	sort.Strings(reasons)
	hs.Reasons = reasons
	hs.Grade, hs.Label = gradeForScore(hs.Score)
	return hs
}

// deductThreshold 对单个“计数值”属性做阶梯扣分，返回应扣除的分数。
// pointsPerStep: 每个台阶扣多少；cap: 该维度最多扣多少。
func deductThreshold(value *int64, name string, pointsPerStep, cap int, reasons *[]string) int {
	if value == nil || *value <= 0 {
		return 0
	}
	v := *value
	steps := int64(0)
	switch {
	case v >= 100:
		steps = 4
	case v >= 50:
		steps = 3
	case v >= 10:
		steps = 2
	default:
		steps = 1
	}
	points := int(steps) * pointsPerStep
	if points > cap {
		points = cap
	}
	*reasons = append(*reasons, name+" "+formatInt64(v)+"（-"+formatInt64(int64(points))+"）")
	return points
}

func gradeForScore(score int) (string, string) {
	switch {
	case score >= 90:
		return "A", "优秀"
	case score >= 75:
		return "B", "良好"
	case score >= 60:
		return "C", "注意"
	case score >= 40:
		return "D", "告警"
	default:
		return "F", "严重"
	}
}

// buildHealthScoreMap 为磁盘列表构建“设备路径 → 健康评分”映射。
// 以独立的 healthScores 字段附加到 /api/disks 响应，不改动 disks 数组结构，
// 从而与既有前端与契约测试完全向后兼容（老客户端可忽略该字段）。
func buildHealthScoreMap(disks []Disk) map[string]HealthScore {
	out := make(map[string]HealthScore, len(disks))
	for _, disk := range disks {
		if disk.Path == "" {
			continue
		}
		out[disk.Path] = scoreDiskHealth(disk)
	}
	return out
}

// --- 小工具（避免引入额外依赖） -------------------------------------------

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func formatInt64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := [24]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatPercent(v float64) string {
	whole := int64(v)
	frac := int64((v - float64(whole)) * 10)
	if frac < 0 {
		frac = -frac
	}
	return formatInt64(whole) + "." + formatInt64(frac) + "%"
}
