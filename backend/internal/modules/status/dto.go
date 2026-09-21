package status

import (
	"time"

	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// LabelCount 是通用分组统计项。
type LabelCount struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// FaultBrief 是故障摘要, 用于看板列表与追踪结果。
type FaultBrief struct {
	ID           uint      `json:"id"`
	FaultNo      string    `json:"fault_no"`
	LampCode     string    `json:"lamp_code"`
	RoadName     string    `json:"road_name"`
	FaultType    string    `json:"fault_type"`
	FaultLevel   string    `json:"fault_level"`
	Status       string    `json:"status"`
	ReportedAt   time.Time `json:"reported_at"`
	WaitingHours float64   `json:"waiting_hours"`
	Overdue      bool      `json:"overdue"` // 未闭环且上报超过 OverdueThreshold
}

// LampSummary 路灯台账概览。
type LampSummary struct {
	Total       int64            `json:"total"`
	RoadCount   int64            `json:"road_count"`
	ByRunStatus map[string]int64 `json:"by_run_status"`
}

// FaultSummary 故障概览。
type FaultSummary struct {
	Total         int64            `json:"total"`
	OpenTotal     int64            `json:"open_total"`
	ClosedTotal   int64            `json:"closed_total"`
	ByStatus      map[string]int64 `json:"by_status"`
	TodayReported int64            `json:"today_reported"`
	OverdueTotal  int64            `json:"overdue_total"`
}

// RepairSummary 维修概览。
type RepairSummary struct {
	Total             int64   `json:"total"`
	OngoingTotal      int64   `json:"ongoing_total"`
	FinishedTotal     int64   `json:"finished_total"`
	TodayFinished     int64   `json:"today_finished"`
	AverageDurationHr float64 `json:"average_duration_hours"`
	TotalCost         float64 `json:"total_cost"`
}

// Overview 维修状态总览看板。
type Overview struct {
	Lamp          LampSummary   `json:"lamp"`
	Fault         FaultSummary  `json:"fault"`
	Repair        RepairSummary `json:"repair"`
	FaultByType   []LabelCount  `json:"fault_by_type"`
	FaultByLevel  []LabelCount  `json:"fault_by_level"`
	TopRoads      []LabelCount  `json:"top_roads"`
	RecentFaults  []FaultBrief  `json:"recent_faults"`
	OverdueFaults []FaultBrief  `json:"overdue_faults"`
	OverdueHours  float64       `json:"overdue_threshold_hours"`
	GeneratedAt   time.Time     `json:"generated_at"`
}

// LampStatusRow 是"维修状态查询"列表中的一行: 一盏路灯的当前维修进展。
type LampStatusRow struct {
	LampID        uint       `json:"lamp_id"`
	LampCode      string     `json:"lamp_code"`
	LampName      string     `json:"lamp_name"`
	RoadName      string     `json:"road_name"`
	District      string     `json:"district"`
	LampType      string     `json:"lamp_type"`
	RunStatus     string     `json:"run_status"`
	TotalFaults   int64      `json:"total_fault_count"`
	OpenFaults    int64      `json:"open_fault_count"`
	FaultNo       string     `json:"current_fault_no"`
	FaultType     string     `json:"current_fault_type"`
	FaultLevel    string     `json:"current_fault_level"`
	FaultStatus   string     `json:"current_fault_status"`
	FaultReported *time.Time `json:"current_fault_reported_at"`
	// FaultOverdue 与概览逾期清单同一口径: 当前故障未闭环且上报超过阈值。
	FaultOverdue bool       `json:"current_fault_overdue"`
	RepairNo     string     `json:"latest_repair_no"`
	Repairman    string     `json:"latest_repairman"`
	RepairStatus string     `json:"latest_repair_status"`
	RepairResult string     `json:"latest_repair_result"`
	RepairedAt   *time.Time `json:"latest_repaired_at"`
}

// ExportRow 是维修状态清单导出行, 字段与 LampStatusRow 一一对应,
// 保证导出文件与页面清单、概览数字出自同一查询、同一口径。
type ExportRow struct {
	LampCode      string     `json:"lamp_code" csv:"路灯编号"`
	LampName      string     `json:"lamp_name" csv:"名称"`
	RoadName      string     `csv:"所在道路"`
	RunStatus     string     `json:"-" csv:"运行状态"`
	TotalFaults   int64      `csv:"累计故障数"`
	OpenFaults    int64      `csv:"未闭环故障数"`
	FaultNo       string     `csv:"当前故障单号"`
	FaultStatus   string     `json:"-" csv:"当前故障状态"`
	FaultType     string     `csv:"当前故障类型"`
	FaultReported *time.Time `csv:"上报时间"`
	FaultOverdue  bool       `csv:"是否逾期"`
	RepairNo      string     `csv:"最近维修单号"`
	Repairman     string     `csv:"最近维修人员"`
	RepairStatus  string     `json:"-" csv:"最近维修状态"`
	RepairResult  string     `json:"-" csv:"最近维修结果"`
	RepairedAt    *time.Time `csv:"最近完工时间"`
}

// TimelineEvent 是维修状态追踪中的一个节点。
type TimelineEvent struct {
	Stage     string    `json:"stage"`
	Label     string    `json:"label"`
	Operator  string    `json:"operator"`
	Detail    string    `json:"detail"`
	Timestamp time.Time `json:"timestamp"`
}

// TrackResult 是单条故障(或单盏路灯)的完整处理链路。
type TrackResult struct {
	SearchType    string          `json:"search_type"`
	Lamp          *lamp.Lamp      `json:"lamp,omitempty"`
	Fault         *fault.Fault    `json:"fault,omitempty"`
	Repairs       []repair.Repair `json:"repairs"`
	Timeline      []TimelineEvent `json:"timeline"`
	RelatedFaults []FaultBrief    `json:"related_faults,omitempty"`
}
