package main

import (
	"testing"
)

func TestScoreDiskHealthNoSmartIsUnknown(t *testing.T) {
	disk := Disk{Path: "/dev/sda", Model: "No SMART disk"}
	hs := scoreDiskHealth(disk)
	if hs.Score != healthScoreUnknown || hs.Grade != "U" || hs.Available {
		t.Fatalf("expected unknown health, got %+v", hs)
	}
}

func TestScoreDiskHealthHealthySSD(t *testing.T) {
	disk := Disk{
		Path: "/dev/nvme0n1",
		SmartAvailable: true, SmartStatus: "PASSED",
		ReallocatedSectors: intPtr(0), PendingSectors: intPtr(0),
		PowerOnHours: floatPtr(100),
	}
	hs := scoreDiskHealth(disk)
	if !hs.Available || !hs.SMARTValid || hs.Grade != "A" || hs.Score < 90 {
		t.Fatalf("expected grade A, got %+v", hs)
	}
}

func TestScoreDiskHealthSmartFailed(t *testing.T) {
	disk := Disk{
		Path: "/dev/sdb",
		SmartAvailable: true, SmartStatus: "FAILED",
		PowerOnHours: floatPtr(100),
	}
	hs := scoreDiskHealth(disk)
	if hs.SMARTValid {
		t.Fatalf("expected SMART invalid")
	}
	if hs.Score >= 60 {
		t.Fatalf("expected low score for SMART failure, got %+v", hs)
	}
}

func TestScoreDiskHealthBadSectors(t *testing.T) {
	disk := Disk{
		Path: "/dev/sdc",
		SmartAvailable: true, SmartStatus: "PASSED",
		ReallocatedSectors: intPtr(200),
	}
	hs := scoreDiskHealth(disk)
	if hs.Grade == "A" || hs.Score >= 75 {
		t.Fatalf("expected degraded score for 200 reallocated sectors, got %+v", hs)
	}
	if len(hs.Reasons) == 0 {
		t.Fatalf("expected reasons for deduction")
	}
}

func TestScoreDiskHealthNVMELife(t *testing.T) {
	disk := Disk{
		Path: "/dev/nvme1n1",
		SmartAvailable: true, SmartStatus: "passed",
		NVMeMediaErrors: intPtr(0),
		PercentageUsed: floatPtr(95),
	}
	hs := scoreDiskHealth(disk)
	if hs.Score >= 90 {
		t.Fatalf("expected penalty for 95%% life used, got %+v", hs)
	}
}

func TestBuildHealthScoreMap(t *testing.T) {
	disks := []Disk{
		{Path: "/dev/nvme0n1", SmartAvailable: true, SmartStatus: "PASSED", PowerOnHours: floatPtr(10)},
		{Path: "/dev/sda"},
		{Path: "", SmartAvailable: true, SmartStatus: "PASSED", PowerOnHours: floatPtr(10)},
	}
	m := buildHealthScoreMap(disks)
	if len(m) != 2 {
		t.Fatalf("expected 2 entries (empty path skipped), got %d", len(m))
	}
	if _, ok := m["/dev/nvme0n1"]; !ok {
		t.Fatalf("missing nvme entry")
	}
	if m["/dev/sda"].Grade != "U" {
		t.Fatalf("expected unknown for /dev/sda, got %+v", m["/dev/sda"])
	}
}

func TestGradeForScoreBoundaries(t *testing.T) {
	cases := []struct {
		score int
		grade string
	}{
		{100, "A"}, {90, "A"}, {89, "B"}, {75, "B"},
		{74, "C"}, {60, "C"}, {59, "D"}, {40, "D"}, {39, "F"}, {0, "F"},
	}
	for _, tc := range cases {
		grade, _ := gradeForScore(tc.score)
		if grade != tc.grade {
			t.Errorf("score %d: got %s want %s", tc.score, grade, tc.grade)
		}
	}
}
