package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kafka 메트릭/모니터링 시스템 시뮬레이션
//
// 이 PoC는 Kafka의 메트릭 프레임워크 핵심 개념을 시뮬레이션한다:
//   1. Metrics 레지스트리 (센서와 메트릭의 글로벌 저장소)
//   2. Sensor 시스템 (측정 핸들, 부모-자식 전파)
//   3. 다양한 통계 타입 (Rate, Avg, Max, Min, CumulativeSum)
//   4. MetricConfig와 쿼터 통합
//   5. JmxReporter 시뮬레이션 (MBean 이름 생성, 동적 등록/해제)
//   6. MetricsReporter 플러그인 아키텍처
//   7. RecordingLevel 기반 성능 최적화
//
// 참조 소스:
//   clients/src/main/java/org/apache/kafka/common/metrics/Metrics.java
//   clients/src/main/java/org/apache/kafka/common/metrics/Sensor.java
//   clients/src/main/java/org/apache/kafka/common/metrics/KafkaMetric.java
//   clients/src/main/java/org/apache/kafka/common/metrics/JmxReporter.java
//   clients/src/main/java/org/apache/kafka/common/metrics/MetricsReporter.java
// =============================================================================

// --- Recording Level ---

// RecordingLevel은 메트릭의 기록 레벨을 나타낸다.
// Kafka의 Sensor.RecordingLevel에 대응한다.
type RecordingLevel int

const (
	INFO  RecordingLevel = 0
	DEBUG RecordingLevel = 1
	TRACE RecordingLevel = 2
)

func (r RecordingLevel) String() string {
	switch r {
	case INFO:
		return "INFO"
	case DEBUG:
		return "DEBUG"
	case TRACE:
		return "TRACE"
	default:
		return "UNKNOWN"
	}
}

// ShouldRecord는 현재 설정 레벨에서 이 메트릭을 기록해야 하는지 반환한다.
func (r RecordingLevel) ShouldRecord(configLevel RecordingLevel) bool {
	switch configLevel {
	case INFO:
		return r == INFO
	case DEBUG:
		return r == INFO || r == DEBUG
	case TRACE:
		return true
	default:
		return false
	}
}

// --- MetricName ---

// MetricName은 메트릭의 고유 식별자이다.
// Kafka의 MetricName(name, group, description, tags)에 대응한다.
type MetricName struct {
	Name        string
	Group       string
	Description string
	Tags        map[string]string
}

func (m MetricName) String() string {
	tagParts := []string{}
	// 태그를 정렬하여 일관된 출력
	keys := make([]string, 0, len(m.Tags))
	for k := range m.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m.Tags[k] != "" {
			tagParts = append(tagParts, k+"="+m.Tags[k])
		}
	}
	if len(tagParts) > 0 {
		return fmt.Sprintf("%s{%s}", m.Name, strings.Join(tagParts, ","))
	}
	return m.Name
}

// --- Stat 인터페이스 ---

// Stat은 통계 구현의 인터페이스이다.
// Kafka의 MeasurableStat에 대응한다.
type Stat interface {
	Record(value float64, timeMs int64)
	Value(timeMs int64) float64
	Name() string
}

// --- 통계 구현체들 ---

// RateStat은 시간 윈도우 기반 초당 속도를 계산한다.
type RateStat struct {
	samples      []windowSample
	numSamples   int
	windowSizeMs int64
	currentIdx   int
}

type windowSample struct {
	startTimeMs int64
	value       float64
}

func NewRateStat(numSamples int, windowSizeMs int64) *RateStat {
	now := time.Now().UnixMilli()
	samples := make([]windowSample, numSamples)
	for i := range samples {
		samples[i].startTimeMs = now
	}
	return &RateStat{
		samples:      samples,
		numSamples:   numSamples,
		windowSizeMs: windowSizeMs,
	}
}

func (r *RateStat) Record(value float64, timeMs int64) {
	current := &r.samples[r.currentIdx]
	if timeMs-current.startTimeMs >= r.windowSizeMs {
		r.currentIdx = (r.currentIdx + 1) % r.numSamples
		r.samples[r.currentIdx] = windowSample{startTimeMs: timeMs}
	}
	r.samples[r.currentIdx].value += value
}

func (r *RateStat) Value(timeMs int64) float64 {
	oldestIdx := (r.currentIdx + 1) % r.numSamples
	elapsed := float64(timeMs-r.samples[oldestIdx].startTimeMs) / 1000.0
	if elapsed <= 0 {
		return 0
	}
	var total float64
	for i := range r.samples {
		if i != oldestIdx {
			total += r.samples[i].value
		}
	}
	return total / elapsed
}

func (r *RateStat) Name() string { return "Rate" }

// AvgStat은 평균을 계산한다.
type AvgStat struct {
	sum   float64
	count int64
}

func (a *AvgStat) Record(value float64, _ int64) {
	a.sum += value
	a.count++
}

func (a *AvgStat) Value(_ int64) float64 {
	if a.count == 0 {
		return 0
	}
	return a.sum / float64(a.count)
}

func (a *AvgStat) Name() string { return "Avg" }

// MaxStat은 최대값을 추적한다.
type MaxStat struct {
	max    float64
	hasVal bool
}

func (m *MaxStat) Record(value float64, _ int64) {
	if !m.hasVal || value > m.max {
		m.max = value
		m.hasVal = true
	}
}

func (m *MaxStat) Value(_ int64) float64 {
	if !m.hasVal {
		return math.NaN()
	}
	return m.max
}

func (m *MaxStat) Name() string { return "Max" }

// MinStat은 최소값을 추적한다.
type MinStat struct {
	min    float64
	hasVal bool
}

func (m *MinStat) Record(value float64, _ int64) {
	if !m.hasVal || value < m.min {
		m.min = value
		m.hasVal = true
	}
}

func (m *MinStat) Value(_ int64) float64 {
	if !m.hasVal {
		return math.NaN()
	}
	return m.min
}

func (m *MinStat) Name() string { return "Min" }

// CumulativeSumStat은 전체 누적 합계를 유지한다.
type CumulativeSumStat struct {
	sum float64
}

func (c *CumulativeSumStat) Record(value float64, _ int64) {
	c.sum += value
}

func (c *CumulativeSumStat) Value(_ int64) float64 {
	return c.sum
}

func (c *CumulativeSumStat) Name() string { return "CumulativeSum" }

// --- KafkaMetric ---

// KafkaMetric은 이름과 통계를 결합한 개별 메트릭이다.
// Kafka의 KafkaMetric에 대응한다.
type KafkaMetric struct {
	metricName MetricName
	stat       Stat
	quota      *float64 // nil이면 쿼터 없음
}

func (k *KafkaMetric) MetricValue(timeMs int64) float64 {
	return k.stat.Value(timeMs)
}

// --- Sensor ---

// Sensor는 수치 값을 수신하여 연관된 통계에 전달하는 측정 핸들이다.
// Kafka의 Sensor에 대응한다.
type Sensor struct {
	name           string
	stats          []Stat
	metrics        map[string]*KafkaMetric // metricName.Name → KafkaMetric
	parents        []*Sensor
	recordingLevel RecordingLevel
	lastRecordTime int64
	mu             sync.Mutex
}

// Record는 값을 기록하고 모든 통계에 전달한다.
// 부모 센서에도 자동 전파한다.
func (s *Sensor) Record(value float64, timeMs int64, configLevel RecordingLevel) {
	if !s.recordingLevel.ShouldRecord(configLevel) {
		return
	}

	s.mu.Lock()
	s.lastRecordTime = timeMs
	for _, stat := range s.stats {
		stat.Record(value, timeMs)
	}
	s.mu.Unlock()

	// 부모 센서에 전파
	for _, parent := range s.parents {
		parent.Record(value, timeMs, configLevel)
	}
}

// --- MetricsReporter ---

// MetricsReporter는 메트릭 변경 이벤트를 수신하는 인터페이스이다.
// Kafka의 MetricsReporter에 대응한다.
type MetricsReporter interface {
	Init(metrics []*KafkaMetric)
	MetricChange(metric *KafkaMetric)
	MetricRemoval(metric *KafkaMetric)
	Close()
	Name() string
}

// --- JmxReporter ---

// JmxReporter는 메트릭을 JMX MBean으로 노출하는 리포터를 시뮬레이션한다.
// Kafka의 JmxReporter에 대응한다.
type JmxReporter struct {
	prefix string
	mbeans map[string]map[string]*KafkaMetric // mbeanName → {attrName → metric}
}

func NewJmxReporter(prefix string) *JmxReporter {
	return &JmxReporter{
		prefix: prefix,
		mbeans: make(map[string]map[string]*KafkaMetric),
	}
}

// GetMBeanName은 JMX MBean 이름을 생성한다.
// Kafka의 JmxReporter.getMBeanName()에 대응한다.
// 형식: prefix:type=group,tag1=val1,tag2=val2
func (j *JmxReporter) GetMBeanName(metricName MetricName) string {
	var sb strings.Builder
	sb.WriteString(j.prefix)
	sb.WriteString(":type=")
	sb.WriteString(metricName.Group)

	// 태그를 정렬하여 일관된 MBean 이름 생성
	keys := make([]string, 0, len(metricName.Tags))
	for k := range metricName.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := metricName.Tags[k]
		if k != "" && v != "" {
			sb.WriteString(",")
			sb.WriteString(k)
			sb.WriteString("=")
			sb.WriteString(v)
		}
	}
	return sb.String()
}

func (j *JmxReporter) Init(metrics []*KafkaMetric) {
	for _, m := range metrics {
		j.addAttribute(m)
	}
}

func (j *JmxReporter) MetricChange(metric *KafkaMetric) {
	j.addAttribute(metric)
}

func (j *JmxReporter) MetricRemoval(metric *KafkaMetric) {
	mbeanName := j.GetMBeanName(metric.metricName)
	if attrs, ok := j.mbeans[mbeanName]; ok {
		delete(attrs, metric.metricName.Name)
		if len(attrs) == 0 {
			delete(j.mbeans, mbeanName)
		}
	}
}

func (j *JmxReporter) Close() {
	j.mbeans = make(map[string]map[string]*KafkaMetric)
}

func (j *JmxReporter) Name() string { return "JmxReporter" }

func (j *JmxReporter) addAttribute(metric *KafkaMetric) {
	mbeanName := j.GetMBeanName(metric.metricName)
	if _, ok := j.mbeans[mbeanName]; !ok {
		j.mbeans[mbeanName] = make(map[string]*KafkaMetric)
	}
	j.mbeans[mbeanName][metric.metricName.Name] = metric
}

// PrintMBeans는 현재 등록된 모든 MBean을 출력한다.
func (j *JmxReporter) PrintMBeans() {
	mbeanNames := make([]string, 0, len(j.mbeans))
	for name := range j.mbeans {
		mbeanNames = append(mbeanNames, name)
	}
	sort.Strings(mbeanNames)

	for _, mbeanName := range mbeanNames {
		attrs := j.mbeans[mbeanName]
		fmt.Printf("    MBean: %s\n", mbeanName)
		attrNames := make([]string, 0, len(attrs))
		for name := range attrs {
			attrNames = append(attrNames, name)
		}
		sort.Strings(attrNames)
		now := time.Now().UnixMilli()
		for _, attrName := range attrNames {
			metric := attrs[attrName]
			val := metric.MetricValue(now)
			fmt.Printf("      속성: %-25s = %.2f\n", attrName, val)
		}
	}
}

// --- ConsoleReporter (커스텀 리포터 예시) ---

// ConsoleReporter는 메트릭 변경을 콘솔에 출력하는 커스텀 리포터이다.
type ConsoleReporter struct {
	prefix string
}

func (c *ConsoleReporter) Init(metrics []*KafkaMetric) {
	fmt.Printf("    [%s] 초기 메트릭 %d개 등록됨\n", c.Name(), len(metrics))
}

func (c *ConsoleReporter) MetricChange(metric *KafkaMetric) {
	fmt.Printf("    [%s] 메트릭 변경: %s (%s)\n",
		c.Name(), metric.metricName.String(), metric.metricName.Description)
}

func (c *ConsoleReporter) MetricRemoval(metric *KafkaMetric) {
	fmt.Printf("    [%s] 메트릭 제거: %s\n", c.Name(), metric.metricName.String())
}

func (c *ConsoleReporter) Close() {}

func (c *ConsoleReporter) Name() string { return "ConsoleReporter" }

// --- Metrics (레지스트리) ---

// Metrics는 센서와 메트릭의 글로벌 레지스트리이다.
// Kafka의 Metrics 클래스에 대응한다.
type Metrics struct {
	mu              sync.Mutex
	sensors         map[string]*Sensor
	metrics         map[string]*KafkaMetric // MetricName.String() → KafkaMetric
	reporters       []MetricsReporter
	configLevel     RecordingLevel
	childrenSensors map[string][]string // parentName → childName 목록
}

// NewMetrics는 새 메트릭 레지스트리를 생성한다.
func NewMetrics(reporters []MetricsReporter, configLevel RecordingLevel) *Metrics {
	m := &Metrics{
		sensors:         make(map[string]*Sensor),
		metrics:         make(map[string]*KafkaMetric),
		reporters:       reporters,
		configLevel:     configLevel,
		childrenSensors: make(map[string][]string),
	}

	// 리포터 초기화
	for _, r := range reporters {
		r.Init(nil)
	}

	// 메타 메트릭: 총 메트릭 수
	m.AddSimpleMetric(MetricName{
		Name:        "count",
		Group:       "kafka-metrics-count",
		Description: "총 등록된 메트릭 수",
	}, &CumulativeSumStat{})

	return m
}

// Sensor는 센서를 조회하거나 생성한다.
func (m *Metrics) Sensor(name string, level RecordingLevel, parents ...*Sensor) *Sensor {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sensors[name]; ok {
		return s
	}

	s := &Sensor{
		name:           name,
		stats:          make([]Stat, 0),
		metrics:        make(map[string]*KafkaMetric),
		parents:        parents,
		recordingLevel: level,
		lastRecordTime: time.Now().UnixMilli(),
	}
	m.sensors[name] = s

	// 부모-자식 관계 등록
	for _, parent := range parents {
		m.childrenSensors[parent.name] = append(m.childrenSensors[parent.name], name)
	}

	return s
}

// AddMetricToSensor는 센서에 메트릭(stat)을 추가한다.
func (m *Metrics) AddMetricToSensor(sensor *Sensor, metricName MetricName, stat Stat) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sensor.mu.Lock()
	sensor.stats = append(sensor.stats, stat)
	metric := &KafkaMetric{metricName: metricName, stat: stat}
	sensor.metrics[metricName.Name] = metric
	sensor.mu.Unlock()

	m.metrics[metricName.String()] = metric

	// 리포터에 알림
	for _, r := range m.reporters {
		r.MetricChange(metric)
	}
}

// AddSimpleMetric은 센서 없이 독립 메트릭을 추가한다.
func (m *Metrics) AddSimpleMetric(metricName MetricName, stat Stat) {
	m.mu.Lock()
	defer m.mu.Unlock()

	metric := &KafkaMetric{metricName: metricName, stat: stat}
	m.metrics[metricName.String()] = metric
}

// RemoveSensor는 센서와 관련 메트릭을 제거한다.
func (m *Metrics) RemoveSensor(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sensor, ok := m.sensors[name]
	if !ok {
		return
	}

	// 자식 센서 먼저 제거
	if children, ok := m.childrenSensors[name]; ok {
		for _, childName := range children {
			delete(m.sensors, childName)
		}
		delete(m.childrenSensors, name)
	}

	// 관련 메트릭 제거 및 리포터 알림
	sensor.mu.Lock()
	for _, metric := range sensor.metrics {
		delete(m.metrics, metric.metricName.String())
		for _, r := range m.reporters {
			r.MetricRemoval(metric)
		}
	}
	sensor.mu.Unlock()

	delete(m.sensors, name)
}

// MetricCount는 현재 등록된 메트릭 수를 반환한다.
func (m *Metrics) MetricCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.metrics)
}

// =============================================================================
// 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Kafka 메트릭/모니터링 시스템 시뮬레이션 ===")
	fmt.Println()

	demo1MetricsRegistry()
	demo2SensorParentChild()
	demo3JmxReporter()
	demo4RecordingLevel()
	demo5StatTypes()

	fmt.Println("=== 시뮬레이션 완료 ===")
}

func demo1MetricsRegistry() {
	fmt.Println("--- 데모 1: Metrics 레지스트리 ---")
	fmt.Println()

	consoleReporter := &ConsoleReporter{prefix: "demo"}
	registry := NewMetrics([]MetricsReporter{consoleReporter}, INFO)

	fmt.Println()

	// 센서 생성 및 메트릭 추가
	sensor := registry.Sensor("bytes-sent", INFO)
	registry.AddMetricToSensor(sensor, MetricName{
		Name:        "byte-rate",
		Group:       "producer-metrics",
		Description: "초당 전송 바이트",
	}, NewRateStat(11, 1000))

	registry.AddMetricToSensor(sensor, MetricName{
		Name:        "byte-total",
		Group:       "producer-metrics",
		Description: "총 전송 바이트",
	}, &CumulativeSumStat{})

	fmt.Printf("\n  현재 등록된 메트릭 수: %d\n", registry.MetricCount())
	fmt.Println()

	// 값 기록
	now := time.Now().UnixMilli()
	for i := 0; i < 10; i++ {
		sensor.Record(100, now+int64(i*100), INFO)
	}
	fmt.Printf("  10회 기록 후 메트릭 값:\n")
	for _, m := range sensor.metrics {
		fmt.Printf("    %s (%s): %.2f\n", m.metricName.Name, m.stat.Name(), m.MetricValue(now+1000))
	}
	fmt.Println()
}

func demo2SensorParentChild() {
	fmt.Println("--- 데모 2: Sensor 부모-자식 전파 ---")
	fmt.Println()

	registry := NewMetrics(nil, INFO)

	// 전체 센서 (부모)
	totalSensor := registry.Sensor("bytes-sent-total", INFO)
	totalRate := NewRateStat(11, 1000)
	registry.AddMetricToSensor(totalSensor, MetricName{
		Name:  "byte-rate",
		Group: "total-metrics",
	}, totalRate)

	// 노드별 센서 (자식) - 부모로 totalSensor 지정
	node1Sensor := registry.Sensor("bytes-sent-node-1", INFO, totalSensor)
	node1Rate := NewRateStat(11, 1000)
	registry.AddMetricToSensor(node1Sensor, MetricName{
		Name:  "byte-rate",
		Group: "node-metrics",
		Tags:  map[string]string{"node-id": "1"},
	}, node1Rate)

	node2Sensor := registry.Sensor("bytes-sent-node-2", INFO, totalSensor)
	node2Rate := NewRateStat(11, 1000)
	registry.AddMetricToSensor(node2Sensor, MetricName{
		Name:  "byte-rate",
		Group: "node-metrics",
		Tags:  map[string]string{"node-id": "2"},
	}, node2Rate)

	// 노드별 센서에 값 기록 → 부모 센서에 자동 전파
	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		node1Sensor.Record(100, now+int64(i*200), INFO) // 노드1: 100 bytes × 5
		node2Sensor.Record(200, now+int64(i*200), INFO) // 노드2: 200 bytes × 5
	}

	time.Sleep(10 * time.Millisecond)
	queryTime := time.Now().UnixMilli()

	fmt.Printf("  노드1 Rate: %.2f bytes/sec\n", node1Rate.Value(queryTime))
	fmt.Printf("  노드2 Rate: %.2f bytes/sec\n", node2Rate.Value(queryTime))
	fmt.Printf("  전체 Rate:  %.2f bytes/sec (자동 합산)\n", totalRate.Value(queryTime))
	fmt.Println()
	fmt.Println("  핵심: 자식 센서에만 record()하면 부모 센서에 자동 전파됨")
	fmt.Println()
}

func demo3JmxReporter() {
	fmt.Println("--- 데모 3: JmxReporter - MBean 이름 생성 ---")
	fmt.Println()

	jmx := NewJmxReporter("kafka.server")

	// 다양한 메트릭을 JMX에 등록
	metrics := []*KafkaMetric{
		{
			metricName: MetricName{
				Name:  "byte-rate",
				Group: "Produce",
				Tags:  map[string]string{"user": "alice", "client-id": "producer-1"},
			},
			stat: &CumulativeSumStat{sum: 1024000},
		},
		{
			metricName: MetricName{
				Name:  "request-time",
				Group: "Request",
				Tags:  map[string]string{"user": "bob", "client-id": "consumer-1"},
			},
			stat: &AvgStat{sum: 500, count: 10},
		},
		{
			metricName: MetricName{
				Name:        "MessagesInPerSec",
				Group:       "BrokerTopicMetrics",
				Description: "초당 수신 메시지",
				Tags:        map[string]string{"topic": "orders"},
			},
			stat: &CumulativeSumStat{sum: 50000},
		},
		{
			metricName: MetricName{
				Name:        "BytesInPerSec",
				Group:       "BrokerTopicMetrics",
				Description: "초당 수신 바이트",
				Tags:        map[string]string{},
			},
			stat: &CumulativeSumStat{sum: 10240000},
		},
	}

	jmx.Init(metrics)

	fmt.Println("  MBean 이름 생성 규칙: prefix:type=group,tag1=val1,tag2=val2")
	fmt.Println()

	// MBean 이름 출력
	for _, m := range metrics {
		mbeanName := jmx.GetMBeanName(m.metricName)
		fmt.Printf("  메트릭: %s\n", m.metricName.String())
		fmt.Printf("  MBean:  %s\n", mbeanName)
		fmt.Printf("  속성:   %s\n\n", m.metricName.Name)
	}

	fmt.Println("  현재 등록된 MBeans:")
	jmx.PrintMBeans()
	fmt.Println()
}

func demo4RecordingLevel() {
	fmt.Println("--- 데모 4: RecordingLevel 기반 성능 최적화 ---")
	fmt.Println()

	registry := NewMetrics(nil, INFO) // 기본: INFO 레벨만 기록

	// INFO 레벨 센서 (항상 기록)
	infoSensor := registry.Sensor("requests", INFO)
	infoStat := &CumulativeSumStat{}
	registry.AddMetricToSensor(infoSensor, MetricName{
		Name: "total-requests", Group: "server",
	}, infoStat)

	// DEBUG 레벨 센서 (INFO 설정에서는 기록 안 됨)
	debugSensor := registry.Sensor("request-details", DEBUG)
	debugStat := &CumulativeSumStat{}
	registry.AddMetricToSensor(debugSensor, MetricName{
		Name: "detailed-time", Group: "server",
	}, debugStat)

	// 값 기록 시도
	now := time.Now().UnixMilli()
	for i := 0; i < 100; i++ {
		infoSensor.Record(1, now+int64(i), INFO)   // INFO → INFO: 기록됨
		debugSensor.Record(1, now+int64(i), INFO)   // DEBUG → INFO: 기록 안 됨
	}

	fmt.Printf("  설정 레벨: INFO\n")
	fmt.Printf("  INFO 센서 (total-requests):  %.0f (기록됨)\n", infoStat.Value(now))
	fmt.Printf("  DEBUG 센서 (detailed-time):   %.0f (기록 안 됨 - 성능 최적화)\n", debugStat.Value(now))
	fmt.Println()

	// DEBUG 레벨로 변경
	for i := 0; i < 100; i++ {
		infoSensor.Record(1, now+int64(i+100), DEBUG)   // INFO → DEBUG: 기록됨
		debugSensor.Record(1, now+int64(i+100), DEBUG)   // DEBUG → DEBUG: 기록됨
	}

	fmt.Printf("  설정 레벨을 DEBUG로 변경 후:\n")
	fmt.Printf("  INFO 센서 (total-requests):  %.0f (계속 기록됨)\n", infoStat.Value(now))
	fmt.Printf("  DEBUG 센서 (detailed-time):   %.0f (이제 기록됨)\n", debugStat.Value(now))
	fmt.Println()
	fmt.Println("  핵심: RecordingLevel로 프로덕션에서는 필수 메트릭만, 디버깅 시 상세 메트릭 활성화")
	fmt.Println()
}

func demo5StatTypes() {
	fmt.Println("--- 데모 5: 다양한 통계 타입 ---")
	fmt.Println()

	now := time.Now().UnixMilli()
	values := []float64{10, 25, 5, 50, 30, 15, 45, 20, 35, 40}

	// 각 통계 타입에 같은 값 시퀀스를 기록
	avg := &AvgStat{}
	max := &MaxStat{}
	min := &MinStat{}
	cumSum := &CumulativeSumStat{}

	for _, v := range values {
		avg.Record(v, now)
		max.Record(v, now)
		min.Record(v, now)
		cumSum.Record(v, now)
	}

	fmt.Printf("  입력 값: %v\n\n", values)
	fmt.Printf("  %-20s = %.2f\n", "Avg (평균)", avg.Value(now))
	fmt.Printf("  %-20s = %.2f\n", "Max (최대값)", max.Value(now))
	fmt.Printf("  %-20s = %.2f\n", "Min (최소값)", min.Value(now))
	fmt.Printf("  %-20s = %.2f\n", "CumulativeSum (누적합)", cumSum.Value(now))
	fmt.Println()

	fmt.Println("  Kafka에서의 활용:")
	fmt.Println("    Rate       → byte-rate, request-rate (초당 속도)")
	fmt.Println("    Avg        → request-latency-avg (평균 지연시간)")
	fmt.Println("    Max        → request-latency-max (최대 지연시간)")
	fmt.Println("    Min        → 최소 응답 시간")
	fmt.Println("    TokenBucket → controller-mutation-rate (토큰 버킷)")
	fmt.Println("    Percentiles → p99 지연시간 (히스토그램 기반)")
	fmt.Println()
}
