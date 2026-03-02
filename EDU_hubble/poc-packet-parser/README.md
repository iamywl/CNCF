# PoC: Hubble 바이너리 패킷 파싱 패턴

## 관련 문서
- [03-DATA-MODEL.md](../03-DATA-MODEL.md) - Flow 데이터 구조
- [04-SEQUENCE-DIAGRAMS.md](../04-SEQUENCE-DIAGRAMS.md) - Parser 디코딩 시퀀스
- [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - Parser 패키지 구현

## 개요

Hubble Parser는 eBPF가 수집한 raw 바이트 데이터를 구조화된 Flow로 변환합니다:
1. 첫 바이트로 메시지 타입 판별 (DROP/TRACE/DEBUG)
2. 메시지 타입에 따라 적절한 파서로 디스패치
3. L2(Ethernet) → L3(IPv4) → L4(TCP/UDP) 레이어별 순차 파싱

## 실행

```bash
go run main.go
```

## 시나리오

### 시나리오 1: TCP TRACE 이벤트
정상적인 TCP 패킷 트레이스 → L2/L3/L4 전체 파싱

### 시나리오 2: UDP DROP 이벤트
DNS(UDP:53) 패킷 드롭 → UDP 헤더까지 파싱

### 시나리오 3: DEBUG 메시지
별도의 Debug 파서로 디스패치

### 바이트 오프셋 시각화
각 프로토콜 레이어의 바이트 위치를 시각적으로 표시

## 핵심 학습 내용
- Big-Endian 바이트 순서 (네트워크 바이트 오더)
- `encoding/binary` 패키지로 바이트 ↔ 정수 변환
- 메시지 타입 기반 파서 디스패치 패턴
- IHL(IP Header Length) 필드를 이용한 가변 길이 헤더 처리
- 실제 Hubble: `gopacket.DecodingLayerParser`로 제로카피 파싱
