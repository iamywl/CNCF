# PoC-02: XOR Encoding (Gorilla Compression)

## 개요

Facebook Gorilla 논문(2015, "Gorilla: A Fast, Scalable, In-Memory Time Series Database")에서 제안한 시계열 데이터 압축 알고리즘을 Prometheus TSDB가 채택한 방식 그대로 구현한 PoC이다.

Prometheus의 `tsdb/chunkenc/xor.go`와 `tsdb/chunkenc/bstream.go`의 핵심 로직을 표준 라이브러리만으로 재현한다.

## 핵심 아이디어

시계열 데이터는 두 가지 강한 패턴을 갖는다:

1. **타임스탬프**: 일정한 간격으로 수집된다 (예: 15초마다)
2. **값**: 연속된 샘플 간 변화가 작다 (또는 동일하다)

Gorilla 압축은 이 패턴을 활용하여 **샘플당 평균 1.37비트**(논문 기준)까지 압축한다.

## 압축 알고리즘

### 타임스탬프: Delta-of-Delta 인코딩

```
시간:    t0      t1      t2      t3      t4
값:     1000    1015    1030    1045    1060
delta:          15      15      15      15        ← 1차 차분
dod:                    0       0       0         ← 2차 차분 (대부분 0!)
```

일정 간격 수집 시 delta-of-delta(dod)는 대부분 0이므로, **1비트**만으로 표현 가능하다.

**인코딩 규칙** (Prometheus 구현):

| dod 범위 | 프리픽스 | 데이터 비트 | 총 비트 |
|----------|---------|------------|---------|
| dod = 0 | `0` | 0 | **1** |
| 14비트 표현 가능 | `10` | 14 | **16** |
| 17비트 표현 가능 | `110` | 17 | **20** |
| 20비트 표현 가능 | `1110` | 20 | **24** |
| 그 외 | `1111` | 64 | **68** |

> Gorilla 논문은 초 단위(7, 9, 12비트 버킷)이지만, Prometheus는 밀리초 해상도이므로 더 큰 버킷(14, 17, 20비트)을 사용한다.

### 값: XOR 인코딩

```
이전 값 (float64 bits):  0100000001001001 00001111 01000010...
현재 값 (float64 bits):  0100000001001001 00001111 01010011...
XOR 결과:                0000000000000000 00000000 00010001...
                         ^^^^^^^^^^^^^^^^ ^^^^^^^^   ^^^^
                         leading zeros              trailing zeros
                         → 유효 비트(significant bits)만 저장!
```

**인코딩 규칙**:

| 상황 | 프리픽스 | 추가 데이터 | 설명 |
|------|---------|-----------|------|
| XOR = 0 (값 동일) | `0` | 없음 | **1비트**만 사용 |
| leading/trailing 재사용 가능 | `10` | 유효 비트만 | 오버헤드 최소화 |
| 새로운 leading/trailing | `11` | 5비트(leading) + 6비트(sigbits) + 유효 비트 | 전체 정보 기록 |

**왜 XOR인가?**
- 유사한 float64 값의 XOR 결과는 대부분의 비트가 0이다
- Leading zeros와 trailing zeros를 제거하면 실제 저장해야 할 비트가 극소수
- 연속된 XOR 결과의 leading/trailing 패턴이 유사하므로 재사용 가능

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 실행 결과 예시

```
시나리오                    샘플 수     Raw(B)      압축(B)        비트/샘플
───────────────────────────────────────────────────────────────────────
일정 간격 카운터              120       1920        826        55.07
불규칙 사인파                 120       1920       1070        71.33
동일 값 반복                  120       1920         48         3.20
```

- **동일 값 반복**: 타임스탬프 dod=0(1비트) + 값 XOR=0(1비트) = 샘플당 ~2비트, 40x 압축
- **일정 간격 카운터**: 타임스탬프는 최적이지만, float64 값이 매번 변하므로 값 인코딩에 비트 소모
- **불규칙 사인파**: 타임스탬프 jitter + 큰 값 변동 → 가장 낮은 압축률

## 구현 구조

```
main.go
├── bstream          # 비트 단위 쓰기 스트림 (← tsdb/chunkenc/bstream.go)
│   ├── writeBit()   # 1비트 쓰기
│   ├── writeByte()  # 1바이트 쓰기 (바이트 경계 처리)
│   └── writeBits()  # N비트 쓰기
├── bstreamReader    # 비트 단위 읽기 스트림
│   ├── readBit()    # 1비트 읽기
│   ├── readBits()   # N비트 읽기
│   └── ReadByte()   # io.ByteReader 인터페이스 (varint 디코딩용)
├── XORChunk         # XOR 압축 청크 (← tsdb/chunkenc/xor.go)
│   ├── Appender()   # 인코딩: Append(t, v)
│   └── Iterator()   # 디코딩: Next() → At() → (t, v)
├── xorWrite()       # 값 XOR 인코딩
├── xorRead()        # 값 XOR 디코딩
└── main()           # 3가지 시나리오 데모 + 검증 + 비교 테이블
```

## Prometheus 실제 코드와의 대응

| PoC | Prometheus 소스 | 설명 |
|-----|----------------|------|
| `bstream` | `tsdb/chunkenc/bstream.go:bstream` | 비트 스트림 쓰기 |
| `bstreamReader` | `tsdb/chunkenc/bstream.go:bstreamReader` | 비트 스트림 읽기 |
| `XORChunk` | `tsdb/chunkenc/xor.go:XORChunk` | XOR 압축 청크 |
| `xorAppender.Append()` | `tsdb/chunkenc/xor.go:xorAppender.Append()` | Delta-of-Delta + XOR 인코딩 |
| `xorIterator.Next()` | `tsdb/chunkenc/xor.go:xorIterator.Next()` | 디코딩 이터레이터 |
| `xorWrite()` | `tsdb/chunkenc/xor.go:xorWrite()` | 값 XOR 인코딩 핵심 함수 |
| `xorRead()` | `tsdb/chunkenc/xor.go:xorRead()` | 값 XOR 디코딩 핵심 함수 |
| `bitRange()` | `tsdb/chunkenc/xor.go:bitRange()` | DoD 비트 범위 판정 |

## 참고 자료

- [Gorilla 논문](http://www.vldb.org/pvldb/vol8/p1816-teller.pdf) - Facebook, VLDB 2015
- Prometheus TSDB `tsdb/chunkenc/xor.go` - Gorilla 압축 구현
- Prometheus TSDB `tsdb/chunkenc/bstream.go` - 비트 스트림 구현
