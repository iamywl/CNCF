# Alertmanager 시간 간격(Time Interval) 시스템 Deep Dive

## 1. 개요

Time Interval 시스템은 특정 시간대에 알림을 억제(Mute)하거나 활성화(Active)하는 메커니즘이다. `timeinterval/timeinterval.go`에 구현되어 있으며, 요일, 시간, 월, 일, 연도, 타임존을 조합하여 복합적인 시간 조건을 정의할 수 있다.

**사용 시나리오**:
- 업무 시간 외 알림 억제 (야간/주말)
- 유지보수 윈도우 동안 알림 억제
- 특정 기간(연말, 휴일)에만 알림 활성화

## 2. 핵심 구조체

### 2.1 Intervener — 시간 간격 관리자

```go
// timeinterval/timeinterval.go
type Intervener struct {
    intervals map[string][]TimeInterval
}
```

`intervals`는 이름 → TimeInterval 슬라이스 매핑이다. 설정 파일에서 정의한 시간 간격 이름으로 조회한다.

```go
func NewIntervener(ti map[string][]TimeInterval) *Intervener {
    return &Intervener{intervals: ti}
}
```

### 2.2 TimeInterval — 복합 시간 조건

```go
// timeinterval/timeinterval.go
type TimeInterval struct {
    Times       []TimeRange       `yaml:"times,omitempty"`
    Weekdays    []WeekdayRange    `yaml:"weekdays,flow,omitempty"`
    DaysOfMonth []DayOfMonthRange `yaml:"days_of_month,flow,omitempty"`
    Months      []MonthRange      `yaml:"months,flow,omitempty"`
    Years       []YearRange       `yaml:"years,flow,omitempty"`
    Location    *Location         `yaml:"location,flow,omitempty"`
}
```

**매칭 로직**: 모든 필드가 AND로 결합된다. 각 필드 내의 여러 범위는 OR로 결합된다.

```
TimeInterval 매칭 = Times(OR) AND Weekdays(OR) AND DaysOfMonth(OR) AND Months(OR) AND Years(OR)

nil 필드는 해당 차원에서 모든 값을 허용한다.
```

### 2.3 범위 타입들

```go
// timeinterval/timeinterval.go
type InclusiveRange struct {
    Begin int
    End   int
}

type TimeRange struct {
    StartMinute int    // 0~1439 (하루를 분 단위로)
    EndMinute   int    // 1~1440 (exclusive)
}

type WeekdayRange struct {
    InclusiveRange     // 0(일요일) ~ 6(토요일)
}

type DayOfMonthRange struct {
    InclusiveRange     // 1~31 또는 -31~-1 (음수는 월말 기준)
}

type MonthRange struct {
    InclusiveRange     // 1(1월) ~ 12(12월)
}

type YearRange struct {
    InclusiveRange     // 양의 정수
}
```

### 2.4 Location — 타임존

```go
// timeinterval/timeinterval.go
type Location struct {
    *time.Location
}
```

Go의 `time.Location`을 래핑한다. IANA 타임존 데이터베이스 이름(예: `"Asia/Seoul"`, `"Australia/Sydney"`)으로 설정한다.

## 3. Mutes() — 뮤트 판정

```go
// timeinterval/timeinterval.go
func (i *Intervener) Mutes(names []string, now time.Time) (bool, []string, error) {
    var in []string

    for _, name := range names {
        interval, ok := i.intervals[name]
        if !ok {
            return false, nil, fmt.Errorf("time interval %s doesn't exist in config", name)
        }

        for _, ti := range interval {
            if ti.ContainsTime(now.UTC()) {
                in = append(in, name)
                break
            }
        }
    }

    return len(in) > 0, in, nil
}
```

```
반환값:
  bool     — 하나라도 매칭되면 true (뮤트 상태)
  []string — 매칭된 시간 간격 이름들
  error    — 존재하지 않는 시간 간격 이름 요청 시

흐름:
  names = ["maintenance-window", "weekend"]

  "maintenance-window":
    intervals[0].ContainsTime(now) → false
    intervals[1].ContainsTime(now) → true → in에 추가, break
  "weekend":
    intervals[0].ContainsTime(now) → false → 추가 안 함

  결과: (true, ["maintenance-window"], nil)
```

## 4. ContainsTime() — 시간 포함 판정 알고리즘

```go
// timeinterval/timeinterval.go
func (tp TimeInterval) ContainsTime(t time.Time) bool
```

### 4.1 알고리즘 흐름

```
ContainsTime(t time.Time):

    1. 타임존 변환
       if Location != nil:
           t = t.In(Location)

    2. 시간(Times) 확인 — OR
       if Times != nil:
           minuteOfDay = t.Hour()*60 + t.Minute()
           for range in Times:
               if minuteOfDay >= range.StartMinute AND
                  minuteOfDay < range.EndMinute:
                   → 매칭
           매칭 없으면 → return false

    3. 월의 일(DaysOfMonth) 확인 — OR
       if DaysOfMonth != nil:
           for range in DaysOfMonth:
               begin, end = 음수 인덱스 변환(range, daysInMonth)
               if t.Day() >= begin AND t.Day() <= end:
                   → 매칭
           매칭 없으면 → return false

    4. 월(Months) 확인 — OR
       if Months != nil:
           for range in Months:
               if t.Month() >= range.Begin AND
                  t.Month() <= range.End:
                   → 매칭
           매칭 없으면 → return false

    5. 요일(Weekdays) 확인 — OR
       if Weekdays != nil:
           for range in Weekdays:
               if t.Weekday() >= range.Begin AND
                  t.Weekday() <= range.End:
                   → 매칭
           매칭 없으면 → return false

    6. 연도(Years) 확인 — OR
       if Years != nil:
           for range in Years:
               if t.Year() >= range.Begin AND
                  t.Year() <= range.End:
                   → 매칭
           매칭 없으면 → return false

    7. 모든 차원 통과 → return true
```

### 4.2 단락 평가(Short-Circuit)

각 차원을 순차적으로 확인하며, 하나라도 실패하면 즉시 `false`를 반환한다. 이 단락 평가로 불필요한 비교를 최소화한다.

### 4.3 음수 일(Day) 처리

DaysOfMonth에서 음수 값은 월말 기준이다:

```
-1 = 월의 마지막 날 (31일 또는 30일 또는 28/29일)
-2 = 월의 마지막에서 두 번째 날
-3 = 월의 마지막에서 세 번째 날

예: 2월 (28일)에서 -1 → 28, -3 → 26
    3월 (31일)에서 -1 → 31, -3 → 29
```

```go
// 음수 인덱스 → 양수 변환
daysInMonth := daysInMonth(t)

if validDates.Begin < 0 {
    begin = daysInMonth + validDates.Begin + 1
} else {
    begin = validDates.Begin
}
if validDates.End < 0 {
    end = daysInMonth + validDates.End + 1
} else {
    end = validDates.End
}
```

## 5. 헬퍼 함수

### 5.1 daysInMonth()

```go
// timeinterval/timeinterval.go
func daysInMonth(t time.Time) int {
    monthStart := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
    monthEnd := monthStart.AddDate(0, 1, 0)
    diff := monthEnd.Sub(monthStart)
    return int(diff.Hours() / 24)
}
```

다음 달 1일에서 현재 달 1일을 빼서 해당 월의 일수를 계산한다. 윤년 2월도 정확히 처리한다.

### 5.2 clamp()

```go
// timeinterval/timeinterval.go
func clamp(n, min, max int) int {
    if n <= min {
        return min
    }
    if n >= max {
        return max
    }
    return n
}
```

음수 일(Day) 인덱스 변환 시 월 경계를 벗어나지 않도록 값을 제한한다.

## 6. YAML 언마샬링

### 6.1 TimeRange 파싱

```go
// timeinterval/timeinterval.go
type yamlTimeRange struct {
    StartTime string `yaml:"start_time"`
    EndTime   string `yaml:"end_time"`
}

func (tr *TimeRange) UnmarshalYAML(unmarshal func(any) error) error {
    var y yamlTimeRange
    if err := unmarshal(&y); err != nil {
        return err
    }

    if y.EndTime == "" || y.StartTime == "" {
        return errors.New("both start and end times must be provided")
    }

    start, err := parseTime(y.StartTime)  // "HH:MM" → 분
    end, err := parseTime(y.EndTime)

    if start >= end {
        return errors.New("start time cannot be equal or greater than end time")
    }

    tr.StartMinute, tr.EndMinute = start, end
    return nil
}
```

### 6.2 parseTime() — 시간 문자열 파싱

```go
// timeinterval/timeinterval.go
func parseTime(in string) (mins int, err error)
```

```
입력: "09:00" → 출력: 540 (9*60+0)
입력: "17:30" → 출력: 1050 (17*60+30)
입력: "24:00" → 출력: 1440 (하루 끝, 특수 값)

정규식: ^((([01][0-9])|(2[0-3])):[0-5][0-9])$|(^24:00$)

유효 범위: 00:00 ~ 24:00
```

### 6.3 WeekdayRange 파싱

```go
// timeinterval/timeinterval.go
func (r *WeekdayRange) UnmarshalYAML(unmarshal func(any) error) error
```

```
입력 형식:
  "monday"              → Begin=1, End=1 (단일 요일)
  "monday:friday"       → Begin=1, End=5 (범위)
  "sunday:saturday"     → Begin=0, End=6 (전체 주)

요일 매핑:
  "sunday"=0, "monday"=1, "tuesday"=2, "wednesday"=3,
  "thursday"=4, "friday"=5, "saturday"=6

대소문자 무시 (lowercase로 변환 후 비교)
```

### 6.4 DayOfMonthRange 파싱

```go
// timeinterval/timeinterval.go
func (r *DayOfMonthRange) UnmarshalYAML(unmarshal func(any) error) error
```

```
입력 형식:
  "1"       → Begin=1, End=1
  "1:15"    → Begin=1, End=15
  "-3:-1"   → Begin=-3, End=-1 (월말 기준)

제약 조건:
  - 범위: -31 ~ 31 (0 제외)
  - 음수와 양수 혼합 불가: "1:-1" → 에러
  - 28일 기준 논리 순서 확인: "-1:-3" → 에러 (역순)
```

### 6.5 MonthRange 파싱

```go
// timeinterval/timeinterval.go
func (r *MonthRange) UnmarshalYAML(unmarshal func(any) error) error
```

```
입력 형식:
  "january"               → Begin=1, End=1
  "january:march"         → Begin=1, End=3
  "1:3"                   → Begin=1, End=3 (숫자도 가능)
  "January:December"      → Begin=1, End=12 (대소문자 무시)

월 매핑:
  "january"=1, "february"=2, ..., "december"=12
```

### 6.6 YearRange 파싱

```
입력 형식:
  "2024"         → Begin=2024, End=2024
  "2024:2026"    → Begin=2024, End=2026
```

### 6.7 Location 파싱

```go
// timeinterval/timeinterval.go
func (tz *Location) UnmarshalYAML(unmarshal func(any) error) error {
    var str string
    unmarshal(&str)

    loc, err := time.LoadLocation(str)  // IANA 타임존
    if err != nil {
        // Windows 환경에서 ZONEINFO 환경변수 안내
        if runtime.GOOS == "windows" {
            // ...
        }
        return err
    }
    *tz = Location{loc}
    return nil
}
```

```
유효한 Location 예시:
  "Asia/Seoul"
  "America/New_York"
  "Europe/London"
  "Australia/Sydney"
  "UTC"
```

### 6.8 stringableRangeFromString() — 범위 파싱 헬퍼

```go
// timeinterval/timeinterval.go
func stringableRangeFromString(in string, r stringableRange) error
```

```
인터페이스:
  type stringableRange interface {
      setBegin(int)
      setEnd(int)
      memberFromString(string) (int, error)
  }

파싱 로직:
  1. 소문자로 변환
  2. ':' 포함 여부 확인
     - "monday:friday" → split → memberFromString("monday")=1, memberFromString("friday")=5
     - "monday" → memberFromString("monday")=1 → Begin=1, End=1

WeekdayRange, DayOfMonthRange, MonthRange, YearRange 모두 이 헬퍼를 공유한다.
```

## 7. 마샬링 (역직렬화)

### 7.1 TimeRange 마샬링

```go
// timeinterval/timeinterval.go
func (tr TimeRange) MarshalYAML() (any, error) {
    startHour, startMin := tr.StartMinute/60, tr.StartMinute%60
    endHour, endMin := tr.EndMinute/60, tr.EndMinute%60

    return map[string]string{
        "start_time": fmt.Sprintf("%02d:%02d", startHour, startMin),
        "end_time":   fmt.Sprintf("%02d:%02d", endHour, endMin),
    }, nil
}
```

```
540 → "09:00"
1050 → "17:30"
1440 → "24:00"
```

### 7.2 WeekdayRange 마샬링

```
Begin=1, End=5 → "monday:friday"
Begin=1, End=1 → "monday"
```

### 7.3 InclusiveRange 마샬링

```
Begin=1, End=15 → "1:15"
Begin=5, End=5  → "5"
```

## 8. MuteTimeIntervals vs ActiveTimeIntervals

Route 설정에서 두 가지 방식으로 시간 간격을 활용한다:

### 8.1 MuteTimeIntervals

```yaml
route:
  routes:
    - matchers:
        - severity="warning"
      receiver: 'slack'
      mute_time_intervals:
        - 'outside-business-hours'
```

**동작**: 현재 시간이 해당 시간 간격에 포함되면 알림을 **억제**한다.

```
시간 간격 "outside-business-hours"에 현재 시간이 포함:
  → Mutes() 반환: (true, ["outside-business-hours"], nil)
  → 알림 억제됨
```

### 8.2 ActiveTimeIntervals

```yaml
route:
  routes:
    - matchers:
        - severity="critical"
      receiver: 'pager'
      active_time_intervals:
        - 'business-hours'
```

**동작**: 현재 시간이 해당 시간 간격에 포함되지 **않으면** 알림을 억제한다. 즉, 해당 시간 간격 동안에만 알림이 전송된다.

```
시간 간격 "business-hours"에 현재 시간이 포함:
  → 알림 전송
시간 간격 "business-hours"에 현재 시간이 미포함:
  → 알림 억제
```

### 8.3 호출 흐름

```
Notification Pipeline
    │
    ▼
TimeMuteStage.Exec()
    │
    ├── MuteTimeIntervals 확인
    │   └── Intervener.Mutes(muteNames, now)
    │       → true이면 뮤트
    │
    ├── ActiveTimeIntervals 확인
    │   └── Intervener.Mutes(activeNames, now)
    │       → false이면 뮤트 (시간 간격 밖)
    │
    └── 둘 다 통과하면 다음 Stage로
```

```
┌──────────────────────────────────────────────────────┐
│              TimeMute 판정 매트릭스                    │
│                                                      │
│  MuteTimeIntervals:                                  │
│    현재 시간 ∈ 시간 간격 → 뮤트                       │
│    현재 시간 ∉ 시간 간격 → 통과                       │
│                                                      │
│  ActiveTimeIntervals:                                │
│    현재 시간 ∈ 시간 간격 → 통과                       │
│    현재 시간 ∉ 시간 간격 → 뮤트                       │
│                                                      │
│  조합:                                               │
│    MuteTimeIntervals AND ActiveTimeIntervals 모두 설정│
│    → 두 조건 모두 "통과"해야 알림 전송                │
└──────────────────────────────────────────────────────┘
```

## 9. YAML 설정 예시

### 9.1 기본 설정

```yaml
time_intervals:
  - name: 'business-hours'
    time_intervals:
      - weekdays: ['monday:friday']
        times:
          - start_time: '09:00'
            end_time: '18:00'

  - name: 'outside-business-hours'
    time_intervals:
      - weekdays: ['monday:friday']
        times:
          - start_time: '00:00'
            end_time: '09:00'
      - weekdays: ['monday:friday']
        times:
          - start_time: '18:00'
            end_time: '24:00'
      - weekdays: ['saturday:sunday']
```

### 9.2 타임존 사용

```yaml
time_intervals:
  - name: 'korea-business-hours'
    time_intervals:
      - weekdays: ['monday:friday']
        times:
          - start_time: '09:00'
            end_time: '18:00'
        location: 'Asia/Seoul'
```

### 9.3 월말 유지보수

```yaml
time_intervals:
  - name: 'month-end'
    time_intervals:
      - days_of_month: ['-3:-1']
        times:
          - start_time: '22:00'
            end_time: '24:00'
```

### 9.4 연간 특정 기간

```yaml
time_intervals:
  - name: 'year-end'
    time_intervals:
      - months: ['december']
        days_of_month: ['25:31']
```

### 9.5 복합 조건

```yaml
time_intervals:
  - name: 'complex-window'
    time_intervals:
      # 평일 야간 유지보수
      - weekdays: ['tuesday:thursday']
        times:
          - start_time: '02:00'
            end_time: '06:00'
        months: ['january:march']
        years: ['2024:2026']
        location: 'Asia/Seoul'
```

### 9.6 Route에서 활용

```yaml
route:
  receiver: 'default'
  routes:
    # critical은 항상 전송
    - matchers:
        - severity="critical"
      receiver: 'pager'

    # warning은 업무 시간에만 전송
    - matchers:
        - severity="warning"
      receiver: 'slack'
      active_time_intervals:
        - 'business-hours'

    # info는 업무 외 시간에 억제
    - matchers:
        - severity="info"
      receiver: 'email'
      mute_time_intervals:
        - 'outside-business-hours'

time_intervals:
  - name: 'business-hours'
    time_intervals:
      - weekdays: ['monday:friday']
        times:
          - start_time: '09:00'
            end_time: '18:00'
        location: 'Asia/Seoul'

  - name: 'outside-business-hours'
    time_intervals:
      - weekdays: ['saturday:sunday']
      - weekdays: ['monday:friday']
        times:
          - start_time: '00:00'
            end_time: '09:00'
        location: 'Asia/Seoul'
      - weekdays: ['monday:friday']
        times:
          - start_time: '18:00'
            end_time: '24:00'
        location: 'Asia/Seoul'
```

## 10. 유효성 검증 규칙

| 구성 요소 | 제약 조건 | 비고 |
|-----------|----------|------|
| TimeRange | start < end, 0 ≤ start < 1440, 0 < end ≤ 1440 | start_time, end_time 둘 다 필수 |
| WeekdayRange | 0 ≤ Begin ≤ End ≤ 6 | 이름만 허용 (숫자 불가), 대소문자 무시 |
| DayOfMonthRange | -31 ≤ Begin ≤ 31, Begin ≠ 0, End ≠ 0 | 음수/양수 혼합 불가 |
| MonthRange | 1 ≤ Begin ≤ End ≤ 12 | 이름 또는 숫자, 대소문자 무시 |
| YearRange | Begin ≤ End | 양의 정수 |
| Location | 유효한 IANA 타임존 | Windows: ZONEINFO 환경변수 필요 |

## 11. 매칭 예시

### 11.1 업무 시간 매칭

```
TimeInterval:
  Weekdays: [monday:friday]  (1:5)
  Times: [09:00~18:00]       (540~1080)
  Location: Asia/Seoul

테스트 시간: 2024-03-07 목요일 14:30 KST
  1. Location 변환: UTC → KST (이미 KST)
  2. Times: 14*60+30 = 870, 540 ≤ 870 < 1080 → 매칭
  3. Weekdays: Thursday(4), 1 ≤ 4 ≤ 5 → 매칭
  → ContainsTime = true

테스트 시간: 2024-03-09 토요일 14:30 KST
  1. Location 변환
  2. Times: 870, 540 ≤ 870 < 1080 → 매칭
  3. Weekdays: Saturday(6), 1 ≤ 6 ≤ 5 → 불일치!
  → ContainsTime = false (단락 평가)
```

### 11.2 월말 매칭

```
TimeInterval:
  DaysOfMonth: [-3:-1]

테스트 시간: 2024-02-27 (2월, 29일까지 - 윤년)
  daysInMonth = 29
  Begin = 29 + (-3) + 1 = 27
  End = 29 + (-1) + 1 = 29
  Day = 27, 27 ≤ 27 ≤ 29 → 매칭

테스트 시간: 2024-03-29 (3월, 31일까지)
  daysInMonth = 31
  Begin = 31 + (-3) + 1 = 29
  End = 31 + (-1) + 1 = 31
  Day = 29, 29 ≤ 29 ≤ 31 → 매칭

테스트 시간: 2024-03-25
  Begin = 29, End = 31
  Day = 25, 25 < 29 → 불일치
```

### 11.3 복합 조건 매칭

```
TimeInterval:
  Weekdays: [tuesday:thursday] (2:4)
  Times: [02:00~06:00] (120~360)
  Months: [january:march] (1:3)
  Years: [2024:2026]

테스트 시간: 2024-02-14 수요일 03:00 UTC
  1. Times: 3*60 = 180, 120 ≤ 180 < 360 → 매칭
  2. DaysOfMonth: nil → 무조건 매칭
  3. Months: February(2), 1 ≤ 2 ≤ 3 → 매칭
  4. Weekdays: Wednesday(3), 2 ≤ 3 ≤ 4 → 매칭
  5. Years: 2024, 2024 ≤ 2024 ≤ 2026 → 매칭
  → ContainsTime = true

테스트 시간: 2024-04-14 수요일 03:00 UTC
  1. Times: 매칭
  2. DaysOfMonth: nil → 매칭
  3. Months: April(4), 1 ≤ 4 ≤ 3 → 불일치!
  → ContainsTime = false (단락 평가)
```

## 12. 타임존 처리

### 12.1 UTC 기본

`Mutes()`에서 `now.UTC()`로 변환하여 `ContainsTime()`에 전달한다. Location이 설정되지 않으면 UTC 기준으로 비교한다.

### 12.2 Location 설정 시

```
1. now.UTC()로 전달 (Mutes에서)
2. ContainsTime 내부에서 Location으로 변환:
   t = t.In(Location)
3. 변환된 시간으로 각 차원 비교

예:
  now = 2024-03-07T05:30:00Z (UTC)
  Location = "Asia/Seoul" (UTC+9)
  변환: 2024-03-07T14:30:00+09:00
  → 비교 시 14:30 사용
```

### 12.3 Windows 지원

Windows 환경에서는 IANA 타임존 데이터베이스가 기본 포함되지 않으므로, `ZONEINFO` 환경변수로 데이터베이스 경로를 지정해야 한다. 언마샬링 시 친절한 에러 메시지를 제공한다.

## 13. Config 통합

### 13.1 Config에서의 정의

```go
// config/config.go
type Config struct {
    // ...
    MuteTimeIntervals []MuteTimeInterval   // deprecated
    TimeIntervals     []TimeInterval
}

type TimeInterval struct {
    Name          string
    TimeIntervals []timeinterval.TimeInterval
}
```

`MuteTimeIntervals`는 deprecated되었고, `TimeIntervals`로 통합되었다.

### 13.2 Route에서의 참조

```go
// config/config.go
type Route struct {
    // ...
    MuteTimeIntervals   []string    // 뮤트 시간대 이름
    ActiveTimeIntervals []string    // 활성 시간대 이름
}
```

Route는 시간 간격을 이름으로 참조한다. 유효성 검증 시 존재하지 않는 이름을 참조하면 에러가 발생한다.

## 14. Notification Pipeline 연동

### 14.1 TimeMuteStage

```
TimeMuteStage는 Notification Pipeline의 Stage로, MuteTimeIntervals와
ActiveTimeIntervals를 확인하여 알림을 억제한다.

흐름:
  1. Route의 MuteTimeIntervals 가져오기
  2. Intervener.Mutes(muteNames, now) 호출
     → true면 알림 억제
  3. Route의 ActiveTimeIntervals 가져오기
  4. Intervener.Mutes(activeNames, now) 호출
     → false면 알림 억제 (활성 시간 밖)
  5. 두 조건 모두 통과하면 다음 Stage로 전달
```

### 14.2 GroupMarker 연동

```go
// types/types.go
type GroupMarker interface {
    Muted(routeID, groupKey string) ([]string, bool)
    SetMuted(routeID, groupKey string, timeIntervalNames []string)
    DeleteByGroupKey(routeID, groupKey string)
}
```

시간 간격에 의해 뮤트된 그룹은 `GroupMarker`에 기록되어, API에서 뮤트 상태를 조회할 수 있다.

## 15. 설계 고려사항

### 15.1 AND vs OR 설계

```
TimeInterval 내부:    각 차원은 AND (모두 만족해야 매칭)
TimeInterval 배열:    OR (하나라도 매칭되면 OK)
Route의 간격 이름:   OR (하나라도 매칭되면 뮤트)

이 설계 이유:
  - AND: 복합 조건 (평일 + 업무시간 + 특정월)을 자연스럽게 표현
  - OR: 여러 시간대를 합집합으로 결합 (주중 야간 + 주말 전체)
```

### 15.2 nil 필드 = 와일드카드

필드를 설정하지 않으면 해당 차원의 모든 값을 허용한다:
- `Weekdays: nil` → 모든 요일 매칭
- `Times: nil` → 24시간 매칭

이를 통해 "매주 토요일 전체"처럼 일부 차원만 지정하는 것이 가능하다.

### 15.3 음수 일 인덱스

월마다 일수가 다르므로(28~31일), "매월 마지막 3일"을 표현하려면 음수 인덱스가 필수적이다. 양수로는 불가능하다.

```
양수: days_of_month: ['29:31']
  → 2월에는 29일(윤년) 또는 28일만 있어 정확하지 않음

음수: days_of_month: ['-3:-1']
  → 2월: 26~28 (평년) 또는 27~29 (윤년)
  → 3월: 29~31
  → 항상 정확한 "마지막 3일"
```

### 15.4 타임존 안전성

`Mutes()`에서 먼저 UTC로 변환 (`now.UTC()`)하고, `ContainsTime()`에서 Location으로 재변환하는 이중 변환 방식을 사용한다. 이는 호출자가 어떤 타임존의 시간을 전달하든 일관된 결과를 보장한다.
