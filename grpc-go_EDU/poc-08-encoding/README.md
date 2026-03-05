# PoC-08: 코덱 레지스트리 & 압축

## 개념

gRPC의 인코딩(코덱)과 압축 시스템, 그리고 5바이트 메시지 프레이밍을 시뮬레이션한다.

```
메시지 인코딩 파이프라인:
  구조체 ──▶ Codec.Marshal() ──▶ Compressor.Compress() ──▶ 프레이밍 ──▶ 와이어
           (직렬화)            (압축)                  (5B 헤더)

메시지 디코딩 파이프라인:
  와이어 ──▶ 프레임 파싱 ──▶ Compressor.Decompress() ──▶ Codec.Unmarshal() ──▶ 구조체
           (5B 헤더)       (압축 해제)                (역직렬화)

5바이트 메시지 프레이밍:
┌──────────┬──────────────┬─────────────────┐
│ Flag (1B)│ Length (4B)   │ Message (N bytes)│
│ 0=비압축  │ big-endian    │                  │
│ 1=압축    │              │                  │
└──────────┴──────────────┴─────────────────┘
```

## 시뮬레이션하는 gRPC 구조

| 구조체/함수 | 실제 위치 | 역할 |
|------------|----------|------|
| `Codec` | `encoding/encoding.go:102` | Marshal/Unmarshal/Name |
| `Compressor` | `encoding/encoding.go:61` | Compress/Decompress/Name |
| `RegisterCodec` | `encoding/encoding.go` | 코덱 레지스트리 등록 |
| `GetCodec` | `encoding/encoding.go` | 코덱 레지스트리 조회 |
| `RegisterCompressor` | `encoding/encoding.go` | 압축기 레지스트리 등록 |
| `GetCompressor` | `encoding/encoding.go` | 압축기 레지스트리 조회 |
| 메시지 프레이밍 | `handler_server.go` | 5바이트 헤더 (flag + length) |

## 실행 방법

```bash
cd poc-08-encoding
go run main.go
```

## 예상 출력

```
=== 코덱 레지스트리 & 압축 시뮬레이션 ===

── 1. 레지스트리 등록 ──
[레지스트리] 코덱 등록: json
[레지스트리] 코덱 등록: proto-sim
[레지스트리] 압축기 등록: gzip
[레지스트리] 압축기 등록: identity

── 3. JSON 코덱 직렬화/역직렬화 ──
  원본: {Name:gRPC Age:10 Message:JSON 코덱 테스트입니다}
  직렬화: {"name":"gRPC","age":10,"message":"JSON 코덱 테스트입니다"} (XX bytes)
  역직렬화: {Name:gRPC Age:10 Message:JSON 코덱 테스트입니다}

── 6. 5바이트 메시지 프레이밍 ──
  비압축 프레임: [00 0000000a] + 10 bytes data = 총 15 bytes
  디코딩: compressed=false, data='Hello gRPC'
...

── 7. 엔드투엔드 파이프라인 ──
  [JSON + gzip]
  인코딩:
    직렬화 (json): XX bytes
    압축 (gzip): XX bytes
    프레이밍: [flag=1][len=XX] → 총 XX bytes
  디코딩:
    프레임 파싱: compressed=true, data=XX bytes
    압축 해제 (gzip): XX bytes
    역직렬화 (json): 성공
  결과: {Name:gRPC-Go Age:9 Message:엔드투엔드 테스트}

=== 시뮬레이션 완료 ===
```

## 핵심 포인트

1. **코덱 레지스트리**: `content-type` 헤더(예: `application/grpc+json`)로 코덱을 선택
2. **압축기 레지스트리**: `grpc-encoding` 헤더(예: `gzip`)로 압축기를 선택
3. **5바이트 프레이밍**: 모든 gRPC 메시지는 1바이트 플래그 + 4바이트 길이 헤더를 가짐
4. **파이프라인**: 직렬화 → 압축 → 프레이밍 → 전송 → 프레임 파싱 → 해제 → 역직렬화
5. **확장성**: Codec/Compressor 인터페이스를 구현하면 커스텀 직렬화/압축 가능
