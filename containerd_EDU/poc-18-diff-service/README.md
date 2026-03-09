# PoC-18: Diff Service Deep-Dive

## 개요
containerd의 Diff Service 핵심 개념을 시뮬레이션한다.

## 시뮬레이션 대상
- WalkingDiff (Compare) - 파일시스템 diff 계산
- fsApplier (Apply) - diff 적용
- StreamProcessor 체인 패턴
- 핸들러 역순 탐색
- 압축 포맷 비교

## 소스 참조
- `core/diff/diff.go` - Comparer/Applier 인터페이스
- `core/diff/stream.go` - StreamProcessor
- `plugins/diff/walking/differ.go` - WalkingDiff

## 실행
```bash
go run main.go
```

## 외부 의존성
없음 (Go 표준 라이브러리만 사용)
