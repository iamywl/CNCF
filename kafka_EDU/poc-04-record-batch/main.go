package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"time"
)

// Kafka RecordBatch Encoding/Decoding PoC
//
// 실제 Kafka 소스 참조:
//   - DefaultRecord.java: 개별 레코드 인코딩 (varint, delta encoding)
//   - DefaultRecordBatch.java: 배치 헤더 (61B 오버헤드), CRC32C
//
// Kafka RecordBatch 구조 (magic v2):
//   BaseOffset(8B) + Length(4B) + PartitionLeaderEpoch(4B) + Magic(1B) +
//   CRC(4B) + Attributes(2B) + LastOffsetDelta(4B) + BaseTimestamp(8B) +
//   MaxTimestamp(8B) + ProducerId(8B) + ProducerEpoch(2B) + BaseSequence(4B) +
//   RecordsCount(4B) = 61 bytes
//
// 개별 Record 구조:
//   Length(varint) + Attributes(1B) + TimestampDelta(varlong) + OffsetDelta(varint) +
//   KeyLength(varint) + Key + ValueLength(varint) + Value +
//   HeadersCount(varint) + Headers

// --- Varint/Varlong 인코딩 (Protocol Buffers 스타일) ---
// Kafka의 ByteUtils.writeVarint/writeVarlong 구현

func encodeVarint(buf *bytes.Buffer, value int32) int {
	// ZigZag encoding: (value << 1) ^ (value >> 31)
	v := uint32((value << 1) ^ (value >> 31))
	n := 0
	for v >= 0x80 {
		buf.WriteByte(byte(v) | 0x80)
		v >>= 7
		n++
	}
	buf.WriteByte(byte(v))
	return n + 1
}

func encodeVarlong(buf *bytes.Buffer, value int64) int {
	// ZigZag encoding: (value << 1) ^ (value >> 63)
	v := uint64((value << 1) ^ (value >> 63))
	n := 0
	for v >= 0x80 {
		buf.WriteByte(byte(v) | 0x80)
		v >>= 7
		n++
	}
	buf.WriteByte(byte(v))
	return n + 1
}

func decodeVarint(data []byte, offset int) (int32, int) {
	var result uint32
	shift := uint(0)
	pos := offset
	for {
		b := data[pos]
		result |= uint32(b&0x7F) << shift
		pos++
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	// ZigZag decode: (result >> 1) ^ -(result & 1)
	return int32((result >> 1) ^ -(result & 1)), pos - offset
}

func decodeVarlong(data []byte, offset int) (int64, int) {
	var result uint64
	shift := uint(0)
	pos := offset
	for {
		b := data[pos]
		result |= uint64(b&0x7F) << shift
		pos++
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return int64((result >> 1) ^ -(result & 1)), pos - offset
}

func sizeOfVarint(value int32) int {
	v := uint32((value << 1) ^ (value >> 31))
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func sizeOfVarlong(value int64) int {
	v := uint64((value << 1) ^ (value >> 63))
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// --- Record (DefaultRecord.java) ---
// Record =>
//   Length => Varint
//   Attributes => Int8
//   TimestampDelta => Varlong
//   OffsetDelta => Varint
//   KeyLength => Varint
//   Key => Bytes
//   ValueLength => Varint
//   Value => Bytes
//   HeadersCount => Varint

type Record struct {
	TimestampDelta int64
	OffsetDelta    int32
	Key            []byte
	Value          []byte
	Headers        []RecordHeader
}

type RecordHeader struct {
	Key   string
	Value []byte
}

// encodeRecord는 DefaultRecord.writeTo()를 구현한다.
// 핵심: varint로 길이 인코딩, delta encoding으로 timestamp/offset 절약
func encodeRecord(rec *Record) []byte {
	// 먼저 body 크기 계산 (DefaultRecord.sizeOfBodyInBytes)
	bodySize := 1 // attributes (1 byte)
	bodySize += sizeOfVarlong(rec.TimestampDelta)
	bodySize += sizeOfVarint(rec.OffsetDelta)

	if rec.Key == nil {
		bodySize += sizeOfVarint(-1) // null key
	} else {
		bodySize += sizeOfVarint(int32(len(rec.Key))) + len(rec.Key)
	}

	if rec.Value == nil {
		bodySize += sizeOfVarint(-1) // null value
	} else {
		bodySize += sizeOfVarint(int32(len(rec.Value))) + len(rec.Value)
	}

	bodySize += sizeOfVarint(int32(len(rec.Headers)))
	for _, h := range rec.Headers {
		keyBytes := []byte(h.Key)
		bodySize += sizeOfVarint(int32(len(keyBytes))) + len(keyBytes)
		if h.Value == nil {
			bodySize += sizeOfVarint(-1)
		} else {
			bodySize += sizeOfVarint(int32(len(h.Value))) + len(h.Value)
		}
	}

	var buf bytes.Buffer

	// Length (varint) - body의 크기
	encodeVarint(&buf, int32(bodySize))

	// Attributes (1 byte) - 현재 사용하지 않음
	buf.WriteByte(0)

	// TimestampDelta (varlong) - baseTimestamp 대비 차이
	encodeVarlong(&buf, rec.TimestampDelta)

	// OffsetDelta (varint) - baseOffset 대비 차이
	encodeVarint(&buf, rec.OffsetDelta)

	// Key
	if rec.Key == nil {
		encodeVarint(&buf, -1)
	} else {
		encodeVarint(&buf, int32(len(rec.Key)))
		buf.Write(rec.Key)
	}

	// Value
	if rec.Value == nil {
		encodeVarint(&buf, -1)
	} else {
		encodeVarint(&buf, int32(len(rec.Value)))
		buf.Write(rec.Value)
	}

	// Headers
	encodeVarint(&buf, int32(len(rec.Headers)))
	for _, h := range rec.Headers {
		keyBytes := []byte(h.Key)
		encodeVarint(&buf, int32(len(keyBytes)))
		buf.Write(keyBytes)
		if h.Value == nil {
			encodeVarint(&buf, -1)
		} else {
			encodeVarint(&buf, int32(len(h.Value)))
			buf.Write(h.Value)
		}
	}

	return buf.Bytes()
}

// decodeRecord는 DefaultRecord.readFrom()을 구현한다.
func decodeRecord(data []byte, baseOffset int64, baseTimestamp int64) (*Record, int) {
	pos := 0

	// Length
	bodySize, n := decodeVarint(data[pos:], 0)
	pos += n

	// Attributes
	_ = data[pos] // attributes (unused)
	pos++

	// TimestampDelta
	tsDelta, n := decodeVarlong(data[pos:], 0)
	pos += n

	// OffsetDelta
	offDelta, n := decodeVarint(data[pos:], 0)
	pos += n

	// Key
	keyLen, n := decodeVarint(data[pos:], 0)
	pos += n
	var key []byte
	if keyLen >= 0 {
		key = make([]byte, keyLen)
		copy(key, data[pos:pos+int(keyLen)])
		pos += int(keyLen)
	}

	// Value
	valueLen, n := decodeVarint(data[pos:], 0)
	pos += n
	var value []byte
	if valueLen >= 0 {
		value = make([]byte, valueLen)
		copy(value, data[pos:pos+int(valueLen)])
		pos += int(valueLen)
	}

	// Headers
	headerCount, n := decodeVarint(data[pos:], 0)
	pos += n
	headers := make([]RecordHeader, headerCount)
	for i := 0; i < int(headerCount); i++ {
		hKeyLen, n := decodeVarint(data[pos:], 0)
		pos += n
		hKey := string(data[pos : pos+int(hKeyLen)])
		pos += int(hKeyLen)

		hValLen, n := decodeVarint(data[pos:], 0)
		pos += n
		var hVal []byte
		if hValLen >= 0 {
			hVal = make([]byte, hValLen)
			copy(hVal, data[pos:pos+int(hValLen)])
			pos += int(hValLen)
		}
		headers[i] = RecordHeader{Key: hKey, Value: hVal}
	}

	_ = bodySize // already consumed
	return &Record{
		TimestampDelta: tsDelta,
		OffsetDelta:    offDelta,
		Key:            key,
		Value:          value,
		Headers:        headers,
	}, pos
}

// --- RecordBatch (DefaultRecordBatch.java) ---
// 배치 헤더 레이아웃 (61 bytes):
//   BaseOffset(8) + Length(4) + PartitionLeaderEpoch(4) + Magic(1) +
//   CRC(4) + Attributes(2) + LastOffsetDelta(4) + BaseTimestamp(8) +
//   MaxTimestamp(8) + ProducerId(8) + ProducerEpoch(2) + BaseSequence(4) +
//   RecordsCount(4) = 61

const (
	batchOverhead = 61 // DefaultRecordBatch.RECORD_BATCH_OVERHEAD

	baseOffsetOff           = 0
	lengthOff               = 8
	partitionLeaderEpochOff = 12
	magicOff                = 16
	crcOff                  = 17
	attributesOff           = 21
	lastOffsetDeltaOff      = 23
	baseTimestampOff        = 27
	maxTimestampOff         = 35
	producerIdOff           = 43
	producerEpochOff        = 51
	baseSequenceOff         = 53
	recordsCountOff         = 57
	recordsOff              = 61
)

type RecordBatch struct {
	BaseOffset           int64
	PartitionLeaderEpoch int32
	Magic                int8
	Attributes           int16
	LastOffsetDelta      int32
	BaseTimestamp        int64
	MaxTimestamp         int64
	ProducerId           int64
	ProducerEpoch        int16
	BaseSequence         int32
	Records              []*Record
}

// Encode는 DefaultRecordBatch.writeHeader() + 각 Record 인코딩을 수행한다.
// CRC는 Attributes부터 배치 끝까지를 커버한다.
func (batch *RecordBatch) Encode() []byte {
	// 1) 레코드들 인코딩
	var recordsBuf bytes.Buffer
	for _, rec := range batch.Records {
		encoded := encodeRecord(rec)
		recordsBuf.Write(encoded)
	}
	recordsData := recordsBuf.Bytes()

	// 2) 전체 배치 크기 계산
	totalSize := batchOverhead + len(recordsData)
	buf := make([]byte, totalSize)

	// 3) 배치 헤더 기록 (DefaultRecordBatch.writeHeader 참조)
	// BaseOffset
	binary.BigEndian.PutUint64(buf[baseOffsetOff:], uint64(batch.BaseOffset))
	// Length (전체 크기 - BaseOffset(8) - Length(4) = totalSize - 12)
	// Kafka: buffer.putInt(position + LENGTH_OFFSET, sizeInBytes - LOG_OVERHEAD)
	// LOG_OVERHEAD = 12 (BaseOffset 8B + Length 4B)
	binary.BigEndian.PutUint32(buf[lengthOff:], uint32(totalSize-12))
	// PartitionLeaderEpoch
	binary.BigEndian.PutUint32(buf[partitionLeaderEpochOff:], uint32(batch.PartitionLeaderEpoch))
	// Magic
	buf[magicOff] = byte(batch.Magic)
	// Attributes
	binary.BigEndian.PutUint16(buf[attributesOff:], uint16(batch.Attributes))
	// LastOffsetDelta
	binary.BigEndian.PutUint32(buf[lastOffsetDeltaOff:], uint32(batch.LastOffsetDelta))
	// BaseTimestamp
	binary.BigEndian.PutUint64(buf[baseTimestampOff:], uint64(batch.BaseTimestamp))
	// MaxTimestamp
	binary.BigEndian.PutUint64(buf[maxTimestampOff:], uint64(batch.MaxTimestamp))
	// ProducerId
	binary.BigEndian.PutUint64(buf[producerIdOff:], uint64(batch.ProducerId))
	// ProducerEpoch
	binary.BigEndian.PutUint16(buf[producerEpochOff:], uint16(batch.ProducerEpoch))
	// BaseSequence
	binary.BigEndian.PutUint32(buf[baseSequenceOff:], uint32(batch.BaseSequence))
	// RecordsCount
	binary.BigEndian.PutUint32(buf[recordsCountOff:], uint32(len(batch.Records)))

	// 4) 레코드 데이터 복사
	copy(buf[recordsOff:], recordsData)

	// 5) CRC32C 계산 (Attributes부터 끝까지)
	// Kafka: Crc32C.compute(buffer, ATTRIBUTES_OFFSET, sizeInBytes - ATTRIBUTES_OFFSET)
	crcTable := crc32.MakeTable(crc32.Castagnoli) // CRC32C
	crcValue := crc32.Checksum(buf[attributesOff:], crcTable)
	binary.BigEndian.PutUint32(buf[crcOff:], crcValue)

	return buf
}

// Decode는 바이트 배열에서 RecordBatch를 디코딩한다.
func DecodeBatch(data []byte) (*RecordBatch, error) {
	if len(data) < batchOverhead {
		return nil, fmt.Errorf("data too short: %d < %d", len(data), batchOverhead)
	}

	batch := &RecordBatch{
		BaseOffset:           int64(binary.BigEndian.Uint64(data[baseOffsetOff:])),
		PartitionLeaderEpoch: int32(binary.BigEndian.Uint32(data[partitionLeaderEpochOff:])),
		Magic:                int8(data[magicOff]),
		Attributes:           int16(binary.BigEndian.Uint16(data[attributesOff:])),
		LastOffsetDelta:      int32(binary.BigEndian.Uint32(data[lastOffsetDeltaOff:])),
		BaseTimestamp:        int64(binary.BigEndian.Uint64(data[baseTimestampOff:])),
		MaxTimestamp:         int64(binary.BigEndian.Uint64(data[maxTimestampOff:])),
		ProducerId:           int64(binary.BigEndian.Uint64(data[producerIdOff:])),
		ProducerEpoch:        int16(binary.BigEndian.Uint16(data[producerEpochOff:])),
		BaseSequence:         int32(binary.BigEndian.Uint32(data[baseSequenceOff:])),
	}

	// CRC 검증
	storedCRC := binary.BigEndian.Uint32(data[crcOff:])
	crcTable := crc32.MakeTable(crc32.Castagnoli)
	computedCRC := crc32.Checksum(data[attributesOff:], crcTable)
	if storedCRC != computedCRC {
		return nil, fmt.Errorf("CRC mismatch: stored=0x%08X, computed=0x%08X", storedCRC, computedCRC)
	}

	// 레코드 디코딩
	recordCount := int(binary.BigEndian.Uint32(data[recordsCountOff:]))
	pos := recordsOff
	for i := 0; i < recordCount; i++ {
		rec, n := decodeRecord(data[pos:], batch.BaseOffset, batch.BaseTimestamp)
		batch.Records = append(batch.Records, rec)
		pos += n
	}

	return batch, nil
}

func main() {
	fmt.Println("========================================")
	fmt.Println(" Kafka RecordBatch Encoding/Decoding PoC")
	fmt.Println(" Based on: DefaultRecord.java, DefaultRecordBatch.java")
	fmt.Println("========================================")

	// --- 1. Varint 인코딩 데모 ---
	fmt.Println("\n[1] Varint 인코딩 (ZigZag + Variable-Length)")
	fmt.Println("    ByteUtils.writeVarint/writeVarlong 구현")
	fmt.Println()

	testValues := []int32{0, 1, -1, 127, 128, 300, -300, 10000, -10000}
	for _, v := range testValues {
		var buf bytes.Buffer
		n := encodeVarint(&buf, v)
		encoded := buf.Bytes()
		decoded, _ := decodeVarint(encoded, 0)
		fmt.Printf("  varint(%6d) -> %d bytes: %v -> decoded=%d (match=%v)\n",
			v, n, encoded, decoded, v == decoded)
	}

	// --- 2. Delta 인코딩 효과 ---
	fmt.Println("\n[2] Delta 인코딩 효과")
	fmt.Println("    절대값 대비 상대값의 크기 절약")
	fmt.Println()

	baseTimestamp := time.Now().UnixMilli()
	timestamps := []int64{baseTimestamp, baseTimestamp + 10, baseTimestamp + 25, baseTimestamp + 50, baseTimestamp + 100}

	totalAbsolute := 0
	totalDelta := 0
	fmt.Printf("  Base timestamp: %d\n", baseTimestamp)
	for i, ts := range timestamps {
		delta := ts - baseTimestamp
		absSize := sizeOfVarlong(ts)
		deltaSize := sizeOfVarlong(delta)
		totalAbsolute += absSize
		totalDelta += deltaSize
		fmt.Printf("  record[%d]: ts=%d, delta=%d, absSize=%dB, deltaSize=%dB (절약 %dB)\n",
			i, ts, delta, absSize, deltaSize, absSize-deltaSize)
	}
	fmt.Printf("  합계: 절대값=%dB, delta=%dB, 절약=%dB (%.0f%%)\n",
		totalAbsolute, totalDelta, totalAbsolute-totalDelta,
		float64(totalAbsolute-totalDelta)/float64(totalAbsolute)*100)

	// --- 3. 개별 Record 인코딩 ---
	fmt.Println("\n[3] 개별 Record 인코딩 (DefaultRecord.writeTo)")
	fmt.Println("    Record = Length(varint) + Attrs(1B) + TsDelta(varlong) +")
	fmt.Println("             OffDelta(varint) + Key + Value + Headers")
	fmt.Println()

	records := []*Record{
		{TimestampDelta: 0, OffsetDelta: 0, Key: []byte("key-0"), Value: []byte("hello kafka")},
		{TimestampDelta: 10, OffsetDelta: 1, Key: []byte("key-1"), Value: []byte("record batch encoding")},
		{TimestampDelta: 25, OffsetDelta: 2, Key: nil, Value: []byte("null key record"),
			Headers: []RecordHeader{{Key: "source", Value: []byte("poc")}}},
		{TimestampDelta: 50, OffsetDelta: 3, Key: []byte("key-3"), Value: []byte("with headers"),
			Headers: []RecordHeader{
				{Key: "trace-id", Value: []byte("abc-123")},
				{Key: "region", Value: []byte("ap-northeast-2")},
			}},
	}

	totalRecordBytes := 0
	for i, rec := range records {
		encoded := encodeRecord(rec)
		totalRecordBytes += len(encoded)
		keyStr := "nil"
		if rec.Key != nil {
			keyStr = string(rec.Key)
		}
		fmt.Printf("  record[%d]: tsDelta=%d, offDelta=%d, key=%q, value=%q, headers=%d -> %d bytes\n",
			i, rec.TimestampDelta, rec.OffsetDelta, keyStr, string(rec.Value), len(rec.Headers), len(encoded))

		// 라운드트립 확인
		decoded, _ := decodeRecord(encoded, 0, baseTimestamp)
		match := decoded.OffsetDelta == rec.OffsetDelta && decoded.TimestampDelta == rec.TimestampDelta
		fmt.Printf("           roundtrip: offDelta=%d, tsDelta=%d (match=%v)\n",
			decoded.OffsetDelta, decoded.TimestampDelta, match)
	}

	// --- 4. RecordBatch 인코딩 ---
	fmt.Println("\n[4] RecordBatch 인코딩 (DefaultRecordBatch.writeHeader)")
	fmt.Printf("    배치 헤더 오버헤드: %d bytes\n", batchOverhead)
	fmt.Println()

	batch := &RecordBatch{
		BaseOffset:           100,
		PartitionLeaderEpoch: 1,
		Magic:                2, // Kafka magic v2
		Attributes:           0, // no compression, CREATE_TIME
		LastOffsetDelta:      int32(len(records) - 1),
		BaseTimestamp:        baseTimestamp,
		MaxTimestamp:         baseTimestamp + 50,
		ProducerId:           12345,
		ProducerEpoch:        1,
		BaseSequence:         0,
		Records:              records,
	}

	encoded := batch.Encode()
	fmt.Printf("  배치 헤더 레이아웃 (61 bytes):\n")
	fmt.Printf("    BaseOffset(8B):           %d\n", batch.BaseOffset)
	fmt.Printf("    Length(4B):               %d\n", len(encoded)-12)
	fmt.Printf("    PartitionLeaderEpoch(4B): %d\n", batch.PartitionLeaderEpoch)
	fmt.Printf("    Magic(1B):                %d\n", batch.Magic)
	fmt.Printf("    CRC(4B):                  0x%08X\n", binary.BigEndian.Uint32(encoded[crcOff:]))
	fmt.Printf("    Attributes(2B):           0x%04X\n", batch.Attributes)
	fmt.Printf("    LastOffsetDelta(4B):       %d\n", batch.LastOffsetDelta)
	fmt.Printf("    BaseTimestamp(8B):         %d\n", batch.BaseTimestamp)
	fmt.Printf("    MaxTimestamp(8B):          %d\n", batch.MaxTimestamp)
	fmt.Printf("    ProducerId(8B):            %d\n", batch.ProducerId)
	fmt.Printf("    ProducerEpoch(2B):         %d\n", batch.ProducerEpoch)
	fmt.Printf("    BaseSequence(4B):          %d\n", batch.BaseSequence)
	fmt.Printf("    RecordsCount(4B):          %d\n", len(batch.Records))
	fmt.Printf("\n  전체 배치 크기: %d bytes (헤더 %dB + 레코드 %dB)\n",
		len(encoded), batchOverhead, totalRecordBytes)
	fmt.Printf("  오버헤드 비율: %.1f%%\n", float64(batchOverhead)/float64(len(encoded))*100)

	// --- 5. CRC 검증 + 디코딩 ---
	fmt.Println("\n[5] CRC32C 검증 + 디코딩")

	decoded, err := DecodeBatch(encoded)
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		return
	}
	fmt.Printf("  CRC 검증: PASS\n")
	fmt.Printf("  디코딩 결과: baseOffset=%d, recordCount=%d\n",
		decoded.BaseOffset, len(decoded.Records))
	for i, rec := range decoded.Records {
		keyStr := "nil"
		if rec.Key != nil {
			keyStr = string(rec.Key)
		}
		fmt.Printf("    record[%d]: offset=%d, ts=%d, key=%q, value=%q, headers=%d\n",
			i, decoded.BaseOffset+int64(rec.OffsetDelta),
			decoded.BaseTimestamp+rec.TimestampDelta,
			keyStr, string(rec.Value), len(rec.Headers))
	}

	// CRC 변조 테스트
	fmt.Println("\n  CRC 변조 테스트:")
	corrupted := make([]byte, len(encoded))
	copy(corrupted, encoded)
	corrupted[recordsOff+5] ^= 0xFF // 레코드 데이터 1바이트 변조
	_, err = DecodeBatch(corrupted)
	if err != nil {
		fmt.Printf("    변조 감지: %v\n", err)
	}

	// --- 6. 압축 시뮬레이션 ---
	fmt.Println("\n[6] 압축 효과 시뮬레이션")
	fmt.Println("    Kafka Attributes 비트 0-2: 압축 타입")
	fmt.Println("    0=NONE, 1=GZIP, 2=SNAPPY, 3=LZ4, 4=ZSTD")
	fmt.Println()

	// 유사한 레코드 100개 배치
	largeRecords := make([]*Record, 100)
	for i := 0; i < 100; i++ {
		largeRecords[i] = &Record{
			TimestampDelta: int64(i * 10),
			OffsetDelta:    int32(i),
			Key:            []byte(fmt.Sprintf("sensor-%03d", i%10)),
			Value:          []byte(fmt.Sprintf(`{"temperature":%.1f,"humidity":%d,"timestamp":%d}`, 22.5+float64(i%5)*0.1, 60+i%20, baseTimestamp+int64(i*10))),
		}
	}

	largeBatch := &RecordBatch{
		BaseOffset:      1000,
		Magic:           2,
		LastOffsetDelta: 99,
		BaseTimestamp:    baseTimestamp,
		MaxTimestamp:     baseTimestamp + 990,
		ProducerId:      -1,
		BaseSequence:    -1,
		Records:         largeRecords,
	}
	largeEncoded := largeBatch.Encode()

	// 단순 중복 제거 비율로 압축 효과 추정
	uniqueBytes := make(map[byte]bool)
	for _, b := range largeEncoded[batchOverhead:] {
		uniqueBytes[b] = true
	}
	entropy := float64(len(uniqueBytes)) / 256.0

	fmt.Printf("  100개 레코드 배치:\n")
	fmt.Printf("    원본 크기: %d bytes (헤더 %dB + 레코드 %dB)\n",
		len(largeEncoded), batchOverhead, len(largeEncoded)-batchOverhead)
	fmt.Printf("    레코드당 평균: %.1f bytes\n", float64(len(largeEncoded)-batchOverhead)/100.0)
	fmt.Printf("    바이트 엔트로피: %.2f (낮을수록 압축 효과 큼)\n", entropy)
	fmt.Printf("    예상 GZIP 압축률: ~%.0f%% (추정)\n", entropy*60+20)
	fmt.Printf("    예상 LZ4 압축률:  ~%.0f%% (추정)\n", entropy*70+15)

	// --- 7. 설계 요약 ---
	fmt.Println("\n[7] Kafka RecordBatch 설계 요약")
	fmt.Println("  +--------------------------------------------------+")
	fmt.Println("  | RecordBatch (61 bytes header overhead)            |")
	fmt.Println("  |--------------------------------------------------|")
	fmt.Println("  | BaseOffset(8) | Length(4) | LeaderEpoch(4)       |")
	fmt.Println("  | Magic(1) | CRC32C(4) | Attributes(2)            |")
	fmt.Println("  | LastOffsetDelta(4) | BaseTimestamp(8)            |")
	fmt.Println("  | MaxTimestamp(8) | ProducerId(8)                  |")
	fmt.Println("  | ProducerEpoch(2) | BaseSequence(4)              |")
	fmt.Println("  | RecordsCount(4)                                  |")
	fmt.Println("  |--------------------------------------------------|")
	fmt.Println("  | Record[0]: len|attr|tsDelta|offDelta|key|val|hdr |")
	fmt.Println("  | Record[1]: len|attr|tsDelta|offDelta|key|val|hdr |")
	fmt.Println("  | ...                                               |")
	fmt.Println("  +--------------------------------------------------+")
	fmt.Println()
	fmt.Println("  핵심 최적화:")
	fmt.Println("  1) Delta Encoding: timestamp/offset을 base 대비 차이로 저장 -> varint로 적은 바이트")
	fmt.Println("  2) Varint: 작은 값은 1바이트, 큰 값만 여러 바이트 (ZigZag for signed)")
	fmt.Println("  3) CRC32C: Attributes~끝을 커버, Castagnoli 다항식 (하드웨어 가속)")
	fmt.Println("  4) 배치 단위 압축: 개별 레코드가 아닌 배치 전체를 압축하여 효율 극대화")
}
