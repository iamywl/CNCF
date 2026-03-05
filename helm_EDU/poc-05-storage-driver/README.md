# PoC-05: Helm v4 스토리지 드라이버

## 개요

Helm v4의 릴리스 저장소 패턴(Driver 인터페이스, Memory/File 드라이버, Storage 래퍼)을 시뮬레이션합니다.

## 시뮬레이션하는 패턴

| 패턴 | 실제 소스 | 설명 |
|------|----------|------|
| Driver 인터페이스 | `pkg/storage/driver/driver.go` | Creator+Updator+Deletor+Queryor 합성 |
| Memory 드라이버 | `pkg/storage/driver/memory.go` | RWMutex, namespace→name→records 맵 |
| File 드라이버 | (Secrets/ConfigMaps 대체) | JSON 파일 기반 영속 저장 |
| Storage 래퍼 | `pkg/storage/storage.go` | 키 생성, MaxHistory, History/Last/Deployed |
| 레이블 쿼리 | `driver/labels.go` | name/owner/status 레이블 매칭 |

## 실행 방법

```bash
go run main.go
```

## 키 형식

```
sh.helm.release.v1.<릴리스명>.v<리비전>
예: sh.helm.release.v1.myapp.v3
```

## 드라이버 선택 (실제 Helm)

```
HELM_DRIVER 환경변수
  ├── "secret" (기본) → Kubernetes Secrets
  ├── "configmap"      → Kubernetes ConfigMaps
  ├── "memory"         → 인메모리 (테스트용)
  └── "sql"            → SQL 데이터베이스
```
