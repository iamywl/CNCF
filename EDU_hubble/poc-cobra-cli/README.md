# PoC: Cobra CLI 패턴 (Hubble CLI 구조)

> **관련 문서**: [05-API-REFERENCE.md](../05-API-REFERENCE.md) - CLI 커맨드 상세, [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - Cobra/Viper 설정 통합 패턴

## 이 PoC가 보여주는 것

Hubble CLI의 **서브커맨드 계층 구조**를 시뮬레이션합니다.

```
mini-hubble
├── observe      [--follow] [--verdict X] [--source-pod X] [-o format]
├── status       [--server X]
├── config
│   └── view
├── list
│   └── nodes
└── version
```

## 실행 방법

```bash
cd EDU/poc-cobra-cli

# 도움말
go run main.go

# Flow 관찰 시뮬레이션
go run main.go observe --follow --verdict DROPPED

# 특정 Pod 필터
go run main.go observe --source-pod frontend

# 서버 상태
go run main.go status

# 설정 보기
go run main.go config view

# 노드 목록
go run main.go list nodes

# 버전
go run main.go version
```

## 핵심 학습 포인트

- **서브커맨드 구조**: `hubble` → `observe` → `flows` 형태의 트리 구조
- **플래그 파싱**: `--follow`, `--verdict DROPPED` 등 플래그를 각 커맨드가 독립적으로 처리
- **실제 Hubble**: Cobra 프레임워크가 자동으로 도움말, 자동완성, 에러 처리 제공
