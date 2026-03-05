package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Hubble 익스포터 시뮬레이션
//
// 실제 구현 참조:
//   cilium/pkg/hubble/exporter/exporter.go  - exporter.Export()
//   cilium/pkg/hubble/exporter/encoder.go   - Encoder 인터페이스, JsonEncoder
//   cilium/pkg/hubble/exporter/writer.go    - FileWriter, 파일 로테이션
//   cilium/pkg/hubble/exporter/option.go    - Options, WithAllowList/DenyList/FieldMask
//
// 핵심 개념:
//   1. Export 파이프라인: 필터 → OnExportEvent 훅 → 인코딩 → 쓰기
//   2. Encoder 인터페이스: json.Encoder 기반, 확장 가능
//   3. 파일 로테이션: lumberjack 기반, MaxSize/MaxBackups 설정
//   4. 필드 마스킹: FieldMask로 특정 필드만 익스포트
// =============================================================================

// --- 데이터 모델 ---

type Verdict string

const (
	VerdictForwarded Verdict = "FORWARDED"
	VerdictDropped   Verdict = "DROPPED"
)

type Endpoint struct {
	Namespace string `json:"namespace"`
	PodName   string `json:"pod_name"`
	Labels    []string `json:"labels,omitempty"`
}

type Flow struct {
	Time        time.Time `json:"time"`
	NodeName    string    `json:"node_name"`
	Source      *Endpoint `json:"source"`
	Destination *Endpoint `json:"destination"`
	Verdict     Verdict   `json:"verdict"`
	Type        string    `json:"type"`
	Summary     string    `json:"summary"`
	L4Protocol  string    `json:"l4_protocol"`
	SrcPort     int       `json:"src_port"`
	DstPort     int       `json:"dst_port"`
}

type LostEvent struct {
	Source        string `json:"source"`
	NumEventsLost uint64 `json:"num_events_lost"`
}

// Event는 다양한 이벤트 타입의 래퍼
// 실제: v1.Event
type Event struct {
	Timestamp time.Time
	Event     interface{} // *Flow 또는 *LostEvent
}

// ExportEvent는 익스포트되는 이벤트 형식
// 실제: observerpb.ExportEvent
type ExportEvent struct {
	Time      string      `json:"time"`
	NodeName  string      `json:"node_name"`
	Flow      *Flow       `json:"flow,omitempty"`
	LostEvent *LostEvent  `json:"lost_events,omitempty"`
}

// --- Encoder 인터페이스 ---
// 실제: exporter.Encoder

type Encoder interface {
	Encode(v interface{}) error
}

// JsonEncoder는 JSON 인코더
// 실제: exporter.JsonEncoder
type jsonEncoder struct {
	enc *json.Encoder
}

func NewJsonEncoder(w io.Writer) Encoder {
	return &jsonEncoder{enc: json.NewEncoder(w)}
}

func (e *jsonEncoder) Encode(v interface{}) error {
	return e.enc.Encode(v)
}

// --- Writer with Rotation ---
// 실제: exporter.FileWriter (lumberjack 기반)

type RotatingWriter struct {
	mu         sync.Mutex
	filename   string
	maxSize    int64 // 바이트 단위
	maxBackups int
	file       *os.File
	size       int64
}

func NewRotatingWriter(filename string, maxSizeMB int, maxBackups int) (*RotatingWriter, error) {
	w := &RotatingWriter{
		filename:   filename,
		maxSize:    int64(maxSizeMB) * 1024 * 1024,
		maxBackups: maxBackups,
	}
	if err := w.openFile(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) openFile() error {
	f, err := os.OpenFile(w.filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 로테이션 필요 여부 확인
	if w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate는 현재 파일을 백업하고 새 파일 생성
// 실제: lumberjack.Logger의 rotate 로직
func (w *RotatingWriter) rotate() error {
	if w.file != nil {
		w.file.Close()
	}

	// 백업 파일 이름 생성
	ext := filepath.Ext(w.filename)
	base := w.filename[:len(w.filename)-len(ext)]
	backupName := fmt.Sprintf("%s-%s%s", base, time.Now().Format("20060102-150405"), ext)

	os.Rename(w.filename, backupName)
	fmt.Printf("  [로테이션] %s -> %s\n", w.filename, backupName)

	// 오래된 백업 정리
	w.cleanupOldBackups()

	return w.openFile()
}

func (w *RotatingWriter) cleanupOldBackups() {
	dir := filepath.Dir(w.filename)
	ext := filepath.Ext(w.filename)
	base := filepath.Base(w.filename[:len(w.filename)-len(ext)])

	entries, _ := os.ReadDir(dir)
	var backups []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, base+"-") && strings.HasSuffix(name, ext) {
			backups = append(backups, filepath.Join(dir, name))
		}
	}

	// maxBackups 초과분 삭제
	for len(backups) > w.maxBackups {
		os.Remove(backups[0])
		fmt.Printf("  [정리] 오래된 백업 삭제: %s\n", backups[0])
		backups = backups[1:]
	}
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// --- Filter 함수 ---
// 실제: filters.Apply()

type FilterFunc func(ev *Event) bool

// Apply는 화이트리스트/블랙리스트 필터 적용
// 실제: filters.Apply(whitelist, blacklist, event)
func ApplyFilters(allowList, denyList []FilterFunc, ev *Event) bool {
	// deny 리스트에 해당하면 거부
	for _, deny := range denyList {
		if deny(ev) {
			return false
		}
	}
	// allow 리스트가 비어있으면 전체 허용
	if len(allowList) == 0 {
		return true
	}
	// allow 리스트 중 하나라도 매치되면 허용
	for _, allow := range allowList {
		if allow(ev) {
			return true
		}
	}
	return false
}

// --- OnExportEvent 훅 ---
// 실제: exporter.OnExportEvent

type OnExportEvent interface {
	OnExportEvent(ctx context.Context, ev *Event, encoder Encoder) (stop bool, err error)
}

type OnExportEventFunc func(ctx context.Context, ev *Event, encoder Encoder) (bool, error)

func (f OnExportEventFunc) OnExportEvent(ctx context.Context, ev *Event, encoder Encoder) (bool, error) {
	return f(ctx, ev, encoder)
}

// --- FieldMask ---
// 실제: fieldmask.FieldMask

type FieldMask struct {
	fields map[string]bool
}

func NewFieldMask(fields []string) *FieldMask {
	if len(fields) == 0 {
		return nil
	}
	fm := &FieldMask{fields: make(map[string]bool)}
	for _, f := range fields {
		fm.fields[f] = true
	}
	return fm
}

func (fm *FieldMask) Active() bool {
	return fm != nil && len(fm.fields) > 0
}

// ApplyToFlow는 FieldMask에 따라 Flow를 필터링
func (fm *FieldMask) ApplyToFlow(f *Flow) *Flow {
	if !fm.Active() {
		return f
	}
	masked := &Flow{}
	if fm.fields["time"] {
		masked.Time = f.Time
	}
	if fm.fields["node_name"] {
		masked.NodeName = f.NodeName
	}
	if fm.fields["source"] {
		masked.Source = f.Source
	}
	if fm.fields["destination"] {
		masked.Destination = f.Destination
	}
	if fm.fields["verdict"] {
		masked.Verdict = f.Verdict
	}
	if fm.fields["type"] {
		masked.Type = f.Type
	}
	if fm.fields["summary"] {
		masked.Summary = f.Summary
	}
	return masked
}

// --- Exporter ---
// 실제: exporter.exporter

type ExporterOptions struct {
	AllowFilters   []FilterFunc
	DenyFilters    []FilterFunc
	OnExportEvents []OnExportEvent
	FieldMask      *FieldMask
}

type Exporter struct {
	encoder Encoder
	writer  io.WriteCloser
	opts    ExporterOptions
}

// NewExporter는 새 익스포터를 생성
// 실제: exporter.NewExporter()
func NewExporter(writer io.WriteCloser, opts ExporterOptions) *Exporter {
	encoder := NewJsonEncoder(writer)
	return &Exporter{
		encoder: encoder,
		writer:  writer,
		opts:    opts,
	}
}

// Export는 이벤트를 익스포트
// 실제: exporter.Export()
//
// 파이프라인:
// 1. context 확인
// 2. 필터 적용 (AllowList/DenyList)
// 3. OnExportEvent 훅 실행
// 4. Event → ExportEvent 변환
// 5. Encoder로 직렬화
func (e *Exporter) Export(ctx context.Context, ev *Event) error {
	// 1. context 확인
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// 2. 필터 적용
	// 실제: filters.Apply(e.opts.AllowFilters(), e.opts.DenyFilters(), ev)
	if !ApplyFilters(e.opts.AllowFilters, e.opts.DenyFilters, ev) {
		return nil
	}

	// 3. OnExportEvent 훅 실행
	for _, hook := range e.opts.OnExportEvents {
		stop, err := hook.OnExportEvent(ctx, ev, e.encoder)
		if err != nil {
			fmt.Printf("  [경고] OnExportEvent 실패: %v\n", err)
		}
		if stop {
			return nil // 훅이 중단 요청
		}
	}

	// 4. Event → ExportEvent 변환
	// 실제: exporter.eventToExportEvent()
	exportEvent := e.eventToExportEvent(ev)
	if exportEvent == nil {
		return nil
	}

	// 5. 인코딩 및 쓰기
	return e.encoder.Encode(exportEvent)
}

// eventToExportEvent는 내부 Event를 ExportEvent로 변환
// 실제: exporter.eventToExportEvent()
func (e *Exporter) eventToExportEvent(event *Event) *ExportEvent {
	switch ev := event.Event.(type) {
	case *Flow:
		// FieldMask 적용
		flow := ev
		if e.opts.FieldMask.Active() {
			flow = e.opts.FieldMask.ApplyToFlow(ev)
		}
		return &ExportEvent{
			Time:     event.Timestamp.Format(time.RFC3339Nano),
			NodeName: ev.NodeName,
			Flow:     flow,
		}
	case *LostEvent:
		return &ExportEvent{
			Time:      event.Timestamp.Format(time.RFC3339Nano),
			LostEvent: ev,
		}
	default:
		return nil
	}
}

// Stop은 익스포터를 정지
// 실제: exporter.Stop()
func (e *Exporter) Stop() error {
	if e.writer != nil {
		return e.writer.Close()
	}
	return nil
}

// --- 테스트 Flow 생성 ---

func generateFlows(n int) []*Event {
	events := make([]*Event, 0, n)
	namespaces := []string{"default", "kube-system", "monitoring", "prod"}
	pods := []string{"frontend", "backend", "api", "db", "cache"}
	verdicts := []Verdict{VerdictForwarded, VerdictForwarded, VerdictForwarded, VerdictDropped}

	for i := 0; i < n; i++ {
		srcNs := namespaces[rand.Intn(len(namespaces))]
		dstNs := namespaces[rand.Intn(len(namespaces))]
		flow := &Flow{
			Time:     time.Now().Add(time.Duration(i) * time.Millisecond),
			NodeName: fmt.Sprintf("node-%d", rand.Intn(3)),
			Source: &Endpoint{
				Namespace: srcNs,
				PodName:   fmt.Sprintf("%s-%d", pods[rand.Intn(len(pods))], rand.Intn(3)),
			},
			Destination: &Endpoint{
				Namespace: dstNs,
				PodName:   fmt.Sprintf("%s-%d", pods[rand.Intn(len(pods))], rand.Intn(3)),
			},
			Verdict:    verdicts[rand.Intn(len(verdicts))],
			Type:       "L3/L4",
			Summary:    fmt.Sprintf("TCP Flags: SYN; seq=%d", rand.Intn(99999)),
			L4Protocol: "TCP",
			SrcPort:    30000 + rand.Intn(35000),
			DstPort:    []int{80, 443, 8080, 3306}[rand.Intn(4)],
		}
		events = append(events, &Event{
			Timestamp: flow.Time,
			Event:     flow,
		})
	}

	// LostEvent 추가
	events = append(events, &Event{
		Timestamp: time.Now(),
		Event: &LostEvent{
			Source:        "HUBBLE_RING_BUFFER",
			NumEventsLost: 42,
		},
	})

	return events
}

func main() {
	fmt.Println("=== Hubble 익스포터 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: cilium/pkg/hubble/exporter/exporter.go - Export()")
	fmt.Println("참조: cilium/pkg/hubble/exporter/encoder.go  - Encoder, JsonEncoder")
	fmt.Println("참조: cilium/pkg/hubble/exporter/writer.go   - FileWriter, 파일 로테이션")
	fmt.Println("참조: cilium/pkg/hubble/exporter/option.go   - Options, FieldMask")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "hubble-exporter-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()

	// === 테스트 1: 기본 익스포트 (stdout) ===
	fmt.Println("--- 테스트 1: 기본 JSON 익스포트 (stdout, 3개) ---")
	fmt.Println()

	stdoutExporter := NewExporter(
		&noopCloser{os.Stdout}, // 실제: StdoutNoOpWriter
		ExporterOptions{},
	)

	events := generateFlows(3)
	for _, ev := range events[:3] {
		stdoutExporter.Export(ctx, ev)
	}
	fmt.Println()

	// === 테스트 2: 필터 적용 익스포트 ===
	fmt.Println("--- 테스트 2: 필터 적용 (kube-system 소스만 허용) ---")
	fmt.Println()

	filteredExporter := NewExporter(
		&noopCloser{os.Stdout},
		ExporterOptions{
			AllowFilters: []FilterFunc{
				func(ev *Event) bool {
					if f, ok := ev.Event.(*Flow); ok {
						return f.Source != nil && f.Source.Namespace == "kube-system"
					}
					return false
				},
			},
		},
	)

	allEvents := generateFlows(20)
	allowedCount := 0
	totalCount := 0
	for _, ev := range allEvents {
		totalCount++
		if f, ok := ev.Event.(*Flow); ok && f.Source != nil && f.Source.Namespace == "kube-system" {
			allowedCount++
		}
		filteredExporter.Export(ctx, ev)
	}
	fmt.Printf("\n  (총 %d개 중 kube-system 소스 %d개만 익스포트됨)\n\n", totalCount, allowedCount)

	// === 테스트 3: FieldMask 적용 ===
	fmt.Println("--- 테스트 3: FieldMask (time, source, verdict만 포함) ---")
	fmt.Println()

	maskedExporter := NewExporter(
		&noopCloser{os.Stdout},
		ExporterOptions{
			FieldMask: NewFieldMask([]string{"time", "source", "verdict"}),
		},
	)

	for _, ev := range generateFlows(3)[:3] {
		maskedExporter.Export(ctx, ev)
	}
	fmt.Println()

	// === 테스트 4: OnExportEvent 훅 ===
	fmt.Println("--- 테스트 4: OnExportEvent 훅 (DROP 이벤트 카운트) ---")
	fmt.Println()

	var dropCount int
	hookedExporter := NewExporter(
		&noopCloser{os.Stdout},
		ExporterOptions{
			OnExportEvents: []OnExportEvent{
				OnExportEventFunc(func(ctx context.Context, ev *Event, encoder Encoder) (bool, error) {
					if f, ok := ev.Event.(*Flow); ok && f.Verdict == VerdictDropped {
						dropCount++
						fmt.Printf("  [훅] DROPPED 이벤트 감지 #%d: %s -> %s\n",
							dropCount, f.Source.PodName, f.Destination.PodName)
					}
					return false, nil // 파이프라인 계속 진행
				}),
			},
		},
	)

	for _, ev := range generateFlows(10) {
		hookedExporter.Export(ctx, ev)
	}
	fmt.Printf("\n  (총 DROPPED 이벤트: %d개)\n\n", dropCount)

	// === 테스트 5: 파일 로테이션 ===
	fmt.Println("--- 테스트 5: 파일 로테이션 (512바이트 제한, 최대 2 백업) ---")
	fmt.Println()

	exportFile := filepath.Join(tmpDir, "hubble-flows.json")
	rotWriter, err := NewRotatingWriter(exportFile, 0, 2) // maxSize 직접 설정
	if err != nil {
		fmt.Printf("Writer 생성 실패: %v\n", err)
		return
	}
	// 작은 사이즈로 로테이션 테스트
	rotWriter.maxSize = 512

	fileExporter := NewExporter(rotWriter, ExporterOptions{})

	fmt.Printf("  파일: %s (최대 512바이트, 최대 2 백업)\n", exportFile)
	for _, ev := range generateFlows(30) {
		fileExporter.Export(ctx, ev)
	}
	fileExporter.Stop()

	// 생성된 파일 확인
	entries, _ := os.ReadDir(tmpDir)
	fmt.Printf("\n  생성된 파일들:\n")
	for _, e := range entries {
		info, _ := e.Info()
		fmt.Printf("    %s (%d 바이트)\n", e.Name(), info.Size())
	}
	fmt.Println()

	// === 테스트 6: DenyList 필터 ===
	fmt.Println("--- 테스트 6: DenyList (monitoring 네임스페이스 제외) ---")
	fmt.Println()

	denyExporter := NewExporter(
		&noopCloser{os.Stdout},
		ExporterOptions{
			DenyFilters: []FilterFunc{
				func(ev *Event) bool {
					if f, ok := ev.Event.(*Flow); ok {
						return (f.Source != nil && f.Source.Namespace == "monitoring") ||
							(f.Destination != nil && f.Destination.Namespace == "monitoring")
					}
					return false
				},
			},
		},
	)

	monitoringCount := 0
	for _, ev := range generateFlows(10) {
		if f, ok := ev.Event.(*Flow); ok {
			if (f.Source != nil && f.Source.Namespace == "monitoring") ||
				(f.Destination != nil && f.Destination.Namespace == "monitoring") {
				monitoringCount++
			}
		}
		denyExporter.Export(ctx, ev)
	}
	fmt.Printf("\n  (monitoring 네임스페이스 관련 %d개 이벤트 제외됨)\n\n", monitoringCount)

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Export 파이프라인: ctx확인 → 필터 → 훅 → 변환 → 인코딩")
	fmt.Println("  2. Encoder 인터페이스: json.Encoder 래핑, 다른 형식 확장 가능")
	fmt.Println("  3. 파일 로테이션: MaxSize 초과시 자동 백업 + 오래된 백업 정리")
	fmt.Println("  4. FieldMask: 필요한 필드만 선택적 익스포트")
	fmt.Println("  5. AllowList/DenyList: 네임스페이스, verdict 등으로 필터링")
	fmt.Println("  6. OnExportEvent: 익스포트 파이프라인에 커스텀 로직 삽입")
}

// noopCloser는 io.Writer를 io.WriteCloser로 래핑
// 실제: exporter.noopWriteCloser
type noopCloser struct {
	w io.Writer
}

func (nc *noopCloser) Write(p []byte) (int, error) { return nc.w.Write(p) }
func (nc *noopCloser) Close() error                { return nil }
