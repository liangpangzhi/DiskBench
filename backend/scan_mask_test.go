package main

import (
	"os"
	"testing"
)

func TestNormalizeMaskRangesSortsMergesAndClamps(t *testing.T) {
	got := normalizeMaskRanges([]ScanMaskRange{
		{Offset: 4096, Length: 1024},
		{Offset: 0, Length: 512},
		{Offset: 512, Length: 512},   // 与上一段相邻，合并
		{Offset: 4000, Length: 2000}, // 与 4096 重叠，合并到 6000
	}, 5000)
	// 合并后：[0,1024) 与 [4000,5000)（裁剪到磁盘 5000）
	if len(got) != 2 {
		t.Fatalf("expected 2 merged ranges, got %+v", got)
	}
	if got[0].Offset != 0 || got[0].Length != 1024 {
		t.Errorf("range0 = %+v", got[0])
	}
	if got[1].Offset != 4000 || got[1].Length != 1000 {
		t.Errorf("range1 = %+v", got[1])
	}
}

func TestNormalizeMaskRangesDropsInvalid(t *testing.T) {
	got := normalizeMaskRanges([]ScanMaskRange{
		{Offset: -1, Length: 10},
		{Offset: 0, Length: 0},
		{Offset: 10, Length: 10},
	}, 0)
	if len(got) != 1 || got[0].Offset != 10 || got[0].Length != 10 {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestPlanMaskedReadSkipsAndBounds(t *testing.T) {
	masks := normalizeMaskRanges([]ScanMaskRange{
		{Offset: 100, Length: 50}, // [100,150)
	}, 1_000_000)

	// pos 在屏蔽区内：跳过 50 字节，从 150 开始读。
	skip, start, length := planMaskedRead(masks, 120, 1000, 200)
	if skip != 30 || start != 150 {
		t.Fatalf("skip=%d start=%d", skip, start)
	}
	if length != 200 {
		t.Fatalf("length=%d", length)
	}

	// pos 在屏蔽区前：不跳过，读到屏蔽起点为止。
	skip, start, length = planMaskedRead(masks, 0, 1000, 200)
	if skip != 0 || start != 0 || length != 100 {
		t.Fatalf("before mask: skip=%d start=%d len=%d", skip, start, length)
	}

	// pos 已越过屏蔽区尾部：正常读到 maxLen。
	skip, start, length = planMaskedRead(masks, 200, 1000, 300)
	if skip != 0 || start != 200 || length != 300 {
		t.Fatalf("after mask: skip=%d start=%d len=%d", skip, start, length)
	}
}

func TestPlanMaskedReadFullyMasked(t *testing.T) {
	masks := normalizeMaskRanges([]ScanMaskRange{{Offset: 0, Length: 1000}}, 1000)
	skip, _, length := planMaskedRead(masks, 0, 1000, 500)
	if skip != 1000 || length != 0 {
		t.Fatalf("expected fully masked, skip=%d len=%d", skip, length)
	}
}

func TestMaskedBytesIn(t *testing.T) {
	masks := normalizeMaskRanges([]ScanMaskRange{{Offset: 100, Length: 100}}, 1000)
	if n := maskedBytesIn(masks, 0, 1000); n != 100 {
		t.Fatalf("want 100 got %d", n)
	}
	if n := maskedBytesIn(masks, 0, 50); n != 0 {
		t.Fatalf("want 0 got %d", n)
	}
	if n := maskedBytesIn(masks, 150, 400); n != 50 {
		t.Fatalf("want 50 got %d", n)
	}
}

func TestMaskSaveLoadAndIdentityGuard(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	snap := diskSnapshotFrom(testDisk())

	list, _ := m.loadMaskList(snap)
	if len(list.Ranges) != 0 {
		t.Fatal("expected empty list")
	}
	list.Ranges = []ScanMaskRange{{Offset: 0, Length: 512, Reason: "unreadable"}}
	if err := m.saveMaskList(snap, list, m.maskNow()); err != nil {
		t.Fatal(err)
	}
	loaded, warning := m.loadMaskList(snap)
	if warning != "" || len(loaded.Ranges) != 1 || loaded.Ranges[0].Length != 512 {
		t.Fatalf("roundtrip failed: %+v warning=%q", loaded, warning)
	}

	// 容量变化后旧表应被忽略。
	other := snap
	other.Size = snap.Size + 1
	if l, w := m.loadMaskList(other); len(l.Ranges) != 0 || w == "" {
		t.Fatalf("size change should ignore old list, got %+v warning=%q", l, w)
	}

	// 序列号变化即视为另一块盘：文件名随之改变，不会读到旧表。
	other = snap
	other.Serial = "OTHER-SERIAL"
	if l, _ := m.loadMaskList(other); len(l.Ranges) != 0 {
		t.Fatalf("serial change should not reuse old list, got %+v", l)
	}

	// 防御性：文件内容里的序列号与当前磁盘不符时忽略。
	if err := os.WriteFile(m.maskPath(snap), []byte(`{"diskSerial":"DIFFERENT","diskSize":0,"ranges":[{"offset":0,"length":512}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, w := m.loadMaskList(snap); len(l.Ranges) != 0 || w == "" {
		t.Fatalf("mismatched serial content should be ignored, got %+v warning=%q", l, w)
	}
}

func TestAddAndRemoveMask(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	device := testDisk().Path

	list, err := m.AddMask(device, []ScanMaskRange{{Offset: 0, Length: 512, Reason: "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Ranges) != 1 {
		t.Fatalf("expected 1 range, got %+v", list.Ranges)
	}
	// 重叠区间应合并。
	list, err = m.AddMask(device, []ScanMaskRange{{Offset: 256, Length: 512, Reason: "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Ranges) != 1 || list.Ranges[0].Length != 768 {
		t.Fatalf("expected merged 768, got %+v", list.Ranges)
	}
	// 删除单个区间。
	list, err = m.RemoveMask(device, 0, 768, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Ranges) != 0 {
		t.Fatalf("expected empty after remove, got %+v", list.Ranges)
	}
}

func TestBlocklistAPI(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	s := newTestServer(t, m, apiServerOptions{LocalDev: true, MockDev: true})
	defer s.Close()
	device := testDisk().Path

	// 初始为空。
	status, body := doRequest(s, "GET", "/api/blocklist?device="+device, "", nil)
	if status != 200 {
		t.Fatalf("GET status=%d body=%v", status, body)
	}
	bl, _ := body["blocklist"].(map[string]interface{})
	if bl == nil || len(bl["ranges"].([]interface{})) != 0 {
		t.Fatalf("expected empty blocklist, got %v", body)
	}

	// 添加需要来源头。
	status, _ = doRequest(s, "POST", "/api/blocklist", `{"device":"`+device+`","ranges":[{"offset":0,"length":512}]}`, nil)
	if status != 403 {
		t.Fatalf("POST without header should be 403, got %d", status)
	}
	status, body = doRequest(s, "POST", "/api/blocklist",
		`{"device":"`+device+`","ranges":[{"offset":0,"length":512,"reason":"test"}]}`,
		map[string]string{"X-Requested-With": "DiskBench", "Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("POST status=%d body=%v", status, body)
	}
	bl, _ = body["blocklist"].(map[string]interface{})
	if bl == nil || len(bl["ranges"].([]interface{})) != 1 {
		t.Fatalf("expected 1 range after POST, got %v", body)
	}

	// 清空。
	status, body = doRequest(s, "DELETE", "/api/blocklist?device="+device, "", map[string]string{"X-Requested-With": "DiskBench"})
	if status != 200 {
		t.Fatalf("DELETE status=%d body=%v", status, body)
	}
	bl, _ = body["blocklist"].(map[string]interface{})
	if bl == nil || len(bl["ranges"].([]interface{})) != 0 {
		t.Fatalf("expected empty after DELETE, got %v", body)
	}
}

func TestAddMaskFromJob(t *testing.T) {
	m := newTestManager(t, JobManagerOptions{})
	job := issueTestJob(m)
	job.Disk = DiskSnapshot{Name: testDisk().Name, Model: testDisk().Model, Serial: testDisk().Serial, Size: 1024 * 1024, LogicalSectorSize: 512}
	// 写入一条不可读坏道与一条普通记录。
	saveDisplayRecords(t, m, job,
		BadRange{Offset: 0, Length: 512, Code: "EIO", Outcome: "unreadable"},
		BadRange{Offset: 512, Length: 512, Code: "RECOVERED_ERROR", Outcome: "recovered"},
	)

	list, added, err := m.AddMaskFromJob(job.ID, "unreadable")
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || len(list.Ranges) != 1 || list.Ranges[0].Offset != 0 {
		t.Fatalf("expected 1 unreadable range, got added=%d list=%+v", added, list.Ranges)
	}

	// all 过滤会包含 recovered。
	if _, added, err = m.AddMaskFromJob(job.ID, "all"); err != nil || added != 2 {
		t.Fatalf("all filter: added=%d err=%v", added, err)
	}
}

// 验证真实全盘扫描会跳过屏蔽区：第一个区域被完全屏蔽且不被读取。
func TestRunScanSkipsMaskedRanges(t *testing.T) {
	disk := testDisk()
	disk.Size = 2 * fullScanReadSize
	var readOffsets []int64
	m := newTestManager(t, JobManagerOptions{
		ResolveDisk: func(string) (*Disk, error) { return disk, nil },
		Open: func(string) (ReadHandle, error) {
			return &scanTestHandle{read: func(p []byte, offset int64) (int, error) {
				readOffsets = append(readOffsets, offset)
				return len(p), nil
			}}, nil
		},
		ScanEvidence: func(Disk) ScanEvidence { return ScanEvidence{} },
	})
	if _, err := m.AddMask(disk.Path, []ScanMaskRange{{Offset: 0, Length: fullScanReadSize, Reason: "test"}}); err != nil {
		t.Fatal(err)
	}
	skip := true
	job := &Job{
		ID: newUUID(), Type: "scan", Status: "queued", Device: disk.Path,
		Disk:    DiskSnapshot{Name: disk.Name, Model: disk.Model, Serial: disk.Serial, Size: disk.Size, LogicalSectorSize: 512},
		Options: JobOptions{ScanSkipMasked: &skip},
		Result:  &ScanResult{Version: 2, ScanMode: "full", Coverage: "full", Map: createScanMap(disk.Size, 2, 512)},
	}
	m.jobs[job.ID] = job
	if err := m.runScan(job, "full"); err != nil {
		t.Fatal(err)
	}
	r := job.Result.(*ScanResult)
	if r.Stats.MaskedBytes != fullScanReadSize {
		t.Fatalf("maskedBytes=%d want %d", r.Stats.MaskedBytes, fullScanReadSize)
	}
	if r.Map[0].Status != "masked" {
		t.Fatalf("cell0 status=%q want masked", r.Map[0].Status)
	}
	if !scanCellComplete(r, &r.Map[0]) {
		t.Fatal("masked cell should count as complete")
	}
	for _, offset := range readOffsets {
		if offset < fullScanReadSize {
			t.Fatalf("scan read masked offset %d", offset)
		}
	}
	if len(r.MaskedRanges) != 1 {
		t.Fatalf("result should record masked ranges, got %+v", r.MaskedRanges)
	}
}
