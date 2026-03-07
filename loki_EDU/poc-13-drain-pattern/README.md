# PoC #13: Drain 패턴 - Drain 알고리즘 기반 로그 패턴 자동 감지

## 개요

Loki의 패턴 감지 엔진(`pkg/pattern/drain/`)에서 사용하는 Drain 알고리즘을 시뮬레이션한다. Drain은 비정형 로그 메시지에서 반복되는 패턴을 자동으로 감지하여 변수 부분을 와일드카드(`<_>`)로 치환하는 온라인 로그 클러스터링 알고리즘이다.

## Drain 알고리즘 핵심 개념

### 접두사 트리 (Prefix Tree)

```
Root
├── [토큰수: 8]               ← 토큰 8개짜리 로그
│   ├── [토큰: "INFO"]
│   │   └── [토큰: "request"]
│   │       └── clusterIDs: [1]
│   └── [토큰: "ERROR"]
│       └── clusterIDs: [2]
├── [토큰수: 10]              ← 토큰 10개짜리 로그
│   └── ...
```

### 학습(Train) 흐름

```
입력 로그 → 토큰화 → 트리 탐색 → 유사도 매칭
                                      │
                              ┌───────┴───────┐
                              │               │
                        매칭 성공          매칭 실패
                        템플릿 갱신       새 클러스터 생성
                        (다른 토큰 → <_>) 트리에 추가
```

### 유사도 계산

```
클러스터:  INFO request from <_>   status <_>   duration <_>
입력:      INFO request from 10.0.0.1 status 200  duration 55ms
           ✓    ✓       ✓    (param)  ✓      (param) ✓     (param)

유사도 = 일치 토큰 수 / 전체 토큰 수 = 5/8 = 0.625
임계값(SimTh) = 0.3 → 매칭 성공
```

## 실행 방법

```bash
go run main.go
```

## 시뮬레이션 내용

1. **Drain 인스턴스 생성**: SimTh=0.3, MaxNodeDepth=8, MaxClusters=50
2. **로그 메시지 학습**: HTTP 요청, 에러, 사용자 활동, 메트릭 로그
3. **패턴 자동 감지**: 변수 부분이 `<_>`로 치환된 패턴 추출
4. **접두사 트리 시각화**: 트리 구조 출력
5. **유사도 계산 데모**: 다양한 입력에 대한 유사도 측정
6. **LRU 제거 시뮬레이션**: 최대 클러스터 수 초과 시 오래된 패턴 제거

## Loki 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/pattern/drain/drain.go` | Drain 구조체, Train(), treeSearch(), fastMatch() |
| `pkg/pattern/drain/log_cluster.go` | LogCluster 구조체, 패턴 문자열 변환 |
| `pkg/pattern/drain/line_tokenizer.go` | 토큰화 전략 (punctuation, logfmt, JSON) |
| `pkg/pattern/drain/limiter.go` | Eviction 비율 제한기 |

## 핵심 설정값 (Loki 기본값)

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `SimTh` | 0.3 | 유사도 임계값 (이 값 이상이면 매칭) |
| `LogClusterDepth` | 30 | 트리 탐색 깊이 (maxNodeDepth = 28) |
| `MaxChildren` | 15 | 노드당 최대 자식 수 |
| `MaxClusters` | 300 | LRU 캐시 최대 크기 |
| `ParamString` | `<_>` | 와일드카드 치환 문자열 |
| `MaxEvictionRatio` | 0.25 | 최대 Eviction 비율 |
