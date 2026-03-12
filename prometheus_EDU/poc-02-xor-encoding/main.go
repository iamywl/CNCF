// Prometheus XOR Encoding (Gorilla Compression) PoC
//
// Facebook Gorilla 논문(2015)에서 제안한 시계열 데이터 압축 알고리즘을
// Prometheus TSDB가 채택한 방식 그대로 구현한다.
//
// 핵심 원리:
//   - 타임스탬프: Delta-of-Delta 인코딩 (연속적인 간격의 변화만 저장)
//   - 값(float64): XOR 인코딩 (이전 값과의 XOR 결과에서 유효 비트만 저장)
//
// 참조: prometheus/tsdb/chunkenc/xor.go, bstream.go
//
// 실행: go run main.go

package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"strings"
)

// ============================================================================
// BitStream: 비트 단위 읽기/쓰기
// Prometheus bstream(tsdb/chunkenc/bstream.go) 구현을 재현한다.
// ============================================================================

type bit bool

const (
	zero bit = false
	one  bit = true
)

// bstream은 비트 단위로 쓸 수 있는 바이트 스트림이다.
// Prometheus의 bstream 구조체와 동일한 설계:
// - stream: 실제 데이터 바이트 슬라이스
// - count: 현재 마지막 바이트에서 아직 쓸 수 있는 비트 수 (오른쪽부터)
type bstream struct {
	stream []byte
	count  uint8 // 현재 바이트에서 남은 쓰기 가능 비트 수
}

// writeBit은 단일 비트를 스트림에 쓴다.
// Prometheus bstream.writeBit()과 동일한 로직이다.
func (b *bstream) writeBit(v bit) {
	if b.count == 0 {
		b.stream = append(b.stream, 0)
		b.count = 8
	}
	i := len(b.stream) - 1
	if v {
		b.stream[i] |= 1 << (b.count - 1)
	}
	b.count--
}

// writeByte는 1바이트를 스트림에 쓴다.
// 현재 바이트 경계에 맞춰 분할 기록한다.
func (b *bstream) writeByte(byt byte) {
	if b.count == 0 {
		b.stream = append(b.stream, byt)
		return
	}
	i := len(b.stream) - 1
	b.stream[i] |= byt >> (8 - b.count)
	b.stream = append(b.stream, byt<<b.count)
}

// writeBits는 u의 오른쪽 nbits 비트를 왼쪽에서 오른쪽 순서로 기록한다.
// Prometheus bstream.writeBits()와 동일한 구현이다.
func (b *bstream) writeBits(u uint64, nbits int) {
	u <<= 64 - uint(nbits)
	for nbits >= 8 {
		byt := byte(u >> 56)
		b.writeByte(byt)
		u <<= 8
		nbits -= 8
	}
	for nbits > 0 {
		b.writeBit((u >> 63) == 1)
		u <<= 1
		nbits--
	}
}

func (b *bstream) bytes() []byte {
	return b.stream
}

// bstreamReader는 비트 단위로 읽을 수 있는 리더이다.
// Prometheus bstreamReader를 단순화한 버전이다.
type bstreamReader struct {
	stream []byte
	pos    int   // 현재 바이트 위치
	valid  uint8 // buffer에서 읽을 수 있는 유효 비트 수
	buffer uint64
}

func newBReader(data []byte) bstreamReader {
	return bstreamReader{stream: data}
}

func (r *bstreamReader) loadBuffer() bool {
	if r.pos >= len(r.stream) {
		return false
	}
	remaining := len(r.stream) - r.pos
	if remaining >= 8 {
		r.buffer = binary.BigEndian.Uint64(r.stream[r.pos:])
		r.pos += 8
		r.valid = 64
	} else {
		r.buffer = 0
		for i := 0; i < remaining; i++ {
			r.buffer |= uint64(r.stream[r.pos+i]) << uint(8*(remaining-i-1))
		}
		r.pos += remaining
		r.valid = uint8(remaining * 8)
	}
	return true
}

func (r *bstreamReader) readBit() (bit, error) {
	if r.valid == 0 {
		if !r.loadBuffer() {
			return false, fmt.Errorf("EOF")
		}
	}
	r.valid--
	return (r.buffer>>r.valid)&1 != 0, nil
}

func (r *bstreamReader) readBits(nbits uint8) (uint64, error) {
	if r.valid == 0 {
		if !r.loadBuffer() {
			return 0, fmt.Errorf("EOF")
		}
	}
	if nbits <= r.valid {
		bitmask := (uint64(1) << nbits) - 1
		r.valid -= nbits
		return (r.buffer >> r.valid) & bitmask, nil
	}
	// 현재 버퍼의 남은 비트를 읽고, 다음 버퍼에서 나머지를 읽는다
	bitmask := (uint64(1) << r.valid) - 1
	remaining := nbits - r.valid
	v := (r.buffer & bitmask) << remaining
	r.valid = 0
	if !r.loadBuffer() {
		return 0, fmt.Errorf("EOF")
	}
	bitmask = (uint64(1) << remaining) - 1
	v |= (r.buffer >> (r.valid - remaining)) & bitmask
	r.valid -= remaining
	return v, nil
}

// ReadByte는 io.ByteReader 인터페이스를 구현한다.
// binary.ReadVarint/ReadUvarint에서 사용된다.
func (r *bstreamReader) ReadByte() (byte, error) {
	v, err := r.readBits(8)
	if err != nil {
		return 0, err
	}
	return byte(v), nil
}

// ============================================================================
// XORChunk: Gorilla 압축 알고리즘 구현
// Prometheus tsdb/chunkenc/xor.go의 핵심 로직을 재현한다.
// ============================================================================

const chunkHeaderSize = 2 // 샘플 개수를 저장하는 2바이트 헤더

// XORChunk은 XOR 인코딩된 시계열 데이터 청크이다.
type XORChunk struct {
	b bstream
}

// NewXORChunk은 새 XOR 청크를 생성한다.
func NewXORChunk() *XORChunk {
	b := make([]byte, chunkHeaderSize, 128)
	return &XORChunk{b: bstream{stream: b, count: 0}}
}

// NumSamples는 청크에 저장된 샘플 수를 반환한다.
func (c *XORChunk) NumSamples() int {
	return int(binary.BigEndian.Uint16(c.b.bytes()))
}

// Bytes는 청크의 원시 바이트를 반환한다.
func (c *XORChunk) Bytes() []byte {
	return c.b.bytes()
}

// xorAppender는 XOR 청크에 샘플을 추가하는 구조체이다.
type xorAppender struct {
	b        *bstream
	t        int64
	v        float64
	tDelta   uint64
	leading  uint8
	trailing uint8
}

// Appender는 청크에 데이터를 추가할 수 있는 appender를 반환한다.
func (c *XORChunk) Appender() *xorAppender {
	return &xorAppender{
		b:       &c.b,
		t:       math.MinInt64,
		leading: 0xff, // 초기값: 아직 leading/trailing이 설정되지 않았음을 표시
	}
}

// bitRange는 주어진 정수가 nbits로 표현 가능한지 판단한다.
// Prometheus bitRange() 함수와 동일하다.
// 범위: -(2^(nbits-1) - 1) <= x <= 2^(nbits-1)
func bitRange(x int64, nbits uint8) bool {
	return -((1 << (nbits - 1)) - 1) <= x && x <= 1<<(nbits-1)
}

// Append는 타임스탬프-값 쌍을 청크에 추가한다.
// Prometheus xorAppender.Append()의 핵심 로직을 그대로 재현한다.
//
// 인코딩 전략:
//   - 첫 번째 샘플: 타임스탬프 varint + 값 raw 64비트
//   - 두 번째 샘플: 타임스탬프 delta uvarint + 값 XOR
//   - 세 번째 이후: 타임스탬프 delta-of-delta 가변길이 + 값 XOR
func (a *xorAppender) Append(t int64, v float64) {
	var tDelta uint64
	num := binary.BigEndian.Uint16(a.b.bytes())

	switch num {
	case 0:
		// 첫 번째 샘플: 타임스탬프를 varint로, 값을 raw 64비트로 저장
		buf := make([]byte, binary.MaxVarintLen64)
		for _, b := range buf[:binary.PutVarint(buf, t)] {
			a.b.writeByte(b)
		}
		a.b.writeBits(math.Float64bits(v), 64)

	case 1:
		// 두 번째 샘플: 이전 타임스탬프와의 delta를 uvarint로 저장
		tDelta = uint64(t - a.t)
		buf := make([]byte, binary.MaxVarintLen64)
		for _, b := range buf[:binary.PutUvarint(buf, tDelta)] {
			a.b.writeByte(b)
		}
		a.writeVDelta(v)

	default:
		// 세 번째 이후: Delta-of-Delta 인코딩
		// Gorilla 논문의 핵심 아이디어: 대부분의 시계열은 일정한 간격으로
		// 수집되므로, 간격의 변화(dod)는 대부분 0이거나 매우 작다.
		tDelta = uint64(t - a.t)
		dod := int64(tDelta - a.tDelta)

		// Prometheus는 밀리초 해상도를 사용하므로(Gorilla는 초 단위)
		// 더 큰 비트 버킷을 사용한다: 14, 17, 20, 64비트
		switch {
		case dod == 0:
			// '0' 비트 하나로 표현 — 가장 흔한 경우
			a.b.writeBit(zero)
		case bitRange(dod, 14):
			// '10' + 14비트: 작은 변동
			a.b.writeBits(0b10, 2)
			a.b.writeBits(uint64(dod), 14)
		case bitRange(dod, 17):
			// '110' + 17비트: 중간 변동
			a.b.writeBits(0b110, 3)
			a.b.writeBits(uint64(dod), 17)
		case bitRange(dod, 20):
			// '1110' + 20비트: 큰 변동
			a.b.writeBits(0b1110, 4)
			a.b.writeBits(uint64(dod), 20)
		default:
			// '1111' + 64비트: 매우 큰 변동 (전체 저장)
			a.b.writeBits(0b1111, 4)
			a.b.writeBits(uint64(dod), 64)
		}
		a.writeVDelta(v)
	}

	a.t = t
	a.v = v
	binary.BigEndian.PutUint16(a.b.bytes(), num+1)
	a.tDelta = tDelta
}

// writeVDelta는 값의 XOR 인코딩을 수행한다.
func (a *xorAppender) writeVDelta(v float64) {
	xorWrite(a.b, v, a.v, &a.leading, &a.trailing)
}

// xorWrite는 Gorilla 논문의 값 압축 알고리즘을 구현한다.
// Prometheus xorWrite() 함수와 동일한 로직이다.
//
// 알고리즘:
//  1. XOR = 현재값 ^ 이전값
//  2. XOR == 0 → '0' (1비트): 값이 동일
//  3. XOR != 0 → '1' + ...
//     a. leading/trailing zeros가 이전과 같거나 더 많으면 → '10' + 유효비트만
//     b. 그렇지 않으면 → '11' + leading(5비트) + sigbits(6비트) + 유효비트
func xorWrite(b *bstream, newValue, currentValue float64, leading, trailing *uint8) {
	delta := math.Float64bits(newValue) ^ math.Float64bits(currentValue)

	if delta == 0 {
		b.writeBit(zero) // 값 동일: 1비트
		return
	}
	b.writeBit(one) // 값 변경됨

	newLeading := uint8(bits.LeadingZeros64(delta))
	newTrailing := uint8(bits.TrailingZeros64(delta))

	// Leading zeros를 32 미만으로 클램프 (5비트로 인코딩하므로)
	if newLeading >= 32 {
		newLeading = 31
	}

	if *leading != 0xff && newLeading >= *leading && newTrailing >= *trailing {
		// 이전 leading/trailing을 재사용할 수 있는 경우
		// '10' + 유효 비트만 기록 → 오버헤드 최소화
		b.writeBit(zero)
		b.writeBits(delta>>*trailing, 64-int(*leading)-int(*trailing))
		return
	}

	// 새로운 leading/trailing 정보를 기록해야 하는 경우
	*leading, *trailing = newLeading, newTrailing

	b.writeBit(one)
	b.writeBits(uint64(newLeading), 5)  // leading zeros (5비트)

	sigbits := 64 - newLeading - newTrailing
	b.writeBits(uint64(sigbits), 6)     // significant bits 수 (6비트)
	b.writeBits(delta>>newTrailing, int(sigbits)) // 유효 비트만 저장
}

// ============================================================================
// XOR Iterator: 디코딩
// Prometheus xorIterator의 Next() 로직을 재현한다.
// ============================================================================

// xorIterator는 XOR 청크에서 샘플을 순차적으로 읽는다.
type xorIterator struct {
	br       bstreamReader
	numTotal uint16
	numRead  uint16
	t        int64
	val      float64
	leading  uint8
	trailing uint8
	tDelta   uint64
	err      error
}

// NewIterator는 XOR 청크용 이터레이터를 생성한다.
func (c *XORChunk) NewIterator() *xorIterator {
	return &xorIterator{
		br:       newBReader(c.b.bytes()[chunkHeaderSize:]),
		numTotal: binary.BigEndian.Uint16(c.b.bytes()),
		t:        math.MinInt64,
	}
}

// Next는 다음 샘플을 읽고 성공 여부를 반환한다.
// Prometheus xorIterator.Next()와 동일한 디코딩 로직이다.
func (it *xorIterator) Next() bool {
	if it.err != nil || it.numRead == it.numTotal {
		return false
	}

	if it.numRead == 0 {
		// 첫 번째 샘플: varint 타임스탬프 + raw 64비트 값
		t, err := binary.ReadVarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		v, err := it.br.readBits(64)
		if err != nil {
			it.err = err
			return false
		}
		it.t = t
		it.val = math.Float64frombits(v)
		it.numRead++
		return true
	}

	if it.numRead == 1 {
		// 두 번째 샘플: uvarint delta + XOR 값
		tDelta, err := binary.ReadUvarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.tDelta = tDelta
		it.t += int64(it.tDelta)
		return it.readValue()
	}

	// 세 번째 이후: delta-of-delta 디코딩
	// 프리픽스 비트를 읽어서 버킷 크기를 결정한다
	var d byte
	for range 4 {
		d <<= 1
		b, err := it.br.readBit()
		if err != nil {
			it.err = err
			return false
		}
		if b == zero {
			break
		}
		d |= 1
	}

	var sz uint8
	var dod int64
	switch d {
	case 0b0:
		// dod == 0: 간격 변화 없음
	case 0b10:
		sz = 14
	case 0b110:
		sz = 17
	case 0b1110:
		sz = 20
	case 0b1111:
		v, err := it.br.readBits(64)
		if err != nil {
			it.err = err
			return false
		}
		dod = int64(v)
	}

	if sz != 0 {
		v, err := it.br.readBits(sz)
		if err != nil {
			it.err = err
			return false
		}
		// 음수 처리: 상위 비트가 설정된 경우 부호 확장
		if v > (1 << (sz - 1)) {
			v -= 1 << sz
		}
		dod = int64(v)
	}

	it.tDelta = uint64(int64(it.tDelta) + dod)
	it.t += int64(it.tDelta)
	return it.readValue()
}

func (it *xorIterator) readValue() bool {
	err := xorRead(&it.br, &it.val, &it.leading, &it.trailing)
	if err != nil {
		it.err = err
		return false
	}
	it.numRead++
	return true
}

// xorRead는 XOR 인코딩된 값을 디코딩한다.
// Prometheus xorRead() 함수와 동일한 로직이다.
func xorRead(br *bstreamReader, value *float64, leading, trailing *uint8) error {
	b, err := br.readBit()
	if err != nil {
		return err
	}
	if b == zero {
		// 값 동일: 아무것도 변경하지 않음
		return nil
	}

	b, err = br.readBit()
	if err != nil {
		return err
	}

	var newLeading, newTrailing, mbits uint8

	if b == zero {
		// '10': 이전 leading/trailing 재사용
		newLeading, newTrailing = *leading, *trailing
		mbits = 64 - newLeading - newTrailing
	} else {
		// '11': 새로운 leading/trailing 정보 포함
		v, err := br.readBits(5)
		if err != nil {
			return err
		}
		newLeading = uint8(v)

		v, err = br.readBits(6)
		if err != nil {
			return err
		}
		mbits = uint8(v)
		if mbits == 0 {
			mbits = 64 // 오버플로 보정 (xorWrite의 주석 참조)
		}
		newTrailing = 64 - newLeading - mbits
		*leading, *trailing = newLeading, newTrailing
	}

	v, err := br.readBits(mbits)
	if err != nil {
		return err
	}
	vbits := math.Float64bits(*value)
	vbits ^= v << newTrailing
	*value = math.Float64frombits(vbits)
	return nil
}

// At은 현재 샘플의 타임스탬프와 값을 반환한다.
func (it *xorIterator) At() (int64, float64) {
	return it.t, it.val
}

// Err는 이터레이션 중 발생한 에러를 반환한다.
func (it *xorIterator) Err() error {
	return it.err
}

// ============================================================================
// 데모 및 검증
// ============================================================================

// Sample은 타임스탬프-값 쌍이다.
type Sample struct {
	T int64
	V float64
}

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println(" Prometheus XOR Encoding (Gorilla Compression) PoC")
	fmt.Println(" 참조: tsdb/chunkenc/xor.go, bstream.go")
	fmt.Println("=" + strings.Repeat("=", 79))

	// ----------------------------------------------------------------
	// 시나리오 1: 일정 간격 카운터 (가장 높은 압축률)
	// 15초 간격으로 수집되는 CPU 사용률 메트릭 시뮬레이션
	// ----------------------------------------------------------------
	fmt.Println("\n[시나리오 1] 일정 간격 카운터 (15초 간격, 작은 값 변동)")
	fmt.Println(strings.Repeat("-", 60))

	samples1 := make([]Sample, 120)
	baseTime := int64(1700000000000) // 밀리초 타임스탬프
	baseValue := 42.0
	for i := range samples1 {
		samples1[i] = Sample{
			T: baseTime + int64(i)*15000, // 15초 간격
			V: baseValue + float64(i)*0.1, // 느리게 증가하는 카운터
		}
	}
	runScenario("일정 간격 카운터", samples1)

	// ----------------------------------------------------------------
	// 시나리오 2: 불규칙 간격, 급격한 값 변동
	// ----------------------------------------------------------------
	fmt.Println("\n[시나리오 2] 불규칙 간격, 급격한 값 변동")
	fmt.Println(strings.Repeat("-", 60))

	samples2 := make([]Sample, 120)
	for i := range samples2 {
		// 간격이 불규칙 (10~20초)
		jitter := int64(i%7) * 1000
		samples2[i] = Sample{
			T: baseTime + int64(i)*15000 + jitter,
			V: math.Sin(float64(i)*0.3) * 100.0, // 사인파 (큰 변동)
		}
	}
	runScenario("불규칙 간격 사인파", samples2)

	// ----------------------------------------------------------------
	// 시나리오 3: 값이 동일한 게이지 (최대 압축)
	// ----------------------------------------------------------------
	fmt.Println("\n[시나리오 3] 동일 값 반복 (최대 압축)")
	fmt.Println(strings.Repeat("-", 60))

	samples3 := make([]Sample, 120)
	for i := range samples3 {
		samples3[i] = Sample{
			T: baseTime + int64(i)*15000,
			V: 99.9, // 항상 같은 값
		}
	}
	runScenario("동일 값 반복", samples3)

	// ----------------------------------------------------------------
	// 비교 테이블
	// ----------------------------------------------------------------
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println(" 압축 비교 테이블")
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("%-25s %10s %10s %10s %12s\n",
		"시나리오", "샘플 수", "Raw(B)", "압축(B)", "비트/샘플")
	fmt.Println(strings.Repeat("-", 80))

	scenarios := []struct {
		name    string
		samples []Sample
	}{
		{"일정 간격 카운터", samples1},
		{"불규칙 사인파", samples2},
		{"동일 값 반복", samples3},
	}

	for _, s := range scenarios {
		rawBytes, compBytes, bitsPerSample := compress(s.samples)
		fmt.Printf("%-25s %10d %10d %10d %12.2f\n",
			s.name, len(s.samples), rawBytes, compBytes, bitsPerSample)
	}

	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("\n참고: Raw 크기 = 샘플당 %d바이트 (int64 타임스탬프 8B + float64 값 8B)\n", 16)
	fmt.Println("      Gorilla 논문 목표: 평균 1.37 bits/sample (실제 워크로드)")
	fmt.Println()

	// ----------------------------------------------------------------
	// 인코딩 상세 분석
	// ----------------------------------------------------------------
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println(" Delta-of-Delta 타임스탬프 인코딩 상세 분석")
	fmt.Println(strings.Repeat("=", 80))
	analyzeDoDDistribution(samples1, "일정 간격 카운터")
	analyzeDoDDistribution(samples2, "불규칙 사인파")
}

func runScenario(name string, samples []Sample) {
	// 인코딩
	chunk := NewXORChunk()
	app := chunk.Appender()
	for _, s := range samples {
		app.Append(s.T, s.V)
	}

	// 디코딩 & 검증
	it := chunk.NewIterator()
	decoded := make([]Sample, 0, len(samples))
	for it.Next() {
		t, v := it.At()
		decoded = append(decoded, Sample{T: t, V: v})
	}
	if it.Err() != nil {
		fmt.Printf("  ERROR: 디코딩 실패: %v\n", it.Err())
		return
	}

	// 검증
	if len(decoded) != len(samples) {
		fmt.Printf("  ERROR: 샘플 수 불일치 (원본=%d, 디코딩=%d)\n", len(samples), len(decoded))
		return
	}
	allMatch := true
	for i := range samples {
		if samples[i].T != decoded[i].T || samples[i].V != decoded[i].V {
			fmt.Printf("  ERROR: 샘플 %d 불일치\n", i)
			fmt.Printf("    원본:    t=%d, v=%f\n", samples[i].T, samples[i].V)
			fmt.Printf("    디코딩:  t=%d, v=%f\n", decoded[i].T, decoded[i].V)
			allMatch = false
			break
		}
	}

	rawBytes := len(samples) * 16
	compressedBytes := len(chunk.Bytes())
	ratio := float64(rawBytes) / float64(compressedBytes)
	bitsPerSample := float64(compressedBytes*8) / float64(len(samples))

	fmt.Printf("  검증: %s\n", func() string {
		if allMatch {
			return "OK (모든 샘플 일치)"
		}
		return "FAIL"
	}())
	fmt.Printf("  샘플 수:      %d\n", len(samples))
	fmt.Printf("  Raw 크기:     %d 바이트\n", rawBytes)
	fmt.Printf("  압축 크기:    %d 바이트\n", compressedBytes)
	fmt.Printf("  압축률:       %.2fx\n", ratio)
	fmt.Printf("  비트/샘플:    %.2f bits\n", bitsPerSample)

	// 처음 5개 샘플 출력
	fmt.Printf("\n  처음 5개 샘플 (디코딩 결과):\n")
	for i := 0; i < 5 && i < len(decoded); i++ {
		fmt.Printf("    [%d] t=%d, v=%.4f\n", i, decoded[i].T, decoded[i].V)
	}
}

func compress(samples []Sample) (rawBytes, compressedBytes int, bitsPerSample float64) {
	chunk := NewXORChunk()
	app := chunk.Appender()
	for _, s := range samples {
		app.Append(s.T, s.V)
	}
	rawBytes = len(samples) * 16
	compressedBytes = len(chunk.Bytes())
	bitsPerSample = float64(compressedBytes*8) / float64(len(samples))
	return
}

func analyzeDoDDistribution(samples []Sample, name string) {
	fmt.Printf("\n  [%s] DoD 분포:\n", name)

	buckets := map[string]int{
		"0 (1비트)":      0,
		"14비트 (16비트)": 0,
		"17비트 (20비트)": 0,
		"20비트 (24비트)": 0,
		"64비트 (68비트)": 0,
	}

	var prevT int64
	var prevDelta uint64
	for i, s := range samples {
		if i == 0 {
			prevT = s.T
			continue
		}
		delta := uint64(s.T - prevT)
		if i == 1 {
			prevDelta = delta
			prevT = s.T
			continue
		}
		dod := int64(delta - prevDelta)
		switch {
		case dod == 0:
			buckets["0 (1비트)"]++
		case bitRange(dod, 14):
			buckets["14비트 (16비트)"]++
		case bitRange(dod, 17):
			buckets["17비트 (20비트)"]++
		case bitRange(dod, 20):
			buckets["20비트 (24비트)"]++
		default:
			buckets["64비트 (68비트)"]++
		}
		prevDelta = delta
		prevT = s.T
	}

	order := []string{"0 (1비트)", "14비트 (16비트)", "17비트 (20비트)", "20비트 (24비트)", "64비트 (68비트)"}
	total := len(samples) - 2 // 첫 두 샘플은 DoD 사용 안 함
	for _, key := range order {
		count := buckets[key]
		pct := float64(count) / float64(total) * 100
		bar := strings.Repeat("#", int(pct/2))
		fmt.Printf("    %-20s: %4d (%5.1f%%) %s\n", key, count, pct, bar)
	}
}
