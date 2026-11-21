package main

// =============================================================================
// 二次开发新增功能：坏道屏蔽 (Bad-sector masking)
// -----------------------------------------------------------------------------
// 把已确认的坏道区间记录为“屏蔽表”，按磁盘身份持久化到本机。启用后，扫描会
// 跳过这些区间，避免反复读取坏道触发磁盘错误恢复（可能长时间卡住甚至扩大损伤）。
//
// 重要边界：
//   - 屏蔽只影响本应用对磁盘的只读访问，不修改磁盘、分区或文件系统，也不做
//     固件级重映射；不会让坏道“消失”。
//   - 屏蔽表按磁盘序列号/容量校验，换盘或容量变化时会拒绝沿用旧记录。
//   - 区间以字节偏移表示，内部归一化为有序、互不重叠的区间集合。
// =============================================================================

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ScanMaskRange 是一条坏道屏蔽区间（字节偏移）。
type ScanMaskRange struct {
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	Reason    string `json:"reason,omitempty"`
	SourceJob string `json:"sourceJob,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// ScanMaskList 是单块磁盘的屏蔽表。
type ScanMaskList struct {
	DiskPath   string          `json:"diskPath"`
	DiskSerial string          `json:"diskSerial,omitempty"`
	DiskModel  string          `json:"diskModel,omitempty"`
	DiskSize   int64           `json:"diskSize"`
	UpdatedAt  string          `json:"updatedAt"`
	Ranges     []ScanMaskRange `json:"ranges"`
}

// maskDiskKey 生成磁盘的稳定文件键：优先序列号，否则用设备路径。
func maskDiskKey(disk DiskSnapshot) string {
	base := strings.TrimSpace(disk.Serial)
	if base == "" {
		base = strings.TrimSpace(disk.Name)
	}
	if base == "" {
		base = "disk"
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (m *JobManager) maskDir() string {
	return filepath.Join(m.dataDir, "blocklists")
}

func (m *JobManager) maskPath(disk DiskSnapshot) string {
	return filepath.Join(m.maskDir(), maskDiskKey(disk)+".json")
}

// loadMaskList 读取某磁盘的屏蔽表。不存在时返回空表（不报错）。
// 若磁盘身份（容量/序列号）与记录不符，返回空表并附带警告，避免误用旧屏蔽。
func (m *JobManager) loadMaskList(disk DiskSnapshot) (ScanMaskList, string) {
	empty := ScanMaskList{DiskPath: disk.Name, DiskSerial: disk.Serial, DiskModel: disk.Model, DiskSize: disk.Size, Ranges: []ScanMaskRange{}}
	path := m.maskPath(disk)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, ""
		}
		return empty, "读取屏蔽表失败，已按无屏蔽处理"
	}
	var list ScanMaskList
	if err := json.Unmarshal(data, &list); err != nil {
		return empty, "屏蔽表损坏，已按无屏蔽处理"
	}
	if list.DiskSize > 0 && disk.Size > 0 && list.DiskSize != disk.Size {
		return empty, "磁盘容量已变化，旧屏蔽表不适用，已忽略"
	}
	if list.DiskSerial != "" && disk.Serial != "" && !strings.EqualFold(list.DiskSerial, disk.Serial) {
		return empty, "磁盘序列号不匹配，旧屏蔽表不适用，已忽略"
	}
	list.Ranges = normalizeMaskRanges(list.Ranges, disk.Size)
	list.DiskPath = disk.Name
	return list, ""
}

func (m *JobManager) saveMaskList(disk DiskSnapshot, list ScanMaskList, now string) error {
	if err := os.MkdirAll(m.maskDir(), 0o700); err != nil {
		return err
	}
	list.DiskPath = disk.Name
	list.DiskSerial = disk.Serial
	list.DiskModel = disk.Model
	list.DiskSize = disk.Size
	list.UpdatedAt = now
	list.Ranges = normalizeMaskRanges(list.Ranges, disk.Size)
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.maskPath(disk) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.maskPath(disk))
}

// normalizeMaskRanges 裁剪到磁盘容量内、排序并合并重叠/相邻区间。
func normalizeMaskRanges(ranges []ScanMaskRange, diskSize int64) []ScanMaskRange {
	out := make([]ScanMaskRange, 0, len(ranges))
	for _, r := range ranges {
		if r.Offset < 0 || r.Length <= 0 {
			continue
		}
		end := r.Offset + r.Length
		if diskSize > 0 && end > diskSize {
			end = diskSize
		}
		if end <= r.Offset {
			continue
		}
		reason := r.Reason
		if reason == "" {
			reason = "manual"
		}
		out = append(out, ScanMaskRange{Offset: r.Offset, Length: end - r.Offset, Reason: reason, SourceJob: r.SourceJob, CreatedAt: r.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	merged := make([]ScanMaskRange, 0, len(out))
	for _, r := range out {
		if n := len(merged); n > 0 {
			last := &merged[n-1]
			if r.Offset <= last.Offset+last.Length { // 重叠或相邻
				end := last.Offset + last.Length
				if r.Offset+r.Length > end {
					end = r.Offset + r.Length
				}
				last.Length = end - last.Offset
				if last.Reason != r.Reason {
					last.Reason = "mixed"
				}
				continue
			}
		}
		merged = append(merged, r)
	}
	return merged
}

// planMaskedRead 规划一次读取，跳过 pos 处的屏蔽区间。
// 返回：
//   skip      —— 从 pos 起需要跳过的屏蔽字节数
//   readStart —— 本次可读起点（skip 之后）
//   readLen   —— 本次可读长度（0 表示 [pos,end) 内已无可读部分）
//
// masks 需为 normalizeMaskRanges 处理过的有序集合。
func planMaskedRead(masks []ScanMaskRange, pos, end, maxLen int64) (skip, readStart, readLen int64) {
	if len(masks) == 0 || pos >= end {
		return 0, pos, minInt64(end-pos, maxLen)
	}
	cur := pos
	// 跳过 cur 处连续的屏蔽区间。
	for {
		moved := false
		for _, m := range masks {
			ms, me := m.Offset, m.Offset+m.Length
			if ms <= cur && cur < me {
				skip += me - cur
				cur = me
				moved = true
				break
			}
		}
		if !moved {
			break
		}
	}
	if cur >= end {
		return skip, cur, 0
	}
	// 找 cur 之后最近的屏蔽起点，作为本次可读上界。
	nextMask := end
	for _, m := range masks {
		if m.Offset > cur && m.Offset < nextMask {
			nextMask = m.Offset
		}
	}
	readStart = cur
	readLen = minInt64(minInt64(end-cur, maxLen), nextMask-cur)
	if readLen < 0 {
		readLen = 0
	}
	return skip, readStart, readLen
}

// maskedBytesIn 返回 [start,end) 内被屏蔽的字节数（用于统计与展示）。
func maskedBytesIn(masks []ScanMaskRange, start, end int64) int64 {
	var total int64
	for _, m := range masks {
		ms, me := m.Offset, m.Offset+m.Length
		s := maxInt64(ms, start)
		e := minInt64(me, end)
		if e > s {
			total += e - s
		}
	}
	return total
}

// --- 磁盘身份与屏蔽表操作（API 层调用） -------------------------------------

func diskSnapshotFrom(disk *Disk) DiskSnapshot {
	return DiskSnapshot{
		Name: disk.Name, Model: disk.Model, Serial: disk.Serial, Size: disk.Size,
		LogicalSectorSize: disk.LogicalSectorSize, PhysicalSectorSize: disk.PhysicalSectorSize,
		Rotational: disk.Rotational, IsSystem: disk.IsSystem,
	}
}

func (m *JobManager) maskSnapshotForDevice(device string) (DiskSnapshot, error) {
	disk, err := m.resolveDisk(device)
	if err != nil {
		return DiskSnapshot{}, err
	}
	if disk == nil {
		return DiskSnapshot{}, newJobError("磁盘不存在或已离线，请刷新后重试", "DISK_NOT_FOUND", 404)
	}
	return diskSnapshotFrom(disk), nil
}

func (m *JobManager) maskNow() string {
	if m.nowFn != nil {
		return m.nowFn().UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// MaskList 返回指定磁盘的屏蔽表。
func (m *JobManager) MaskList(device string) (*ScanMaskList, error) {
	snap, err := m.maskSnapshotForDevice(device)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	list, warning := m.loadMaskList(snap)
	if warning != "" {
		m.logger.Errorf("[diskbench] %s", warning)
	}
	return &list, nil
}

// AddMask 向指定磁盘的屏蔽表追加区间并持久化。
func (m *JobManager) AddMask(device string, ranges []ScanMaskRange) (*ScanMaskList, error) {
	if len(ranges) == 0 {
		return nil, newJobError("请提供至少一个屏蔽区间", "INVALID_ARGUMENT", 400)
	}
	snap, err := m.maskSnapshotForDevice(device)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	list, _ := m.loadMaskList(snap)
	now := m.maskNow()
	for _, r := range ranges {
		if r.CreatedAt == "" {
			r.CreatedAt = now
		}
		list.Ranges = append(list.Ranges, r)
	}
	list.Ranges = normalizeMaskRanges(list.Ranges, snap.Size)
	if err := m.saveMaskList(snap, list, now); err != nil {
		return nil, newJobError("保存屏蔽表失败", "MASK_SAVE_FAILED", 500)
	}
	return &list, nil
}

// RemoveMask 删除匹配的单个区间；未指定区间时清空整表。
func (m *JobManager) RemoveMask(device string, offset, length int64, hasRange bool) (*ScanMaskList, error) {
	snap, err := m.maskSnapshotForDevice(device)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	list, _ := m.loadMaskList(snap)
	now := m.maskNow()
	if hasRange {
		filtered := make([]ScanMaskRange, 0, len(list.Ranges))
		removed := false
		for _, r := range list.Ranges {
			if !removed && r.Offset == offset && r.Length == length {
				removed = true
				continue
			}
			filtered = append(filtered, r)
		}
		if !removed {
			return nil, newJobError("未找到要删除的屏蔽区间", "MASK_RANGE_NOT_FOUND", 404)
		}
		list.Ranges = filtered
	} else {
		list.Ranges = []ScanMaskRange{}
	}
	if err := m.saveMaskList(snap, list, now); err != nil {
		return nil, newJobError("保存屏蔽表失败", "MASK_SAVE_FAILED", 500)
	}
	return &list, nil
}

// AddMaskFromJob 从某个扫描任务的异常记录中提取坏道区间并加入屏蔽表。
// filter: "unreadable" | "severe"（默认）| "all"。
func (m *JobManager) AddMaskFromJob(jobID, filter string) (*ScanMaskList, int, error) {
	if !jobIDPattern.MatchString(jobID) {
		return nil, 0, newJobError("任务 ID 无效", "INVALID_ARGUMENT", 400)
	}
	m.mu.Lock()
	job := cloneJobLocked(m.jobs[jobID])
	m.mu.Unlock()
	if job == nil {
		return nil, 0, newJobError("任务不存在", "JOB_NOT_FOUND", 404)
	}
	if job.Type != "scan" {
		return nil, 0, newJobError("仅扫描任务可以提取坏道", "INVALID_ARGUMENT", 400)
	}
	switch filter {
	case "", "severe", "unreadable", "all":
	default:
		return nil, 0, newJobError("filter 必须是 unreadable、severe 或 all", "INVALID_ARGUMENT", 400)
	}
	if filter == "" {
		filter = "severe"
	}
	records, err := m.collectJobIssues(jobID)
	if err != nil {
		return nil, 0, err
	}
	snap := diskSnapshotFrom(&Disk{
		Name: job.Disk.Name, Model: job.Disk.Model, Serial: job.Disk.Serial, Size: job.Disk.Size,
		LogicalSectorSize: job.Disk.LogicalSectorSize, PhysicalSectorSize: job.Disk.PhysicalSectorSize,
		Rotational: job.Disk.Rotational, IsSystem: job.Disk.IsSystem,
	})
	var picked []ScanMaskRange
	now := m.maskNow()
	for _, r := range records {
		if r.Length <= 0 {
			continue
		}
		keep := false
		switch filter {
		case "all":
			keep = true
		case "unreadable":
			keep = r.Outcome == "unreadable"
		default: // severe
			keep = r.Outcome == "unreadable" || r.Outcome == "device_error"
		}
		if !keep {
			continue
		}
		reason := r.Outcome
		if reason == "" {
			reason = "scan"
		}
		picked = append(picked, ScanMaskRange{Offset: r.Offset, Length: r.Length, Reason: reason, SourceJob: jobID, CreatedAt: now})
	}
	if len(picked) == 0 {
		return nil, 0, newJobError("该扫描记录没有符合条件、可屏蔽的坏道区间", "MASK_NO_RANGES", 409)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	list, _ := m.loadMaskList(snap)
	list.Ranges = append(list.Ranges, picked...)
	list.Ranges = normalizeMaskRanges(list.Ranges, snap.Size)
	if err := m.saveMaskList(snap, list, now); err != nil {
		return nil, 0, newJobError("保存屏蔽表失败", "MASK_SAVE_FAILED", 500)
	}
	return &list, len(picked), nil
}

// collectJobIssues 分页读取某扫描任务的全部异常记录。
func (m *JobManager) collectJobIssues(jobID string) ([]BadRange, error) {
	const pageSize = 200
	var all []BadRange
	var offset int64
	for {
		page, err := m.scanIssueRanges(jobID, offset, pageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Records...)
		if int64(len(page.Records)) == 0 || page.Next >= page.Total {
			break
		}
		offset = page.Next
	}
	return all, nil
}
