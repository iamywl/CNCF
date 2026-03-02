// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Ring Buffer (순환 버퍼) 구현
//
// Hubble Server는 Ring Buffer에 최근 N개의 Flow를 저장합니다.
// 버퍼가 가득 차면 가장 오래된 Flow를 덮어씁니다.
//
// 왜 Ring Buffer인가?
//   - 고정 메모리: 크기가 미리 정해져 있어 메모리 예측 가능
//   - GC 프리: 사전 할당 슬롯 재사용으로 GC 압력 없음
//   - O(1) 연산: 읽기/쓰기 모두 상수 시간
//   - power-of-2: 비트 마스킹으로 모듈로 연산 대체 (성능 최적화)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"sync"
	"time"
)

// ========================================
// 1. Ring Buffer 구현
// ========================================

// Ring은 Hubble의 container.Ring을 시뮬레이션합니다.
//
// 핵심 설계 결정:
//   - capacity는 반드시 2의 거듭제곱 (power-of-2)
//   - 인덱스 계산: idx & (capacity - 1) = idx % capacity
//     → 비트 AND가 모듈로보다 빠름
//   - write는 항상 증가 (overflow 시 자동으로 오래된 데이터 덮어씀)
type Ring struct {
	mu       sync.RWMutex
	data     []Flow
	capacity uint64
	mask     uint64 // capacity - 1 (비트 마스킹용)
	write    uint64 // 다음 쓰기 위치 (무한 증가)
	len      uint64 // 현재 저장된 항목 수
}

type Flow struct {
	ID        uint64
	Timestamp time.Time
	Source    string
	Dest     string
	Verdict  string
}

// NewRing은 주어진 용량의 Ring Buffer를 생성합니다.
// 용량은 자동으로 2의 거듭제곱으로 올림됩니다.
//
// 왜 power-of-2인가?
//   capacity=8 → mask=7 (0b0111)
//   index & mask = index % capacity
//   비트 AND 연산이 모듈로 나눗셈보다 CPU에서 훨씬 빠름
func NewRing(requestedCapacity uint64) *Ring {
	// 2의 거듭제곱으로 올림
	capacity := nextPowerOf2(requestedCapacity)
	fmt.Printf("  [Ring] 요청 용량: %d → 실제 용량: %d (2^%d)\n",
		requestedCapacity, capacity, log2(capacity))

	return &Ring{
		data:     make([]Flow, capacity),
		capacity: capacity,
		mask:     capacity - 1,
	}
}

func nextPowerOf2(n uint64) uint64 {
	if n == 0 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	return n + 1
}

func log2(n uint64) int {
	count := 0
	for n > 1 {
		n >>= 1
		count++
	}
	return count
}

// Write는 Flow를 버퍼에 저장합니다.
// 버퍼가 가득 찬 경우 가장 오래된 항목을 덮어씁니다.
func (r *Ring) Write(flow Flow) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 비트 마스킹으로 인덱스 계산 (모듈로 대신)
	idx := r.write & r.mask
	overwritten := ""
	if r.len == r.capacity {
		overwritten = fmt.Sprintf(" (덮어씀: #%d)", r.data[idx].ID)
	}

	r.data[idx] = flow
	r.write++

	if r.len < r.capacity {
		r.len++
	}

	fmt.Printf("  [Write] Flow #%d → slot[%d]%s\n", flow.ID, idx, overwritten)
}

// ReadLast는 최근 N개의 Flow를 시간순으로 반환합니다.
// 'hubble observe --last N' 명령과 동일한 동작입니다.
func (r *Ring) ReadLast(n uint64) []Flow {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if n > r.len {
		n = r.len
	}

	result := make([]Flow, n)
	start := r.write - n

	for i := uint64(0); i < n; i++ {
		idx := (start + i) & r.mask
		result[i] = r.data[idx]
	}

	return result
}

// Stats는 현재 버퍼 상태를 반환합니다.
// 'hubble status'에서 보여주는 num_flows/max_flows에 해당합니다.
func (r *Ring) Stats() (current, max, totalWritten uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.len, r.capacity, r.write
}

// ========================================
// 2. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Ring Buffer ===")
	fmt.Println()
	fmt.Println("Ring Buffer는 고정 크기의 순환 버퍼입니다:")
	fmt.Println("  - 가득 차면 가장 오래된 데이터를 덮어씀 (FIFO)")
	fmt.Println("  - 메모리 사용량 = capacity * sizeof(Flow) (고정)")
	fmt.Println("  - power-of-2 용량으로 비트 마스킹 최적화")
	fmt.Println()

	// ── 버퍼 생성 (용량 8) ──
	fmt.Println("── 1단계: Ring Buffer 생성 (용량=8) ──")
	ring := NewRing(8)
	fmt.Println()

	// ── 8개 Flow 쓰기 (버퍼 가득 참) ──
	fmt.Println("── 2단계: 8개 Flow 쓰기 (버퍼 가득 참) ──")
	for i := 1; i <= 8; i++ {
		ring.Write(Flow{
			ID:        uint64(i),
			Timestamp: time.Now(),
			Source:    fmt.Sprintf("pod-%d", i),
			Dest:     "backend",
			Verdict:  "FORWARDED",
		})
	}

	cur, max, total := ring.Stats()
	fmt.Printf("\n  상태: %d/%d (총 쓰기: %d)\n", cur, max, total)
	fmt.Println()

	// ── 4개 더 쓰기 (오래된 것 덮어씀) ──
	fmt.Println("── 3단계: 4개 더 쓰기 (순환! 오래된 것 덮어씀) ──")
	for i := 9; i <= 12; i++ {
		ring.Write(Flow{
			ID:        uint64(i),
			Timestamp: time.Now(),
			Source:    fmt.Sprintf("pod-%d", i),
			Dest:     "backend",
			Verdict:  "DROPPED",
		})
	}

	cur, max, total = ring.Stats()
	fmt.Printf("\n  상태: %d/%d (총 쓰기: %d)\n", cur, max, total)
	fmt.Println()

	// ── 최근 5개 읽기 ──
	fmt.Println("── 4단계: 최근 5개 읽기 (hubble observe --last 5) ──")
	fmt.Println()
	last5 := ring.ReadLast(5)
	for _, f := range last5 {
		fmt.Printf("  Flow #%d: %s → %s [%s]\n", f.ID, f.Source, f.Dest, f.Verdict)
	}

	// ── 전체 읽기 ──
	fmt.Println()
	fmt.Println("── 5단계: 전체 읽기 (hubble observe --all) ──")
	fmt.Println()
	all := ring.ReadLast(max)
	for _, f := range all {
		fmt.Printf("  Flow #%d: %s → %s [%s]\n", f.ID, f.Source, f.Dest, f.Verdict)
	}
	fmt.Println()
	fmt.Println("  → Flow #1~#4는 덮어써져서 사라졌습니다!")

	// ── 비트 마스킹 설명 ──
	fmt.Println()
	fmt.Println("── 비트 마스킹 원리 ──")
	fmt.Println()
	fmt.Println("  capacity = 8  → 이진수: 1000")
	fmt.Println("  mask     = 7  → 이진수: 0111")
	fmt.Println()
	fmt.Println("  index & mask = index % capacity (비트 AND가 나눗셈보다 빠름)")
	fmt.Println()
	for i := 0; i < 12; i++ {
		idx := uint64(i) & 7
		fmt.Printf("  write=%2d → %2d & 0111 = slot[%d]\n", i, i, idx)
	}

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 고정 메모리: 8개 슬롯만 사용, 아무리 많이 써도 변하지 않음")
	fmt.Println("  - FIFO 덮어쓰기: 가장 오래된 데이터부터 사라짐")
	fmt.Println("  - 실제 Hubble: max_flows=4096~65536 (설정 가능)")
	fmt.Println("  - hubble status에서 num_flows/max_flows로 확인 가능")
}
