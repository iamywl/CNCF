# 25. Image Verification + Metrics/Monitoring Deep-Dive

> containerd 소스코드 기반 분석 문서 (P2 심화)
> 분석 대상: `plugins/imageverifier/`, `pkg/imageverifier/`, `core/metrics/`, `core/metrics/cgroups/`

---

## 1. 개요

### 1.1 Image Verification

컨테이너 이미지 검증은 공급 체인 보안(Supply Chain Security)의 핵심 요소다.
containerd의 Image Verification 플러그인은 이미지를 풀(pull)할 때 외부 검증기(verifier)를
실행하여 이미지의 서명, 정책 준수 여부 등을 확인한다.

```
이미지 풀 요청
    |
    v
containerd
    |
    +-- ImageVerifier 플러그인
    |     |
    |     +-- /opt/containerd/image-verifiers/cosign-verifier
    |     +-- /opt/containerd/image-verifiers/notation-verifier
    |     +-- /opt/containerd/image-verifiers/policy-checker
    |
    +-- 모든 verifier가 OK → 이미지 허용
    +-- 하나라도 REJECT → 이미지 거부
```

### 1.2 Metrics/Monitoring

containerd는 Prometheus 형식의 메트릭을 제공하여 컨테이너 리소스 사용량을 모니터링한다.
cgroups v1/v2 인터페이스를 통해 CPU, 메모리, I/O, PID 사용량을 수집하고,
OOM(Out of Memory) 이벤트도 추적한다.

```
컨테이너 리소스 모니터링 스택:

+------------------+     +-----------+     +-----------+
| containerd       | --> | Prometheus| --> | Grafana   |
| (메트릭 수집)    |     | (저장)    |     | (시각화)  |
+------------------+     +-----------+     +-----------+
       |
       v
  cgroups v1/v2
  (커널 인터페이스)
```

---

## 2. Image Verification 아키텍처

### 2.1 소스 구조

```
pkg/imageverifier/
├── image_verifier.go       # ImageVerifier 인터페이스, Judgement 구조체
└── bindir/
    ├── bindir.go           # 디렉토리 기반 검증기 구현
    ├── bindir_test.go
    ├── processes_unix.go   # Unix 프로세스 관리
    ├── processes_windows.go # Windows 프로세스 관리
    └── testdata/verifiers/ # 테스트용 검증기 바이너리

plugins/imageverifier/
├── plugin.go              # containerd 플러그인 등록
├── path_unix.go           # Unix 기본 경로
└── path_windows.go        # Windows 기본 경로
```

### 2.2 ImageVerifier 인터페이스

```go
// pkg/imageverifier/image_verifier.go
type ImageVerifier interface {
    VerifyImage(ctx context.Context, name string, desc ocispec.Descriptor) (*Judgement, error)
}

type Judgement struct {
    OK     bool    // 허용(true) / 거부(false)
    Reason string  // 판단 이유
}
```

**왜 이렇게 단순한 인터페이스인가?**

1. **단일 책임**: 검증기는 "허용/거부" 하나만 결정
2. **확장성**: 어떤 검증 로직이든 이 인터페이스를 구현하면 됨
3. **조합 가능**: 여러 검증기를 체인으로 연결 가능

### 2.3 BinDir 검증기

```go
// pkg/imageverifier/bindir/bindir.go
type Config struct {
    BinDir             string           // 검증기 바이너리 디렉토리
    MaxVerifiers       int              // 최대 검증기 수 (기본: 10)
    PerVerifierTimeout tomlext.Duration // 개별 타임아웃 (기본: 10초)
}

type ImageVerifier struct {
    config *Config
}
```

### 2.4 검증 흐름

```go
// bindir.go:58
func (v *ImageVerifier) VerifyImage(ctx context.Context, name string, desc ocispec.Descriptor) (*imageverifier.Judgement, error) {
    // 1. 디렉토리의 바이너리 목록 (이름순 정렬)
    entries, err := os.ReadDir(v.config.BinDir)

    // 2. 디렉토리 없음 → 자동 허용
    if errors.Is(err, os.ErrNotExist) {
        return &imageverifier.Judgement{OK: true, Reason: "directory does not exist"}, nil
    }

    // 3. 바이너리 없음 → 자동 허용
    if len(entries) == 0 {
        return &imageverifier.Judgement{OK: true, Reason: "no verifier binaries found"}, nil
    }

    // 4. 각 검증기 순차 실행
    for i, entry := range entries {
        // MaxVerifiers 초과 시 경고 후 중단
        if (i+1) > v.config.MaxVerifiers && v.config.MaxVerifiers >= 0 {
            break
        }

        exitCode, reason, err := v.runVerifier(ctx, entry.Name(), name, desc)
        if err != nil {
            return nil, err  // 실행 자체 실패
        }

        // exit code != 0 → 거부
        if exitCode != 0 {
            return &imageverifier.Judgement{
                OK:     false,
                Reason: fmt.Sprintf("verifier %v rejected (exit code %v): %v", bin, exitCode, reason),
            }, nil
        }
    }

    // 5. 모든 검증기 통과 → 허용
    return &imageverifier.Judgement{OK: true, Reason: reasons}, nil
}
```

**검증 결정 매트릭스:**

```
+--------------------+---------------------------+
| 조건               | 결과                      |
+--------------------+---------------------------+
| 디렉토리 미존재     | OK=true (open policy)    |
| 디렉토리 비어있음   | OK=true (no verifiers)   |
| 모든 exit code=0   | OK=true (all accepted)   |
| 하나라도 exit!=0   | OK=false (rejected)      |
| 실행 에러           | error (시스템 오류)       |
+--------------------+---------------------------+
```

### 2.5 검증기 바이너리 프로토콜

```go
// bindir.go:113
func (v *ImageVerifier) runVerifier(ctx context.Context, bin string, imageName string, desc ocispec.Descriptor) (exitCode int, reason string, err error) {
    ctx, cancel := context.WithTimeout(ctx, tomlext.ToStdTime(v.config.PerVerifierTimeout))
    defer cancel()

    binPath := filepath.Join(v.config.BinDir, bin)
    args := []string{
        "-name", imageName,                         // 이미지 이름
        "-digest", desc.Digest.String(),            // 이미지 다이제스트
        "-stdin-media-type", ocispec.MediaTypeDescriptor,  // stdin 데이터 타입
    }

    cmd := exec.CommandContext(ctx, binPath, args...)
    // stdin: OCI Descriptor (JSON)
    // stdout: 판단 이유 (text)
    // stderr: 디버그 로그
```

**검증기 바이너리 인터페이스:**

```
입력:
  CLI args: -name <이미지이름> -digest <다이제스트> -stdin-media-type <미디어타입>
  stdin: OCI Descriptor (JSON)

출력:
  exit code 0: 허용
  exit code != 0: 거부
  stdout: 판단 이유 (최대 32KB)
  stderr: 디버그 로그 (containerd 로그에 기록)
```

### 2.6 보안 설계

```go
// 출력 제한: 32KB
const outputLimitBytes = 1 << 15

// stdin에 Descriptor를 비동기 전송
go func() {
    err := json.NewEncoder(stdinWrite).Encode(desc)
    // ... (파이프 깨짐 허용)
    stdinWrite.Close()
}()

// stdout 제한 읽기
stdout, err := io.ReadAll(io.LimitReader(stdoutRead, outputLimitBytes))

// stderr 디버그 로깅 (제한)
lr := &io.LimitedReader{R: stderrRead, N: outputLimitBytes}
```

**보안 설계 원칙:**

1. **출력 제한**: 악의적 검증기가 무한 출력으로 메모리를 소진하는 것 방지
2. **타임아웃**: 무한 루프 검증기 방지
3. **MaxVerifiers**: 과도한 검증기 수로 인한 성능 저하 방지
4. **비동기 stdin**: 검증기가 stdin을 읽지 않아도 부모 프로세스가 블로킹되지 않음
5. **파이프 데드라인**: 타임아웃 시 파이프를 강제 닫아 행(hang) 방지

### 2.7 플러그인 등록

```go
// plugins/imageverifier/plugin.go
func init() {
    registry.Register(&plugin.Registration{
        Type:   plugins.ImageVerifierPlugin,
        ID:     "bindir",
        Config: defaultConfig(),
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            cfg := ic.Config.(*bindir.Config)
            return bindir.NewImageVerifier(cfg), nil
        },
    })
}

func defaultConfig() *bindir.Config {
    return &bindir.Config{
        BinDir:             defaultPath,          // /opt/containerd/image-verifiers
        MaxVerifiers:       10,
        PerVerifierTimeout: tomlext.FromStdTime(10 * time.Second),
    }
}
```

---

## 3. Metrics/Monitoring 아키텍처

### 3.1 소스 구조

```
core/metrics/
├── metrics.go                    # 빌드 정보 메트릭, 상수
├── types/
│   ├── v1/types.go              # cgroups v1 메트릭 타입
│   └── v2/types.go              # cgroups v2 메트릭 타입
└── cgroups/
    ├── cgroups.go               # TaskMonitor 플러그인 등록
    ├── common/type.go           # 공통 인터페이스
    ├── v1/
    │   ├── metrics.go           # v1 TaskMonitor
    │   ├── cgroups.go           # v1 Collector
    │   ├── cpu.go               # v1 CPU 메트릭
    │   ├── memory.go            # v1 메모리 메트릭
    │   ├── blkio.go             # v1 블록 I/O 메트릭
    │   ├── pids.go              # v1 PID 메트릭
    │   ├── hugetlb.go           # v1 HugeTLB 메트릭
    │   ├── oom.go               # v1 OOM 이벤트
    │   └── metric.go            # v1 메트릭 정의
    └── v2/
        ├── metrics.go           # v2 TaskMonitor + Collector
        ├── cgroups.go           # v2 Collector 초기화
        ├── cpu.go               # v2 CPU 메트릭
        ├── memory.go            # v2 메모리 메트릭
        ├── io.go                # v2 I/O 메트릭
        ├── pids.go              # v2 PID 메트릭
        └── metric.go            # v2 메트릭 정의
```

### 3.2 빌드 정보 메트릭

```go
// core/metrics/metrics.go
func init() {
    ns := goMetrics.NewNamespace("containerd", "", nil)
    c := ns.NewLabeledCounter("build_info", "containerd build information", "version", "revision")
    c.WithValues(version.Version, version.Revision).Inc()
    goMetrics.Register(ns)
    timeout.Set(ShimStatsRequestTimeout, 2*time.Second)
}
```

```
Prometheus 메트릭:
  containerd_build_info{version="2.0.0", revision="abc1234"} 1
```

### 3.3 TaskMonitor 플러그인

```go
// core/metrics/cgroups/cgroups.go
func New(ic *plugin.InitContext) (interface{}, error) {
    var ns *metrics.Namespace
    config := ic.Config.(*Config)
    if !config.NoPrometheus {
        ns = metrics.NewNamespace("container", "", nil)
    }

    ep, _ := ic.GetSingle(plugins.EventPlugin)

    // cgroups 버전에 따라 TaskMonitor 선택
    if cgroups.Mode() == cgroups.Unified {
        tm, err = v2.NewTaskMonitor(ic.Context, ep.(events.Publisher), ns)
    } else {
        tm, err = v1.NewTaskMonitor(ic.Context, ep.(events.Publisher), ns)
    }

    if ns != nil {
        metrics.Register(ns)
    }
    return tm, nil
}
```

**왜 v1/v2를 런타임에 선택하는가?**

```
Linux 커널 버전에 따른 cgroups 모드:

cgroups v1 (레거시):
  - 각 리소스 컨트롤러가 별도 파일시스템
  - /sys/fs/cgroup/cpu/, /sys/fs/cgroup/memory/ 등
  - 커널 4.x 이하

cgroups v2 (통합):
  - 단일 파일시스템 계층
  - /sys/fs/cgroup/ 하나로 통합
  - 커널 5.x 이상

containerd는 cgroups.Mode()로 현재 시스템의 모드를 감지하여
적절한 TaskMonitor를 생성한다.
```

### 3.4 Collector (v2)

```go
// core/metrics/cgroups/v2/metrics.go
type Collector struct {
    ns            *metrics.Namespace
    storedMetrics chan prometheus.Metric

    mu      sync.RWMutex
    tasks   map[string]entry
    metrics []*metric
}

func NewCollector(ns *metrics.Namespace) *Collector {
    c := &Collector{
        ns:    ns,
        tasks: make(map[string]entry),
    }
    c.metrics = append(c.metrics, pidMetrics...)
    c.metrics = append(c.metrics, cpuMetrics...)
    c.metrics = append(c.metrics, memoryMetrics...)
    c.metrics = append(c.metrics, ioMetrics...)
    c.storedMetrics = make(chan prometheus.Metric, 100*len(c.metrics))
    ns.Add(c)
    return c
}
```

### 3.5 메트릭 수집 흐름

```go
// v2/metrics.go:108
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
    c.mu.RLock()
    wg := &sync.WaitGroup{}
    for _, t := range c.tasks {
        wg.Add(1)
        go c.collect(t, ch, true, wg)  // 병렬 수집
    }
    // 저장된 메트릭 플러시
    for {
        select {
        case m := <-c.storedMetrics:
            ch <- m
        default:
            break
        }
    }
    c.mu.RUnlock()
    wg.Wait()
}
```

```go
// v2/metrics.go:129
func (c *Collector) collect(entry entry, ch chan<- prometheus.Metric, block bool, wg *sync.WaitGroup) {
    defer wg.Done()

    t := entry.task
    // 타임아웃 2초로 shim에서 통계 조회
    ctx, cancel := timeout.WithContext(context.Background(), cmetrics.ShimStatsRequestTimeout)
    stats, err := t.Stats(namespaces.WithNamespace(ctx, t.Namespace()))
    cancel()

    if err != nil {
        log.L.WithError(err).Errorf("stat task %s", t.ID())
        return
    }

    s := &v2.Metrics{}
    if err := typeurl.UnmarshalTo(stats, s); err != nil {
        log.L.WithError(err).Errorf("unmarshal stats for %s", t.ID())
        return
    }

    for _, m := range c.metrics {
        m.collect(t.ID(), t.Namespace(), s, ns, ch, block)
    }
}
```

**왜 병렬 수집하는가?**

수십~수백 개의 컨테이너가 있을 때, 각 컨테이너의 통계를 shim 프로세스에서 순차적으로
조회하면 타임아웃이 발생할 수 있다. 병렬 수집으로 전체 수집 시간을 단축한다.

### 3.6 CPU 메트릭 (v2)

```go
// core/metrics/cgroups/v2/cpu.go
var cpuMetrics = []*metric{
    {
        name: "cpu_usage_usec",
        help: "Total cpu usage (cgroup v2)",
        unit: metrics.Unit("microseconds"),
        vt:   prometheus.GaugeValue,
        getValues: func(stats *v2.Metrics) []value {
            if stats.CPU == nil { return nil }
            return []value{{v: float64(stats.CPU.UsageUsec)}}
        },
    },
    {
        name: "cpu_user_usec",
        help: "Current cpu usage in user space (cgroup v2)",
        // ...
    },
    {
        name: "cpu_system_usec",
        help: "Current cpu usage in kernel space (cgroup v2)",
        // ...
    },
    {
        name: "cpu_nr_periods",
        help: "Current cpu number of periods",
        // ...
    },
    {
        name: "cpu_nr_throttled",
        help: "Total number of times tasks have been throttled",
        // ...
    },
    {
        name: "cpu_throttled_usec",
        help: "Total time duration for which tasks have been throttled",
        // ...
    },
}
```

**CPU 메트릭 테이블:**

| 메트릭 | 단위 | 설명 |
|--------|------|------|
| `cpu_usage_usec` | 마이크로초 | 총 CPU 사용 시간 |
| `cpu_user_usec` | 마이크로초 | 사용자 공간 CPU 시간 |
| `cpu_system_usec` | 마이크로초 | 커널 공간 CPU 시간 |
| `cpu_nr_periods` | 횟수 | CPU 기간 수 |
| `cpu_nr_throttled` | 횟수 | CPU 쓰로틀링 횟수 |
| `cpu_throttled_usec` | 마이크로초 | CPU 쓰로틀링 총 시간 |

### 3.7 메모리 메트릭 (v2)

```go
// core/metrics/cgroups/v2/memory.go (일부)
var memoryMetrics = []*metric{
    {name: "memory_usage",            help: "Current memory usage",              unit: metrics.Bytes},
    {name: "memory_usage_limit",      help: "Current memory usage limit",        unit: metrics.Bytes},
    {name: "memory_swap_usage",       help: "Current swap usage",               unit: metrics.Bytes},
    {name: "memory_swap_limit",       help: "Current swap usage limit",          unit: metrics.Bytes},
    {name: "memory_file_mapped",      help: "The file_mapped amount",           unit: metrics.Bytes},
    {name: "memory_file_dirty",       help: "The file_dirty amount",            unit: metrics.Bytes},
    {name: "memory_pgfault",          help: "The pgfault amount",               unit: metrics.Bytes},
    {name: "memory_pgmajfault",       help: "The pgmajfault amount",            unit: metrics.Bytes},
    {name: "memory_inactive_anon",    help: "The inactive_anon amount",         unit: metrics.Bytes},
    {name: "memory_active_anon",      help: "The active_anon amount",           unit: metrics.Bytes},
    {name: "memory_inactive_file",    help: "The inactive_file amount",         unit: metrics.Bytes},
    {name: "memory_active_file",      help: "The active_file amount",           unit: metrics.Bytes},
    {name: "memory_kernel_stack",     help: "The kernel_stack amount",          unit: metrics.Bytes},
    {name: "memory_slab",             help: "The slab amount",                  unit: metrics.Bytes},
    {name: "memory_oom",              help: "Number of OOM events",             unit: metrics.Total},
    // ... 총 32개 메모리 메트릭
}
```

**주요 메모리 메트릭 카테고리:**

```
+---------------------------+
| 메모리 사용량              |
|  memory_usage             |
|  memory_usage_limit       |
+---------------------------+
| 스왑                       |
|  memory_swap_usage        |
|  memory_swap_limit        |
+---------------------------+
| 페이지 캐시                |
|  memory_file_mapped       |
|  memory_file_dirty        |
|  memory_file_writeback    |
+---------------------------+
| 페이지 폴트                |
|  memory_pgfault           |
|  memory_pgmajfault        |
+---------------------------+
| Anon/File 분류             |
|  memory_active_anon       |
|  memory_inactive_anon     |
|  memory_active_file       |
|  memory_inactive_file     |
+---------------------------+
| 커널 메모리                |
|  memory_kernel_stack      |
|  memory_slab              |
|  memory_slab_reclaimable  |
+---------------------------+
| OOM 이벤트                 |
|  memory_oom               |
+---------------------------+
```

### 3.8 Task 등록/제거

```go
// v2/metrics.go:159
func (c *Collector) Add(t common.Statable, labels map[string]string) error {
    if c.ns == nil { return nil }

    id := taskID(t.ID(), t.Namespace())
    // 이미 등록된 경우 무시 (멱등성)
    if _, ok := c.tasks[id]; ok {
        return nil
    }

    entry := entry{task: t}
    if labels != nil {
        entry.ns = c.ns.WithConstLabels(labels)
    }
    c.tasks[id] = entry
    return nil
}

func (c *Collector) Remove(t common.Statable) {
    delete(c.tasks, taskID(t.ID(), t.Namespace()))
}
```

---

## 4. Image Verification과 Metrics의 운영 시나리오

### 4.1 보안 모니터링

```
이미지 검증 실패 → 메트릭에 기록 → 알림 발생

Prometheus 쿼리:
  rate(containerd_image_verification_rejections_total[5m]) > 0

대시보드:
  - 검증 성공/실패 비율
  - 검증 소요 시간 분포
  - 검증기별 성능
```

### 4.2 리소스 모니터링

```
Grafana 대시보드 예시:

Row 1: CPU
  panel: container_cpu_usage_usec (rate)
  panel: container_cpu_throttled_usec (rate)

Row 2: Memory
  panel: container_memory_usage / container_memory_usage_limit
  panel: container_memory_oom (rate)

Row 3: I/O
  panel: container_io_read_bytes (rate)
  panel: container_io_write_bytes (rate)
```

---

## 5. 설정

### 5.1 Image Verifier 설정

```toml
[plugins."io.containerd.image-verifier.v1.bindir"]
  bin_dir = "/opt/containerd/image-verifiers"
  max_verifiers = 10
  per_verifier_timeout = "10s"
```

### 5.2 Metrics 설정

```toml
[plugins."io.containerd.monitor.v1.cgroups"]
  no_prometheus = false   # Prometheus 메트릭 활성화
```

### 5.3 Prometheus 스크레이핑

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'containerd'
    static_configs:
      - targets: ['localhost:10257']  # containerd 메트릭 포트
```

---

## 6. v1 vs v2 cgroups 메트릭 비교

### 6.1 CPU

| cgroups v1 | cgroups v2 | 설명 |
|-----------|-----------|------|
| cpuacct.usage | cpu.stat: usage_usec | 총 사용 시간 |
| cpuacct.usage_user | cpu.stat: user_usec | 사용자 공간 |
| cpuacct.usage_sys | cpu.stat: system_usec | 커널 공간 |
| cpu.stat: nr_periods | cpu.stat: nr_periods | 기간 수 |
| cpu.stat: nr_throttled | cpu.stat: nr_throttled | 쓰로틀링 횟수 |
| cpu.stat: throttled_time | cpu.stat: throttled_usec | 쓰로틀링 시간 |

### 6.2 메모리

| cgroups v1 | cgroups v2 | 설명 |
|-----------|-----------|------|
| memory.usage_in_bytes | memory.current | 현재 사용량 |
| memory.limit_in_bytes | memory.max | 제한 |
| memory.memsw.usage_in_bytes | memory.swap.current | 스왑 사용량 |
| memory.stat: pgfault | memory.stat: pgfault | 페이지 폴트 |
| memory.oom_control | memory.events: oom | OOM 이벤트 |

---

## 7. 정리

### 7.1 핵심 설계 원칙

| 원칙 | Image Verification | Metrics |
|------|-------------------|---------|
| 확장성 | 외부 바이너리 플러그인 | v1/v2 자동 선택 |
| 안전성 | 출력 제한, 타임아웃 | 타임아웃 기반 통계 조회 |
| 멱등성 | 검증기 결과 캐싱 | Add는 중복 등록 무시 |
| 관측 가능성 | Judgement.Reason | Prometheus 메트릭 |
| 성능 | MaxVerifiers 제한 | 병렬 통계 수집 |

### 7.2 관련 소스 파일 요약

| 파일 | 줄수 | 핵심 함수/타입 |
|------|------|---------------|
| `pkg/imageverifier/image_verifier.go` | 33줄 | `ImageVerifier`, `Judgement` |
| `pkg/imageverifier/bindir/bindir.go` | 260줄 | `VerifyImage`, `runVerifier` |
| `plugins/imageverifier/plugin.go` | 49줄 | 플러그인 등록, `defaultConfig` |
| `core/metrics/metrics.go` | 38줄 | 빌드 정보 메트릭, `ShimStatsRequestTimeout` |
| `core/metrics/cgroups/cgroups.go` | 99줄 | `New` (TaskMonitor 팩토리) |
| `core/metrics/cgroups/v2/metrics.go` | 199줄 | `Collector`, `Collect`, `Add`, `Remove` |
| `core/metrics/cgroups/v2/cpu.go` | 125줄 | `cpuMetrics` 정의 |
| `core/metrics/cgroups/v2/memory.go` | 606줄 | `memoryMetrics` 정의 (32개) |

### 7.3 PoC 참조

- `poc-22-image-verification/` -- 이미지 검증 파이프라인과 Judgement 패턴 시뮬레이션
- `poc-23-metrics/` -- cgroups 기반 컨테이너 메트릭 수집과 Prometheus 노출 시뮬레이션

---

*본 문서는 containerd 소스코드의 `plugins/imageverifier/`, `pkg/imageverifier/`, `core/metrics/`, `core/metrics/cgroups/` 디렉토리를 직접 분석하여 작성되었다.*
