// Alertmanager Time Interval PoC
//
// Alertmanager의 시간 간격 시스템을 시뮬레이션한다.
// timeinterval/timeinterval.go의 Intervener, TimeInterval, ContainsTime을 재현한다.
//
// 핵심 개념:
//   - 복합 시간 조건: 요일, 시간, 월, 일, 연도
//   - AND(차원 간) / OR(차원 내) 매칭 로직
//   - MuteTimeIntervals vs ActiveTimeIntervals
//   - 타임존 지원
//   - 음수 일(Day) 인덱스
//
// 실행: go run main.go

package main

import (
	"fmt"
	"time"
)

// TimeRange는 하루 내 시간 범위이다 (분 단위).
type TimeRange struct {
	StartMinute int // 0~1439
	EndMinute   int // 1~1440 (exclusive)
}

// Contains는 시간(분)이 범위 내인지 확인한다.
func (tr TimeRange) Contains(minuteOfDay int) bool {
	return minuteOfDay >= tr.StartMinute && minuteOfDay < tr.EndMinute
}

func (tr TimeRange) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d",
		tr.StartMinute/60, tr.StartMinute%60,
		tr.EndMinute/60, tr.EndMinute%60)
}

// InclusiveRange는 포함 범위이다.
type InclusiveRange struct {
	Begin int
	End   int
}

// Contains는 값이 범위 내인지 확인한다.
func (r InclusiveRange) Contains(val int) bool {
	return val >= r.Begin && val <= r.End
}

// TimeInterval은 복합 시간 조건이다.
type TimeInterval struct {
	Name        string
	Times       []TimeRange      // 시간 범위 (OR)
	Weekdays    []InclusiveRange // 요일 범위 (OR), 0=일요일
	DaysOfMonth []InclusiveRange // 일 범위 (OR), 음수 지원
	Months      []InclusiveRange // 월 범위 (OR), 1=1월
	Years       []InclusiveRange // 연도 범위 (OR)
	Location    *time.Location   // 타임존
}

// daysInMonth는 해당 월의 일수를 반환한다.
func daysInMonth(t time.Time) int {
	monthStart := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	monthEnd := monthStart.AddDate(0, 1, 0)
	return int(monthEnd.Sub(monthStart).Hours() / 24)
}

// ContainsTime은 시간이 TimeInterval에 포함되는지 확인한다.
// AND(차원 간) / OR(차원 내) 로직을 사용한다.
func (ti *TimeInterval) ContainsTime(t time.Time) bool {
	// 타임존 변환
	if ti.Location != nil {
		t = t.In(ti.Location)
	}

	// 1. 시간 확인 (OR)
	if len(ti.Times) > 0 {
		minuteOfDay := t.Hour()*60 + t.Minute()
		matched := false
		for _, tr := range ti.Times {
			if tr.Contains(minuteOfDay) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// 2. 요일 확인 (OR)
	if len(ti.Weekdays) > 0 {
		weekday := int(t.Weekday())
		matched := false
		for _, wd := range ti.Weekdays {
			if wd.Contains(weekday) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// 3. 일(Day) 확인 (OR)
	if len(ti.DaysOfMonth) > 0 {
		dim := daysInMonth(t)
		matched := false
		for _, dom := range ti.DaysOfMonth {
			begin, end := dom.Begin, dom.End
			// 음수 인덱스 변환
			if begin < 0 {
				begin = dim + begin + 1
			}
			if end < 0 {
				end = dim + end + 1
			}
			if begin > dim {
				continue
			}
			if t.Day() >= begin && t.Day() <= end {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// 4. 월 확인 (OR)
	if len(ti.Months) > 0 {
		month := int(t.Month())
		matched := false
		for _, mr := range ti.Months {
			if mr.Contains(month) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// 5. 연도 확인 (OR)
	if len(ti.Years) > 0 {
		year := t.Year()
		matched := false
		for _, yr := range ti.Years {
			if yr.Contains(year) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

// Intervener는 시간 간격을 관리하는 컴포넌트이다.
type Intervener struct {
	intervals map[string][]*TimeInterval
}

// NewIntervener는 새 Intervener를 생성한다.
func NewIntervener() *Intervener {
	return &Intervener{intervals: make(map[string][]*TimeInterval)}
}

// Add는 시간 간격을 등록한다.
func (inv *Intervener) Add(name string, intervals ...*TimeInterval) {
	inv.intervals[name] = append(inv.intervals[name], intervals...)
}

// Mutes는 주어진 시간 간격 이름들 중 현재 시간에 매칭되는 것이 있는지 확인한다.
func (inv *Intervener) Mutes(names []string, now time.Time) (bool, []string, error) {
	var matched []string

	for _, name := range names {
		intervals, ok := inv.intervals[name]
		if !ok {
			return false, nil, fmt.Errorf("시간 간격 %q 설정에 없음", name)
		}

		for _, ti := range intervals {
			if ti.ContainsTime(now.UTC()) {
				matched = append(matched, name)
				break
			}
		}
	}

	return len(matched) > 0, matched, nil
}

func main() {
	fmt.Println("=== Alertmanager Time Interval PoC ===")
	fmt.Println()

	intervener := NewIntervener()

	// 업무 시간: 월~금 09:00~18:00 KST
	kst, _ := time.LoadLocation("Asia/Seoul")
	intervener.Add("business-hours", &TimeInterval{
		Name:     "business-hours",
		Times:    []TimeRange{{StartMinute: 540, EndMinute: 1080}}, // 09:00~18:00
		Weekdays: []InclusiveRange{{Begin: 1, End: 5}},             // 월~금
		Location: kst,
	})

	// 업무 외 시간: 야간 + 주말
	intervener.Add("outside-business-hours",
		&TimeInterval{
			Name:     "weeknight-early",
			Times:    []TimeRange{{StartMinute: 0, EndMinute: 540}}, // 00:00~09:00
			Weekdays: []InclusiveRange{{Begin: 1, End: 5}},
			Location: kst,
		},
		&TimeInterval{
			Name:     "weeknight-late",
			Times:    []TimeRange{{StartMinute: 1080, EndMinute: 1440}}, // 18:00~24:00
			Weekdays: []InclusiveRange{{Begin: 1, End: 5}},
			Location: kst,
		},
		&TimeInterval{
			Name:     "weekend",
			Weekdays: []InclusiveRange{{Begin: 0, End: 0}, {Begin: 6, End: 6}}, // 일, 토
			Location: kst,
		},
	)

	// 월말 유지보수: 매월 마지막 3일 22:00~24:00
	intervener.Add("month-end-maintenance", &TimeInterval{
		Name:        "month-end",
		DaysOfMonth: []InclusiveRange{{Begin: -3, End: -1}},
		Times:       []TimeRange{{StartMinute: 1320, EndMinute: 1440}}, // 22:00~24:00
		Location:    kst,
	})

	// 연말: 12월 25~31일
	intervener.Add("year-end", &TimeInterval{
		Name:        "year-end",
		Months:      []InclusiveRange{{Begin: 12, End: 12}},
		DaysOfMonth: []InclusiveRange{{Begin: 25, End: 31}},
	})

	// 테스트 시간들
	testTimes := []struct {
		desc string
		t    time.Time
	}{
		{"평일 오후 2시 (KST)", time.Date(2024, 3, 7, 5, 0, 0, 0, time.UTC)},  // 14:00 KST
		{"평일 밤 11시 (KST)", time.Date(2024, 3, 7, 14, 0, 0, 0, time.UTC)},  // 23:00 KST
		{"토요일 오후 2시 (KST)", time.Date(2024, 3, 9, 5, 0, 0, 0, time.UTC)}, // 토요일
		{"월말 밤 11시 (KST, 3/29)", time.Date(2024, 3, 29, 14, 0, 0, 0, time.UTC)},
		{"크리스마스 (12/25)", time.Date(2024, 12, 25, 12, 0, 0, 0, time.UTC)},
		{"평일 오전 8시 (KST)", time.Date(2024, 3, 7, 23, 0, 0, 0, time.UTC)}, // 08:00 KST
	}

	// 1. 시간 간격 매칭 테스트
	fmt.Println("--- 1. 시간 간격 매칭 테스트 ---")
	intervalNames := []string{"business-hours", "outside-business-hours", "month-end-maintenance", "year-end"}

	for _, tc := range testTimes {
		fmt.Printf("\n시간: %s\n", tc.desc)
		fmt.Printf("  UTC: %s\n", tc.t.Format("2006-01-02 15:04 MST"))
		if kst != nil {
			fmt.Printf("  KST: %s\n", tc.t.In(kst).Format("2006-01-02 15:04 MST"))
		}

		for _, name := range intervalNames {
			muted, _, _ := intervener.Mutes([]string{name}, tc.t)
			status := "❌"
			if muted {
				status = "✅"
			}
			fmt.Printf("  %s %s\n", status, name)
		}
	}
	fmt.Println()

	// 2. MuteTimeIntervals vs ActiveTimeIntervals 시뮬레이션
	fmt.Println("--- 2. MuteTimeIntervals vs ActiveTimeIntervals ---")

	type RouteConfig struct {
		name                string
		muteTimeIntervals   []string
		activeTimeIntervals []string
	}

	routes := []RouteConfig{
		{name: "warning → slack", muteTimeIntervals: []string{"outside-business-hours"}},
		{name: "critical → pager", activeTimeIntervals: []string{"business-hours"}},
	}

	testTime := time.Date(2024, 3, 7, 14, 0, 0, 0, time.UTC) // 23:00 KST (야간)
	fmt.Printf("테스트 시간: %s (KST: %s)\n\n",
		testTime.Format("15:04 UTC"), testTime.In(kst).Format("15:04 KST"))

	for _, route := range routes {
		fmt.Printf("Route: %s\n", route.name)

		shouldMute := false

		// MuteTimeIntervals 확인
		if len(route.muteTimeIntervals) > 0 {
			muted, matched, _ := intervener.Mutes(route.muteTimeIntervals, testTime)
			fmt.Printf("  MuteTimeIntervals %v → muted=%v (matched: %v)\n",
				route.muteTimeIntervals, muted, matched)
			if muted {
				shouldMute = true
			}
		}

		// ActiveTimeIntervals 확인
		if len(route.activeTimeIntervals) > 0 {
			active, matched, _ := intervener.Mutes(route.activeTimeIntervals, testTime)
			fmt.Printf("  ActiveTimeIntervals %v → active=%v (matched: %v)\n",
				route.activeTimeIntervals, active, matched)
			if !active {
				// 활성 시간 밖 → 뮤트
				shouldMute = true
			}
		}

		if shouldMute {
			fmt.Println("  → 결과: 알림 억제됨")
		} else {
			fmt.Println("  → 결과: 알림 전송")
		}
		fmt.Println()
	}

	// 3. 음수 일(Day) 인덱스
	fmt.Println("--- 3. 음수 일(Day) 인덱스 ---")
	monthTests := []time.Time{
		time.Date(2024, 2, 27, 0, 0, 0, 0, time.UTC), // 2월 (29일, 윤년)
		time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 3, 29, 0, 0, 0, 0, time.UTC), // 3월 (31일)
		time.Date(2024, 3, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 3, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC), // 범위 밖
	}

	lastThreeDays := &TimeInterval{
		DaysOfMonth: []InclusiveRange{{Begin: -3, End: -1}},
	}

	fmt.Println("  범위: days_of_month=[-3:-1] (월의 마지막 3일)")
	for _, t := range monthTests {
		dim := daysInMonth(t)
		result := lastThreeDays.ContainsTime(t)
		fmt.Printf("  %s (월 일수: %d) → %v\n", t.Format("2006-01-02"), dim, result)
	}

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. TimeInterval: 차원 간 AND, 차원 내 OR")
	fmt.Println("2. nil 필드 = 와일드카드 (모든 값 허용)")
	fmt.Println("3. Location으로 타임존 변환 후 비교")
	fmt.Println("4. 음수 일 인덱스: 월말 기준 (-1=마지막 날)")
	fmt.Println("5. MuteTimeIntervals: 매칭 시 억제")
	fmt.Println("6. ActiveTimeIntervals: 미매칭 시 억제")
}
