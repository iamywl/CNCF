package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// Jenkins Cron Scheduler (H Hash + Bitmask CronTab) 시뮬레이션
// =============================================================================
//
// Jenkins의 cron 스케줄러는 표준 cron에 'H' 해시 기능을 추가하여
// 동일 시간대에 모든 잡이 몰리는 것을 방지한다.
//
// 핵심 개념:
//   - H 해시: 잡 이름의 해시로 분산된 실행 시간 결정
//   - Bitmask CronTab: 각 필드를 비트마스크로 표현 (효율적 매칭)
//   - 5개 필드: minute(0-59) hour(0-23) dom(1-31) month(1-12) dow(0-7)
//   - H/N: N 간격으로 해시된 오프셋 사용
//
// 실제 코드 참조:
//   - core/src/main/java/hudson/scheduler/CronTab.java
//   - core/src/main/java/hudson/scheduler/Hash.java
// =============================================================================

// --- Hash 함수 (Jenkins H 토큰) ---

// JenkinsHash는 잡 이름에서 해시 값을 생성한다.
func JenkinsHash(jobName string) uint32 {
	h := sha256.Sum256([]byte(jobName))
	return uint32(h[0])<<24 | uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3])
}

// HashMod는 해시를 범위 내로 변환한다.
func HashMod(jobName string, modulus int) int {
	return int(JenkinsHash(jobName) % uint32(modulus))
}

// --- Bitmask CronTab ---

// CronField는 하나의 cron 필드를 비트마스크로 표현한다.
type CronField struct {
	bits    uint64 // 비트마스크 (각 비트가 해당 값을 나타냄)
	min     int
	max     int
	name    string
}

func NewCronField(min, max int, name string) *CronField {
	return &CronField{min: min, max: max, name: name}
}

func (f *CronField) Set(value int) {
	if value >= f.min && value <= f.max {
		f.bits |= 1 << uint(value)
	}
}

func (f *CronField) SetRange(start, end, step int) {
	for i := start; i <= end; i += step {
		f.Set(i)
	}
}

func (f *CronField) SetAll() {
	f.SetRange(f.min, f.max, 1)
}

func (f *CronField) IsSet(value int) bool {
	return f.bits&(1<<uint(value)) != 0
}

func (f *CronField) String() string {
	var values []string
	for i := f.min; i <= f.max; i++ {
		if f.IsSet(i) {
			values = append(values, fmt.Sprintf("%d", i))
		}
	}
	if len(values) == f.max-f.min+1 {
		return "*"
	}
	return strings.Join(values, ",")
}

// --- CronTab ---

type CronTab struct {
	Minute  *CronField
	Hour    *CronField
	DayOfMonth *CronField
	Month   *CronField
	DayOfWeek  *CronField
	OrigExpr   string
	JobName    string
}

func NewCronTab(jobName string) *CronTab {
	return &CronTab{
		Minute:     NewCronField(0, 59, "minute"),
		Hour:       NewCronField(0, 23, "hour"),
		DayOfMonth: NewCronField(1, 31, "dom"),
		Month:      NewCronField(1, 12, "month"),
		DayOfWeek:  NewCronField(0, 6, "dow"),
		JobName:    jobName,
	}
}

// Parse는 cron 표현식을 파싱한다.
func (ct *CronTab) Parse(expr string) error {
	ct.OrigExpr = expr
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("expected 5 fields, got %d", len(fields))
	}

	parsers := []struct {
		field *CronField
		token string
	}{
		{ct.Minute, fields[0]},
		{ct.Hour, fields[1]},
		{ct.DayOfMonth, fields[2]},
		{ct.Month, fields[3]},
		{ct.DayOfWeek, fields[4]},
	}

	for _, p := range parsers {
		if err := ct.parseField(p.field, p.token); err != nil {
			return fmt.Errorf("field %s: %v", p.field.name, err)
		}
	}
	return nil
}

func (ct *CronTab) parseField(field *CronField, token string) error {
	if token == "*" {
		field.SetAll()
		return nil
	}

	// H token
	if strings.HasPrefix(token, "H") {
		return ct.parseH(field, token)
	}

	// */N (every N)
	if strings.HasPrefix(token, "*/") {
		step := 0
		fmt.Sscanf(token, "*/%d", &step)
		if step <= 0 {
			return fmt.Errorf("invalid step: %s", token)
		}
		field.SetRange(field.min, field.max, step)
		return nil
	}

	// N-M (range)
	if strings.Contains(token, "-") {
		var start, end int
		fmt.Sscanf(token, "%d-%d", &start, &end)
		field.SetRange(start, end, 1)
		return nil
	}

	// Comma-separated values
	for _, part := range strings.Split(token, ",") {
		var val int
		fmt.Sscanf(part, "%d", &val)
		field.Set(val)
	}
	return nil
}

func (ct *CronTab) parseH(field *CronField, token string) error {
	hashValue := HashMod(ct.JobName, field.max-field.min+1) + field.min

	if token == "H" {
		field.Set(hashValue)
		return nil
	}

	// H/N
	if strings.HasPrefix(token, "H/") {
		step := 0
		fmt.Sscanf(token, "H/%d", &step)
		if step <= 0 {
			return fmt.Errorf("invalid step in H: %s", token)
		}
		offset := HashMod(ct.JobName, step)
		for i := field.min + offset; i <= field.max; i += step {
			field.Set(i)
		}
		return nil
	}

	// H(N-M)
	if strings.HasPrefix(token, "H(") && strings.HasSuffix(token, ")") {
		inner := token[2 : len(token)-1]
		var start, end int
		fmt.Sscanf(inner, "%d-%d", &start, &end)
		rangeSize := end - start + 1
		val := start + HashMod(ct.JobName, rangeSize)
		field.Set(val)
		return nil
	}

	field.Set(hashValue)
	return nil
}

// Matches는 주어진 시간이 이 스케줄에 맞는지 검사한다.
func (ct *CronTab) Matches(t time.Time) bool {
	return ct.Minute.IsSet(t.Minute()) &&
		ct.Hour.IsSet(t.Hour()) &&
		ct.DayOfMonth.IsSet(t.Day()) &&
		ct.Month.IsSet(int(t.Month())) &&
		ct.DayOfWeek.IsSet(int(t.Weekday()))
}

// NextExecution는 다음 실행 시간을 찾는다.
func (ct *CronTab) NextExecution(from time.Time) time.Time {
	t := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 525960; i++ { // 최대 1년
		if ct.Matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

func (ct *CronTab) String() string {
	return fmt.Sprintf("%-20s -> min=[%s] hour=[%s] dom=[%s] month=[%s] dow=[%s]",
		ct.OrigExpr, ct.Minute, ct.Hour, ct.DayOfMonth, ct.Month, ct.DayOfWeek)
}

func main() {
	fmt.Println("=== Jenkins Cron Scheduler (H Hash + Bitmask) 시뮬레이션 ===")
	fmt.Println()

	// --- H Hash 분산 ---
	fmt.Println("[1] H Hash 분산 (잡 이름별)")
	fmt.Println(strings.Repeat("-", 60))

	jobNames := []string{
		"my-app-build", "api-server-build", "frontend-deploy",
		"nightly-test", "backup-job", "cleanup-task",
		"microservice-a", "microservice-b", "microservice-c",
		"integration-test",
	}

	fmt.Println("  H(분) 분산 (0-59):")
	for _, name := range jobNames {
		minute := HashMod(name, 60)
		bar := strings.Repeat(" ", minute) + "#"
		fmt.Printf("  %-25s -> H=%2d |%s\n", name, minute, bar)
	}
	fmt.Println()

	// --- Cron 표현식 파싱 ---
	fmt.Println("[2] Cron 표현식 파싱")
	fmt.Println(strings.Repeat("-", 60))

	expressions := []struct {
		expr    string
		jobName string
		desc    string
	}{
		{"H * * * *", "my-app-build", "매시간 (해시된 분)"},
		{"H/15 * * * *", "my-app-build", "15분마다 (해시된 오프셋)"},
		{"H H * * *", "nightly-test", "매일 한번 (해시된 시/분)"},
		{"H H(0-7) * * 1-5", "morning-build", "평일 0-7시 (해시)"},
		{"0 2 * * *", "backup-job", "매일 02:00 (고정)"},
		{"*/10 * * * *", "monitoring-check", "10분마다 (고정)"},
		{"H 0 * * 0", "weekly-cleanup", "매주 일요일 (해시된 분)"},
		{"30 8,12,18 * * 1-5", "report-gen", "평일 8:30/12:30/18:30"},
	}

	for _, e := range expressions {
		ct := NewCronTab(e.jobName)
		err := ct.Parse(e.expr)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			continue
		}
		fmt.Printf("  %s (%s)\n", e.desc, e.jobName)
		fmt.Printf("    %s\n", ct)
	}
	fmt.Println()

	// --- 다음 실행 시간 ---
	fmt.Println("[3] 다음 실행 시간 계산")
	fmt.Println(strings.Repeat("-", 60))

	now := time.Now()
	fmt.Printf("  현재 시각: %s\n\n", now.Format("2006-01-02 15:04:05"))

	for _, e := range expressions[:5] {
		ct := NewCronTab(e.jobName)
		ct.Parse(e.expr)
		next := ct.NextExecution(now)
		fmt.Printf("  %-25s (%s)\n    다음: %s\n",
			e.expr, e.desc, next.Format("2006-01-02 15:04"))
	}
	fmt.Println()

	// --- H 분산 효과 비교 ---
	fmt.Println("[4] H 분산 효과: 'H * * * *' vs '0 * * * *'")
	fmt.Println(strings.Repeat("-", 60))

	minuteDistH := make([]int, 60)
	minuteDistFixed := make([]int, 60)

	for _, name := range jobNames {
		// H * * * * -> 분산됨
		ct := NewCronTab(name)
		ct.Parse("H * * * *")
		for m := 0; m < 60; m++ {
			if ct.Minute.IsSet(m) {
				minuteDistH[m]++
			}
		}
		// 0 * * * * -> 0분에 몰림
		minuteDistFixed[0]++
	}

	fmt.Println("  H 사용 (분산):")
	for m := 0; m < 60; m++ {
		if minuteDistH[m] > 0 {
			fmt.Printf("    :%02d %s\n", m, strings.Repeat("#", minuteDistH[m]))
		}
	}
	fmt.Println("  고정 0분 (집중):")
	fmt.Printf("    :00 %s (%d jobs)\n", strings.Repeat("#", minuteDistFixed[0]), minuteDistFixed[0])
	fmt.Println()

	// --- Bitmask 매칭 ---
	fmt.Println("[5] Bitmask 매칭 테스트")
	fmt.Println(strings.Repeat("-", 60))

	ct := NewCronTab("test-job")
	ct.Parse("30 9 * * 1-5")

	testTimes := []time.Time{
		time.Date(2024, 1, 15, 9, 30, 0, 0, time.Local),  // Mon 09:30
		time.Date(2024, 1, 15, 9, 31, 0, 0, time.Local),  // Mon 09:31
		time.Date(2024, 1, 14, 9, 30, 0, 0, time.Local),  // Sun 09:30
		time.Date(2024, 1, 16, 9, 30, 0, 0, time.Local),  // Tue 09:30
		time.Date(2024, 1, 15, 10, 30, 0, 0, time.Local), // Mon 10:30
	}

	for _, t := range testTimes {
		match := ct.Matches(t)
		icon := "X"
		if match {
			icon = "+"
		}
		fmt.Printf("  [%s] %s (%s) -> match=%v\n",
			icon, t.Format("Mon 15:04"), t.Format("2006-01-02"), match)
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
