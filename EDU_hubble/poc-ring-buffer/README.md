# PoC: Hubble Ring Buffer (순환 버퍼)

> **관련 문서**: [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - Ring Buffer 패턴, [02-ARCHITECTURE.md](../02-ARCHITECTURE.md) - 데이터 흐름

## 이 PoC가 보여주는 것

Hubble Server의 **Ring Buffer** 동작 원리를 시각적으로 보여줍니다.

```
Write 1~8: [1][2][3][4][5][6][7][8]  ← 버퍼 가득 참
Write 9:   [9][2][3][4][5][6][7][8]  ← slot[0] 덮어씀
Write 10:  [9][10][3][4][5][6][7][8] ← slot[1] 덮어씀
Write 11:  [9][10][11][4][5][6][7][8]
Write 12:  [9][10][11][12][5][6][7][8]
```

## 실행 방법

```bash
cd EDU/poc-ring-buffer
go run main.go
```

## 관찰할 수 있는 것

1. **Power-of-2 용량**: 요청한 용량이 자동으로 2의 거듭제곱으로 올림
2. **비트 마스킹**: `index & mask` = `index % capacity` (더 빠름)
3. **덮어쓰기**: 버퍼가 가득 찬 후 새 데이터가 오래된 슬롯을 재사용
4. **ReadLast(N)**: 최근 N개만 조회 (`hubble observe --last N`)
5. **Stats**: `hubble status`의 num_flows/max_flows

## 핵심 학습 포인트

- **고정 메모리**: 아무리 많은 Flow가 와도 메모리 사용량 불변
- **GC 프리**: 슬롯 재사용으로 가비지 컬렉션 압력 없음
- **O(1)**: 쓰기/읽기 모두 상수 시간
