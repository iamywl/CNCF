# PoC-17: 이미지 언팩(Unpacking) Deep-Dive

## 개요
containerd의 이미지 언팩 시스템 핵심 알고리즘을 시뮬레이션한다.

## 시뮬레이션 대상
- ChainID 계산 알고리즘 (OCI 표준)
- topHalf/bottomHalf 분리 패턴
- Fetch-Unpack 파이프라인 병렬화
- KeyedLocker 기반 중복 억제
- Limiter 기반 동시성 제어

## 소스 참조
- `core/unpack/unpacker.go` - Unpacker 구현체

## 실행
```bash
go run main.go
```

## 외부 의존성
없음 (Go 표준 라이브러리만 사용)
