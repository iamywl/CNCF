# PoC-18: TLS 유틸리티, Sympath, CopyStructure 시뮬레이션

## 개요

Helm의 TLS 유틸리티(`internal/tlsutil/`), Sympath(`internal/sympath/`), CopyStructure(`internal/copystructure/`)의 핵심 개념을 시뮬레이션한다.

## 시뮬레이션 항목

| 개념 | 소스 참조 | 시뮬레이션 방법 |
|------|----------|----------------|
| TLSConfigOption | `internal/tlsutil/tls.go` | 함수형 옵션 패턴 |
| 에러 수집 패턴 | `internal/tlsutil/tls.go` | 모든 옵션 에러를 수집 후 합산 |
| WithInsecureSkipVerify | `internal/tlsutil/tls.go` | 불리언 플래그 |
| WithCertKeyPairFiles | `internal/tlsutil/tls.go` | 파일 읽기 + PEM 블록 |
| WithCAFile | `internal/tlsutil/tls.go` | CA 인증서 파일 읽기 |
| Sympath Walk | `internal/sympath/walk.go` | 심볼릭 링크 따라가는 재귀 순회 |
| IsSymlink | `internal/sympath/walk.go` | ModeSymlink 비트 검사 |
| readDirNames | `internal/sympath/walk.go` | 정렬된 디렉토리 항목 |
| Lstat vs Stat | `internal/sympath/walk.go` | 심볼릭 링크 자체 vs 대상 |
| Copy (nil → map) | `internal/copystructure/copystructure.go` | nil 특별 처리 |
| copyValue | `internal/copystructure/copystructure.go` | Kind별 타입 스위치 딥 카피 |
| Map/Slice/Struct 카피 | `internal/copystructure/copystructure.go` | 리플렉션 기반 재귀 복사 |

## 실행

```bash
go run main.go
```

## 핵심 출력

- TLS 함수형 옵션 패턴 적용 및 에러 수집
- 빈 경로 no-op 처리, cert/key 불일치 에러
- Sympath Walk의 심볼릭 링크 감지 및 순회
- Lstat vs Stat 비교, 순환 심볼릭 링크 감지
- CopyStructure의 nil → 빈 map 변환 (Helm 특화)
- Map/Slice/Struct/Pointer 딥 카피 독립성 검증
- 타입별 딥 카피 전략 요약
