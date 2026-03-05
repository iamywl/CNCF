# PoC-03: Hubble 링 버퍼

## 개요

Hubble의 핵심 저장소인 lock-free 순환 버퍼를 시뮬레이션한다. 이 링 버퍼는 atomic 연산과 사이클 카운터를 사용하여 동시성 안전한 이벤트 저장/조회를 구현한다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/hubble/container/ring.go` | Ring 구조체, NewRing(), Write(), read(), readFrom() |
| `pkg/hubble/container/ring_reader.go` | RingReader, Next(), Previous(), NextFollow() |
| `pkg/hubble/math/math.go` | MSB(), GetMask() 비트 연산 유틸리티 |

## 핵심 개념

### 1. 용량 설계: 2^n - 1

```
Capacity1   = 1     (2^1 - 1)
Capacity3   = 3     (2^2 - 1)
Capacity7   = 7     (2^3 - 1)
...
Capacity65535 = 65535 (2^16 - 1)
```

내부 배열 크기는 `용량 + 1`이며, 하나의 슬롯은 쓰기 예약용으로 사용된다. 이 설계 덕분에 `write & mask`로 나눗셈 없이 인덱스를 계산할 수 있다.

### 2. 사이클 기반 오버플로우 감지

```
write 위치는 절대 감소하지 않는 단조 증가 카운터
인덱스 = write & mask
사이클 = write >> cycleExp
```

read()에서 readCycle과 writeCycle을 비교하여:
- 같은 사이클에서 readIdx < lastWriteIdx → 유효한 읽기
- 이전 사이클에서 readIdx > lastWriteIdx → 유효 (wraparound)
- reader가 writer보다 앞 → EOF
- reader가 writer보다 뒤 → LostEvent (덮어씌워짐)

### 3. Writer-Reader 알림 메커니즘

sync.Cond 대신 `chan struct{}`를 사용한다. 이유: select문에서 사용 가능하므로 context 취소와 조합할 수 있다.

```go
// Writer
r.notifyMu.Lock()
if r.notifyCh != nil {
    close(r.notifyCh)  // 모든 대기 reader에게 알림
    r.notifyCh = nil
}
r.notifyMu.Unlock()

// Reader (follow 모드)
select {
case <-notifyCh:   // writer가 새 데이터 쓰면 깨어남
case <-ctx.Done(): // 또는 컨텍스트 취소
}
```

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

1. **2^n 마스크 연산**: 비트 AND로 인덱스 계산 (나눗셈 없음)
2. **사이클 카운터**: 단순 인덱스 비교가 아닌, 사이클까지 고려한 유효성 판별
3. **atomic 연산**: write 위치의 lock-free 증가
4. **채널 기반 알림**: sync.Cond 대신 close(chan)으로 broadcast 알림
5. **LostEvent**: 덮어쓰기 감지 시 유실 이벤트 생성
