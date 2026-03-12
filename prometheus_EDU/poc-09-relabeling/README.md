# PoC-09: Relabeling Pipeline

## 개요

Prometheus의 **relabeling**은 서비스 디스커버리가 발견한 타겟 레이블이나 수집된 메트릭의 레이블을 변환하는 파이프라인이다. 설정 파일의 `relabel_configs`(스크랩 전)와 `metric_relabel_configs`(스크랩 후)에서 사용된다.

**원본 코드**: `prometheus/model/relabel/relabel.go`

## 핵심 개념

### 적용 시점

```
서비스 디스커버리 → [relabel_configs] → 스크랩 → [metric_relabel_configs] → TSDB 저장
```

| 시점 | 설정 | 용도 |
|------|------|------|
| 스크랩 전 | `relabel_configs` | 타겟 필터링, __meta_* 레이블 변환, __address__ 수정 |
| 스크랩 후 | `metric_relabel_configs` | 불필요한 메트릭 드롭, 레이블 이름 정규화 |

### 동작 원리

각 relabel 규칙은 다음 순서로 동작한다:

1. **SourceLabels** 값을 **Separator**(기본 `;`)로 연결하여 입력 문자열 생성
2. **Regex**(기본 `(.*)`)로 입력 문자열 매칭
3. **Action**에 따라 변환/필터링 수행

규칙들은 **순차 적용**되며, keep/drop이 false를 반환하면 해당 타겟/메트릭이 즉시 드롭된다.

## 9가지 Action 상세

### replace (기본 액션)

SourceLabels 값을 Regex로 매칭하고, 캡처 그룹을 이용해 TargetLabel에 Replacement 결과를 기록한다.

```yaml
# __meta_kubernetes_namespace의 값을 namespace 레이블로 복사
- source_labels: [__meta_kubernetes_namespace]
  target_label: namespace
  # action: replace (기본값)
  # regex: "(.*)" (기본값)
  # replacement: "$1" (기본값)
```

**동작**: `val = join(source_labels, separator)` → `regex.FindSubmatchIndex(val)` → `regex.ExpandString(replacement, val, indexes)` → `lb.Set(target, result)`

### keep

SourceLabels 값이 Regex에 매칭되지 않으면 타겟을 드롭한다.

```yaml
# production 네임스페이스만 스크랩
- source_labels: [__meta_kubernetes_namespace]
  regex: "production"
  action: keep
```

### drop

SourceLabels 값이 Regex에 매칭되면 타겟을 드롭한다. keep의 반대.

```yaml
# go_* 메트릭 제거 (metric_relabel_configs에서)
- source_labels: [__name__]
  regex: "go_.*"
  action: drop
```

### hashmod

SourceLabels 값의 MD5 해시 하위 8바이트를 Modulus로 나눈 나머지를 TargetLabel에 기록한다. 여러 Prometheus 인스턴스 간 타겟 샤딩에 사용.

```yaml
# 3개 인스턴스로 샤딩 (인스턴스 0만 스크랩)
- source_labels: [__address__]
  target_label: __tmp_hash
  modulus: 3
  action: hashmod
- source_labels: [__tmp_hash]
  regex: "0"
  action: keep
```

**해시 계산**: `md5(val)` → 하위 8바이트를 `uint64`로 → `% modulus`

### labelmap

모든 레이블 이름을 Regex로 매칭하고, 매칭된 레이블의 값을 Replacement로 치환한 새 이름으로 복사한다.

```yaml
# __meta_consul_tag_(.+) → $1 (태그를 일반 레이블로)
- regex: "__meta_consul_tag_(.+)"
  replacement: "$1"
  action: labelmap
```

### labeldrop

레이블 이름이 Regex에 매칭되면 해당 레이블을 삭제한다.

```yaml
# 모든 __ 접두사 레이블 제거
- regex: "__.*"
  action: labeldrop
```

### labelkeep

레이블 이름이 Regex에 매칭되지 않으면 해당 레이블을 삭제한다. labeldrop의 반대.

```yaml
# __name__, job, instance만 유지
- regex: "__name__|job|instance"
  action: labelkeep
```

### lowercase

SourceLabels 값을 소문자로 변환하여 TargetLabel에 기록한다.

```yaml
- source_labels: [METHOD]
  target_label: method
  action: lowercase
```

### uppercase

SourceLabels 값을 대문자로 변환하여 TargetLabel에 기록한다.

```yaml
- source_labels: [env]
  target_label: env
  action: uppercase
```

## PoC 구현 내용

### 데이터 구조

| 구조체 | 원본 대응 | 설명 |
|--------|----------|------|
| `Label` | `labels.Label` | 이름-값 쌍 |
| `Labels` | `labels.Labels` | 정렬된 레이블 슬라이스 |
| `Builder` | `labels.Builder` | Set/Del로 레이블 변형, Labels()로 최종 생성 |
| `RelabelConfig` | `relabel.Config` | SourceLabels, Separator, Regex, TargetLabel, Replacement, Action, Modulus |

### 핵심 함수

| 함수 | 원본 대응 | 설명 |
|------|----------|------|
| `Process()` | `relabel.Process()` | 레이블 집합에 규칙 목록 순차 적용, nil 반환 시 드롭 |
| `applyRelabel()` | `relabel.relabel()` | 단일 규칙 적용, false 반환 시 드롭 |
| `anchoredRegex()` | `relabel.NewRegexp()` | `^(?s:pattern)$` 형태 앵커링 |

### 데모 시나리오 (6개)

| # | 시나리오 | 핵심 Action |
|---|---------|------------|
| 1 | 타겟 Relabeling | replace: `__meta_kubernetes_*` → 유용한 레이블 |
| 2 | keep/drop 필터링 | keep: 네임스페이스 필터, drop: 앱 이름 필터 |
| 3 | hashmod 샤딩 | hashmod: 타겟을 N개 인스턴스로 분배 |
| 4 | metric_relabel_configs | drop: 불필요 메트릭, lowercase + labeldrop: 레이블 정규화 |
| 5 | labelmap | labelmap: `__meta_consul_tag_*` → 클린 레이블 |
| 6 | 전체 파이프라인 | 발견 → relabel_configs → 스크랩 → metric_relabel_configs → 저장 |

## 실행

```bash
go run main.go
```

## 실무 활용 패턴

### 카디널리티 제어

```yaml
# 고카디널리티 레이블 제거로 스토리지 비용 절감
metric_relabel_configs:
  - regex: "(request_id|trace_id|span_id)"
    action: labeldrop
```

### Kubernetes 표준 relabeling

```yaml
relabel_configs:
  # annotation으로 스크랩 여부 결정
  - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
    regex: "true"
    action: keep
  # annotation에서 메트릭 경로 추출
  - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_path]
    target_label: __metrics_path__
  # Pod 레이블을 Prometheus 레이블로 매핑
  - regex: "__meta_kubernetes_pod_label_(.+)"
    replacement: "$1"
    action: labelmap
```

### 수평 확장 (Sharding)

```yaml
# N개 Prometheus 인스턴스 중 자기 shard만 스크랩
relabel_configs:
  - source_labels: [__address__]
    modulus: 3           # 전체 shard 수
    target_label: __tmp_hash
    action: hashmod
  - source_labels: [__tmp_hash]
    regex: "0"           # 이 인스턴스의 shard 번호
    action: keep
```
