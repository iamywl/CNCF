# PoC #12: 블룸 필터 - 블룸 필터 기반 로그 검색 가속

## 개요

블룸 필터는 확률적 데이터 구조로, 원소의 존재 여부를 매우 효율적으로 판별한다. Loki는 블룸 필터를 사용하여 청크에 특정 검색어가 포함되어 있는지 빠르게 판별함으로써 불필요한 청크 읽기를 건너뛴다.

## 실제 Loki 코드와의 관계

| 이 PoC | Loki 실제 코드 |
|--------|---------------|
| `BloomFilter` | `pkg/storage/bloom/v1/bloom.go` |
| `ScalableBloomFilter` | `pkg/storage/bloom/v1/bloom.go` (Series bloom) |
| 청크 필터링 | `pkg/storage/bloom/v1/fuse.go` |
| Double Hashing | `pkg/storage/bloom/v1/bloom.go` (해시 함수) |

## 실행 방법

```bash
go run main.go
```

## 핵심 개념

### 블룸 필터 동작 원리
```
삽입 (Add):
  "error" → hash1(error)=3, hash2(error)=7, hash3(error)=15
  비트 배열: [...1...1.......1...]

조회 (Test):
  "error" → 위치 3,7,15 모두 1 → "아마도 있음" (True/False Positive)
  "debug" → 위치 2,7,10 중 2가 0 → "확실히 없음" (True Negative)
```

### 최적 파라미터 공식
```
n = 예상 원소 수
p = 목표 false positive 확률
m = -(n * ln(p)) / (ln(2))^2     (최적 비트 수)
k = (m/n) * ln(2)                 (최적 해시 함수 수)
```

### Loki 청크 필터링
```
100개 청크 → 블룸 필터로 30개로 축소 → 70% I/O 절약
```

## 시연 내용

1. **기본 동작**: 비트 배열 변화, 해시 위치, True/False Positive 확인
2. **파라미터와 FP**: 다양한 FP 목표에 따른 메모리/해시 수 변화
3. **Scalable Bloom Filter**: 원소 수 증가 시 자동 확장
4. **청크 필터링**: 5개 청크에서 검색어로 불필요한 청크 건너뛰기
5. **FP Rate 실측**: 이론값과 실측값 비교 (대규모 테스트)
6. **비트 배열 시각화**: 원소 추가에 따른 비트 채움 변화

## 핵심 특성

| 특성 | 설명 |
|------|------|
| False Positive | 가능 ("있다" → 실제로 없을 수 있음) |
| False Negative | 불가능 ("없다" → 반드시 없음) |
| 시간 복잡도 | O(k) (k = 해시 함수 수) |
| 공간 효율 | 원소 자체를 저장하지 않음 |
| 삭제 | 기본 블룸 필터에서는 불가 |
